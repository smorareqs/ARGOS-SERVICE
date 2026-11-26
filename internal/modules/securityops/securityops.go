// Package securityops es el tablero de seguridad y las alarmas de salida
// (fase04.md §3-5, Persona B).
//
//   - Al confirmar una reserva (o cambiar su salida) se programan dos alarmas
//     de salida: la previa (checkout_alarm_minutes de la empresa, 30 por
//     defecto) y la de la hora exacta. Se cancelan al cancelar la reserva o al
//     hacer el check-out.
//   - El trabajo security_alarms (cada minuto, por empresa) hace sonar las que
//     llegaron a su hora: aviso critico (push e inapp, no se apaga) a quien
//     tiene security.dashboard. Si nadie la marca atendida en 60 min levanta la
//     alerta checkout_alarm_unattended.
//   - El tablero del dia: llegadas, salidas, en casa, placas, areas reservadas
//     y alarmas pendientes.
package securityops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/modules/alerts"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/notifications"
	"github.com/smorareqs/argos-service/internal/modules/parking"
	"github.com/smorareqs/argos-service/internal/modules/realtime"
	"github.com/smorareqs/argos-service/internal/modules/reservations"
	"github.com/smorareqs/argos-service/internal/modules/sharedareas"
)

// JobAlarms hace sonar y escala las alarmas (cada minuto por empresa).
const JobAlarms = "security_alarms"

// UnattendedAfter es cuanto espera una alarma sonada antes de la alerta.
const UnattendedAfter = 60 * time.Minute

// Errores del modulo.
var (
	ErrAlarmNotFound = apperr.New(apperr.KindNotFound, "La alarma no existe.")
	ErrAlarmClosed   = apperr.New(apperr.KindValidation, "Esta alarma ya se atendió o se canceló.")
	ErrDate          = apperr.New(apperr.KindValidation, "La fecha no es válida.")
)

// AlarmRead es una alarma de seguridad.
type AlarmRead struct {
	ID              uuid.UUID  `db:"id" json:"id" format:"uuid"`
	ReservationID   uuid.UUID  `db:"reservation_id" json:"reservation_id" format:"uuid"`
	ReservationCode string     `db:"reservation_code" json:"reservation_code"`
	PropertyID      uuid.UUID  `db:"property_id" json:"property_id" format:"uuid"`
	PropertyCode    string     `db:"property_code" json:"property_code"`
	PropertyName    string     `db:"property_name" json:"property_name"`
	GuestName       string     `db:"guest_name" json:"guest_name"`
	Kind            string     `db:"kind" json:"kind" enum:"checkout,checkin"`
	Exact           bool       `db:"exact" json:"exact" doc:"true = la de la hora exacta; false = la previa."`
	FireAt          time.Time  `db:"fire_at" json:"fire_at"`
	Status          string     `db:"status" json:"status" enum:"scheduled,fired,attended,cancelled"`
	FiredAt         *time.Time `db:"fired_at" json:"fired_at" nullable:"true"`
	AttendedAt      *time.Time `db:"attended_at" json:"attended_at" nullable:"true"`
	AttendedByName  *string    `db:"attended_by_name" json:"attended_by_name" nullable:"true"`
	Notes           *string    `db:"notes" json:"notes" nullable:"true"`
	CheckOutTime    string     `db:"check_out_time" json:"check_out_time" doc:"HH:MM"`
	Plates          []string   `db:"plates" json:"plates" doc:"Placas con parqueo en la estancia."`
}

// AttendInput es el cuerpo de POST /security/alarms/{id}/attend.
type AttendInput struct {
	_     struct{} `additionalProperties:"true"`
	Notes *string  `json:"notes,omitempty" maxLength:"1000"`
}

// SecurityStay es una estancia en el tablero.
type SecurityStay struct {
	ReservationID uuid.UUID `db:"id" json:"reservation_id" format:"uuid"`
	Code          string    `db:"code" json:"code"`
	PropertyID    uuid.UUID `db:"property_id" json:"property_id" format:"uuid"`
	PropertyCode  string    `db:"property_code" json:"property_code"`
	PropertyName  string    `db:"property_name" json:"property_name"`
	Building      string    `db:"building" json:"building"`
	GuestName     string    `db:"guest_name" json:"guest_name"`
	Status        string    `db:"status" json:"status"`
	CheckInDate   string    `db:"check_in_date" json:"check_in_date" format:"date"`
	CheckOutDate  string    `db:"check_out_date" json:"check_out_date" format:"date"`
	CheckInTime   string    `db:"check_in_time" json:"check_in_time"`
	CheckOutTime  string    `db:"check_out_time" json:"check_out_time"`
	Adults        int       `db:"adults" json:"adults"`
	Children      int       `db:"children" json:"children"`
	ChannelName   string    `db:"channel_name" json:"channel_name"`
	Plates        []string  `db:"plates" json:"plates"`
}

// SecurityCounts son los totales del tablero.
type SecurityCounts struct {
	Arrivals      int `json:"arrivals"`
	Departures    int `json:"departures"`
	InHouse       int `json:"in_house"`
	Plates        int `json:"plates"`
	AreaBookings  int `json:"area_bookings"`
	AlarmsPending int `json:"alarms_pending"`
}

// SecurityDashboard es GET /security/dashboard.
type SecurityDashboard struct {
	Date           string                    `json:"date" format:"date"`
	SecurityCounts SecurityCounts            `json:"counts"`
	Arrivals       []SecurityStay            `json:"arrivals"`
	Departures     []SecurityStay            `json:"departures"`
	InHouse        []SecurityStay            `json:"in_house" doc:"Hospedados que no entran ni salen ese dia."`
	Plates         []parking.AssignmentRead  `json:"plates" doc:"Placas con espacio ese dia."`
	AreaBookings   []sharedareas.BookingRead `json:"area_bookings"`
	Alarms         []AlarmRead               `json:"alarms" doc:"Sonando o por sonar ese dia."`
}

// Service contiene los casos de uso de seguridad.
type Service struct {
	db     *database.DB
	notif  *notifications.Service
	alerts *alerts.Service
	log    *slog.Logger
	now    func() time.Time
}

// NewService crea el servicio.
func NewService(db *database.DB, notif *notifications.Service, al *alerts.Service, log *slog.Logger) *Service {
	return &Service{db: db, notif: notif, alerts: al, log: log, now: time.Now}
}

// SetClock cambia el reloj (pruebas).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

const tz = `(SELECT timezone FROM shared.tenants WHERE schema_name = current_schema())`

const alarmSelect = `
	SELECT a.id, a.reservation_id, r.code AS reservation_code, p.id AS property_id, p.code AS property_code, p.name AS property_name,
	       coalesce(g.full_name, r.external_summary, 'Huésped') AS guest_name, a.kind,
	       a.fire_at >= ((r.check_out_date + r.check_out_time) AT TIME ZONE ` + tz + `) AS exact,
	       a.fire_at, a.status, a.fired_at, a.attended_at, u.full_name AS attended_by_name, a.notes,
	       to_char(r.check_out_time, 'HH24:MI') AS check_out_time,
	       coalesce((SELECT array_agg(DISTINCT pa.vehicle_plate) FROM parking_assignments pa WHERE pa.reservation_id = r.id), '{}') AS plates
	FROM security_alarms a
	JOIN reservations r ON r.id = a.reservation_id
	JOIN properties p ON p.id = r.property_id
	LEFT JOIN guests g ON g.id = r.primary_guest_id
	LEFT JOIN shared.users u ON u.id = a.attended_by`

func alarmByID(ctx context.Context, q database.Querier, id uuid.UUID) (AlarmRead, error) {
	rows, err := q.Query(ctx, alarmSelect+` WHERE a.id = $1`, id)
	if err != nil {
		return AlarmRead{}, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[AlarmRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return AlarmRead{}, ErrAlarmNotFound
	}
	return a, err
}

// Alarms lista las alarmas: pending (sonando y las que faltan de hoy), today
// (todas las de hoy) o all (las ultimas 200).
func (s *Service) Alarms(ctx context.Context, status string) ([]AlarmRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]AlarmRead, error) {
		return alarmsList(ctx, q, status, s.now())
	})
}

func alarmsList(ctx context.Context, q database.Querier, status string, now time.Time) ([]AlarmRead, error) {
	where := ` WHERE (a.status = 'fired' OR (a.status = 'scheduled' AND (a.fire_at AT TIME ZONE ` + tz + `)::date = ($1::timestamptz AT TIME ZONE ` + tz + `)::date))
		ORDER BY a.status = 'fired' DESC, a.fire_at`
	switch status {
	case "today":
		where = ` WHERE a.status <> 'cancelled' AND (a.fire_at AT TIME ZONE ` + tz + `)::date = ($1::timestamptz AT TIME ZONE ` + tz + `)::date ORDER BY a.fire_at`
	case "all":
		where = ` WHERE $1::timestamptz IS NOT NULL ORDER BY a.fire_at DESC LIMIT 200`
	}
	rows, err := q.Query(ctx, alarmSelect+where, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[AlarmRead])
}

// Attend marca atendida la alarma (y la otra de la misma salida que siga
// pendiente) y resuelve su alerta.
func (s *Service) Attend(ctx context.Context, actor authz.Actor, id uuid.UUID, in AttendInput) (AlarmRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (AlarmRead, error) {
		cur, err := alarmByID(ctx, q, id)
		if err != nil {
			return AlarmRead{}, err
		}
		if cur.Status == "attended" || cur.Status == "cancelled" {
			return AlarmRead{}, ErrAlarmClosed
		}
		var notes *string
		if in.Notes != nil && strings.TrimSpace(*in.Notes) != "" {
			n := strings.TrimSpace(*in.Notes)
			notes = &n
		}
		rows, err := q.Query(ctx, `
			UPDATE security_alarms SET status = 'attended', attended_by = $3, attended_at = now(), notes = coalesce($4, notes), updated_at = now()
			WHERE reservation_id = $1 AND kind = $2 AND status IN ('scheduled', 'fired') AND (id = $5 OR fire_at BETWEEN $6::timestamptz - interval '4 hours' AND $6::timestamptz + interval '4 hours')
			RETURNING id`, cur.ReservationID, cur.Kind, actor.UserID, notes, id, cur.FireAt)
		if err != nil {
			return AlarmRead{}, err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return AlarmRead{}, err
		}
		for _, aid := range ids {
			if _, err := alerts.ResolveFor(ctx, q, alerts.TypeCheckoutAlarm, "security_alarm", aid, &actor.UserID); err != nil {
				return AlarmRead{}, err
			}
		}
		if err := audit.Record(ctx, q, audit.Entry{Actor: &actor.UserID, Action: "security.alarm_attended", Entity: "security_alarm",
			EntityID: &id, PropertyID: &cur.PropertyID, After: map[string]any{"reservation": cur.ReservationCode, "notes": notes}}); err != nil {
			return AlarmRead{}, err
		}
		out, err := alarmByID(ctx, q, id)
		if err != nil {
			return AlarmRead{}, err
		}
		holders, err := notifications.Holders(ctx, q, actor.TenantID, nil, authz.SecurityDashboard)
		if err != nil {
			return AlarmRead{}, err
		}
		return out, realtime.Publish(ctx, q, actor.TenantID, holders, "security.alarm", out)
	})
}

// Dashboard arma el tablero del dia (hoy si date esta vacio).
func (s *Service) Dashboard(ctx context.Context, date string) (SecurityDashboard, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (SecurityDashboard, error) {
		if date == "" {
			d, err := parking.Today(ctx, q, s.now())
			if err != nil {
				return SecurityDashboard{}, err
			}
			date = d
		} else if _, err := time.Parse("2006-01-02", date); err != nil {
			return SecurityDashboard{}, ErrDate.WithField("query.date")
		}
		out := SecurityDashboard{Date: date}
		var err error
		if out.Arrivals, err = stays(ctx, q, `r.check_in_date = $1::date AND r.status IN ('pending', 'confirmed', 'checked_in')`, date); err != nil {
			return SecurityDashboard{}, err
		}
		if out.Departures, err = stays(ctx, q, `r.check_out_date = $1::date AND r.status IN ('confirmed', 'checked_in', 'checked_out')`, date); err != nil {
			return SecurityDashboard{}, err
		}
		if out.InHouse, err = stays(ctx, q, `r.check_in_date < $1::date AND r.check_out_date > $1::date AND r.status IN ('confirmed', 'checked_in')`, date); err != nil {
			return SecurityDashboard{}, err
		}
		plates, err := parking.Overlapping(ctx, q, date, date)
		if err != nil {
			return SecurityDashboard{}, err
		}
		out.Plates = plates
		if out.AreaBookings, err = sharedareas.BookingsBetween(ctx, q, date, date); err != nil {
			return SecurityDashboard{}, err
		}
		rows, err := q.Query(ctx, alarmSelect+`
			WHERE a.status IN ('scheduled', 'fired') AND (a.status = 'fired' OR (a.fire_at AT TIME ZONE `+tz+`)::date = $1::date)
			ORDER BY a.status = 'fired' DESC, a.fire_at`, date)
		if err != nil {
			return SecurityDashboard{}, err
		}
		if out.Alarms, err = pgx.CollectRows(rows, pgx.RowToStructByName[AlarmRead]); err != nil {
			return SecurityDashboard{}, err
		}
		out.SecurityCounts = SecurityCounts{
			Arrivals: len(out.Arrivals), Departures: len(out.Departures), InHouse: len(out.InHouse),
			Plates: len(out.Plates), AreaBookings: len(out.AreaBookings), AlarmsPending: len(out.Alarms),
		}
		return out, nil
	})
}

func stays(ctx context.Context, q database.Querier, where, date string) ([]SecurityStay, error) {
	rows, err := q.Query(ctx, `
		SELECT r.id, r.code, p.id AS property_id, p.code AS property_code, p.name AS property_name, coalesce(b.name, '') AS building,
		       coalesce(g.full_name, r.external_summary, 'Huésped') AS guest_name, r.status,
		       to_char(r.check_in_date, 'YYYY-MM-DD') AS check_in_date, to_char(r.check_out_date, 'YYYY-MM-DD') AS check_out_date,
		       to_char(r.check_in_time, 'HH24:MI') AS check_in_time, to_char(r.check_out_time, 'HH24:MI') AS check_out_time,
		       r.adults, r.children, c.name AS channel_name,
		       coalesce((SELECT array_agg(DISTINCT pa.vehicle_plate) FROM parking_assignments pa WHERE pa.reservation_id = r.id), '{}') AS plates
		FROM reservations r
		JOIN properties p ON p.id = r.property_id
		JOIN booking_channels c ON c.id = r.channel_id
		LEFT JOIN buildings b ON b.id = p.building_id
		LEFT JOIN guests g ON g.id = r.primary_guest_id
		WHERE `+where+` ORDER BY r.check_in_time, p.code`, date)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[SecurityStay])
}

// --- Programacion --------------------------------------------------------------

// Subscribe programa y cancela las alarmas con los eventos de reservas.
func (s *Service) Subscribe(bus *events.Bus) {
	bus.Subscribe(reservations.EventConfirmed, s.onScheduled)
	bus.Subscribe(reservations.EventUpdated, s.onScheduled)
	bus.Subscribe(reservations.EventCancelled, s.onClosed)
	bus.Subscribe(reservations.EventCheckedOut, s.onClosed)
}

func reservationOf(e events.Event) (reservations.ReservationEvent, bool) {
	switch d := e.Data.(type) {
	case reservations.ReservationEvent:
		return d, true
	case *reservations.ReservationEvent:
		if d != nil {
			return *d, true
		}
	}
	return reservations.ReservationEvent{}, false
}

func (s *Service) onScheduled(ctx context.Context, q database.Querier, e events.Event) error {
	ev, ok := reservationOf(e)
	if !ok {
		return nil
	}
	r := ev.Reservation
	if p := ev.Previous; p != nil && p.CheckOutDate == r.CheckOutDate && p.CheckOutTime == r.CheckOutTime && p.Status == r.Status {
		return nil
	}
	return s.Schedule(ctx, q, r.ID)
}

func (s *Service) onClosed(ctx context.Context, q database.Querier, e events.Event) error {
	ev, ok := reservationOf(e)
	if !ok {
		return nil
	}
	return cancelAlarms(ctx, q, ev.Reservation.ID)
}

func cancelAlarms(ctx context.Context, q database.Querier, reservationID uuid.UUID) error {
	rows, err := q.Query(ctx, `
		UPDATE security_alarms SET status = 'cancelled', updated_at = now()
		WHERE reservation_id = $1 AND status IN ('scheduled', 'fired') RETURNING id`, reservationID)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := alerts.ResolveFor(ctx, q, alerts.TypeCheckoutAlarm, "security_alarm", id, nil); err != nil {
			return err
		}
	}
	return nil
}

// Schedule (re)programa las alarmas de salida de una reserva confirmada o en
// casa: la previa y la exacta, en la zona de la empresa. Las que ya pasaron no
// se crean.
func (s *Service) Schedule(ctx context.Context, q database.Querier, reservationID uuid.UUID) error {
	if _, err := q.Exec(ctx, `
		UPDATE security_alarms SET status = 'cancelled', updated_at = now()
		WHERE reservation_id = $1 AND status = 'scheduled'`, reservationID); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `
		WITH r AS (
			SELECT r.id, (r.check_out_date + r.check_out_time) AT TIME ZONE t.timezone AS out_at,
			       coalesce((t.settings->>'checkout_alarm_minutes')::int, 30) AS lead
			FROM reservations r, shared.tenants t
			WHERE r.id = $1 AND t.schema_name = current_schema() AND r.status IN ('confirmed', 'checked_in')
		)
		INSERT INTO security_alarms (reservation_id, kind, fire_at)
		SELECT r.id, 'checkout', f.at FROM r,
		       LATERAL (VALUES (r.out_at - make_interval(mins => r.lead)), (r.out_at)) AS f(at)
		WHERE f.at > $2 AND (r.lead > 0 OR f.at = r.out_at)
		ON CONFLICT DO NOTHING`, reservationID, s.now())
	return err
}

// --- Trabajo -------------------------------------------------------------------

// HandleJob corre security_alarms: sin empresa encola una por empresa activa
// con la bandera reservations; con empresa hace sonar y escala.
func (s *Service) HandleJob(ctx context.Context, job jobs.Running) error {
	if job.Type != JobAlarms {
		return fmt.Errorf("trabajo desconocido %s", job.Type)
	}
	if job.TenantID == nil {
		return s.db.InTx(ctx, func(q database.Querier) error {
			rows, err := q.Query(ctx, `SELECT id FROM shared.tenants WHERE is_active AND coalesce((features->>'reservations')::bool, false) ORDER BY slug`)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := jobs.Enqueue(ctx, q, jobs.Job{
					Type: JobAlarms, TenantID: &id, DedupeKey: JobAlarms + ":" + id.String(), MaxAttempts: 3,
				}); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		_, _, err := s.Run(ctx, q, *job.TenantID)
		return err
	})
}

// Run hace sonar las alarmas que llegaron a su hora y escala las que llevan
// una hora sin atender. Devuelve cuantas sono y cuantas escalo.
func (s *Service) Run(ctx context.Context, q database.Querier, tenantID uuid.UUID) (int, int, error) {
	now := s.now()
	rows, err := q.Query(ctx, `
		UPDATE security_alarms SET status = 'fired', fired_at = $1, updated_at = now()
		WHERE status = 'scheduled' AND fire_at <= $1 RETURNING id`, now)
	if err != nil {
		return 0, 0, err
	}
	fired, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, 0, err
	}
	holders, err := notifications.Holders(ctx, q, tenantID, nil, authz.SecurityDashboard)
	if err != nil {
		return 0, 0, err
	}
	for _, id := range fired {
		a, err := alarmByID(ctx, q, id)
		if err != nil {
			return 0, 0, err
		}
		title := fmt.Sprintf("Salida en un rato: %s", a.PropertyCode)
		if a.Exact {
			title = fmt.Sprintf("Hora de salida: %s", a.PropertyCode)
		}
		body := fmt.Sprintf("%s sale a las %s (%s).", a.GuestName, a.CheckOutTime, a.ReservationCode)
		if len(a.Plates) > 0 {
			body += " Placas: " + strings.Join(a.Plates, ", ") + "."
		}
		if err := s.notif.Notify(ctx, q, tenantID, notifications.Notice{
			Recipients: holders, Type: notifications.TypeCheckoutAlarm, Title: title, Body: body,
			AppRoute: "/security", Data: map[string]any{"alarm_id": a.ID, "reservation_id": a.ReservationID},
			Channels: []string{notifications.ChannelPush},
		}); err != nil {
			return 0, 0, err
		}
		if err := realtime.Publish(ctx, q, tenantID, holders, "security.alarm", a); err != nil {
			return 0, 0, err
		}
	}
	rows, err = q.Query(ctx, `
		UPDATE security_alarms SET escalated_at = $1, updated_at = now()
		WHERE status = 'fired' AND escalated_at IS NULL AND fired_at <= $2 RETURNING id`, now, now.Add(-UnattendedAfter))
	if err != nil {
		return 0, 0, err
	}
	late, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, 0, err
	}
	for _, id := range late {
		a, err := alarmByID(ctx, q, id)
		if err != nil {
			return 0, 0, err
		}
		if _, _, err := s.alerts.Raise(ctx, q, tenantID, alerts.Raise{
			Type: alerts.TypeCheckoutAlarm, Severity: alerts.SeverityHigh, EntityType: "security_alarm", EntityID: a.ID,
			PropertyID: &a.PropertyID,
			Message:    fmt.Sprintf("Nadie atendió la alarma de salida de %s (%s) en %s.", a.GuestName, a.ReservationCode, a.PropertyCode),
		}); err != nil {
			return 0, 0, err
		}
	}
	return len(fired), len(late), nil
}
