// Package sharedareas son las areas comunes de cada edificio (piscina, salon,
// terraza) y sus reservas por bloques (fase04.md §2-3, Persona B, bandera
// shared_areas).
//
// Sigue el contrato de la web (features/shared-areas): la capacidad son
// personas a la vez y cada reserva dice cuantas van. Al reservar se bloquea el
// area (SELECT ... FOR UPDATE) y se suman las personas de las reservas que se
// cruzan, asi dos peticiones a la vez no pasan de la capacidad.
//
// El personal reserva para un huesped hospedado; el huesped reserva desde la
// app (guestaccess) dentro de su estancia, con la anticipacion del area, y
// cancela hasta 1 h antes.
package sharedareas

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/reservations"
)

// Errores del modulo.
var (
	ErrAreaNotFound    = apperr.New(apperr.KindNotFound, "El área ya no existe.")
	ErrBookingNotFound = apperr.New(apperr.KindNotFound, "La reserva del área no existe.")

	ErrHours        = apperr.New(apperr.KindValidation, "El cierre debe ser después de la apertura.")
	ErrTime         = apperr.New(apperr.KindValidation, "Usa el formato HH:MM.")
	ErrDate         = apperr.New(apperr.KindValidation, "La fecha no es válida.")
	ErrPlace        = apperr.New(apperr.KindValidation, "Indica el edificio o la propiedad del área.")
	ErrUnknownPlace = apperr.New(apperr.KindValidation, "Ese edificio o propiedad no existe.")
	ErrInactive     = apperr.New(apperr.KindValidation, "El área no está disponible.")
	ErrPast         = apperr.New(apperr.KindValidation, "Ese horario ya pasó.")
	ErrGuest        = apperr.New(apperr.KindValidation, "Elige un huésped que esté hospedado ese día.")
	ErrStay         = apperr.New(apperr.KindValidation, "Solo puedes reservar durante tu estancia.")
	ErrTooFar       = apperr.New(apperr.KindValidation, "Todavía no se puede reservar esa fecha.")
	ErrLateCancel   = apperr.New(apperr.KindValidation, "Solo puedes cancelar hasta 1 hora antes.")
	ErrNotYours     = apperr.New(apperr.KindNotFound, "La reserva del área no existe.")
)

// AreaRead es un area comun (GET /shared-areas).
type AreaRead struct {
	ID           uuid.UUID  `db:"id" json:"id" format:"uuid"`
	Name         string     `db:"name" json:"name"`
	Description  *string    `db:"description" json:"description" nullable:"true"`
	Building     string     `db:"building" json:"building" doc:"Nombre del edificio (o de la propiedad)."`
	BuildingID   *uuid.UUID `db:"building_id" json:"building_id" format:"uuid" nullable:"true"`
	PropertyID   *uuid.UUID `db:"property_id" json:"property_id" format:"uuid" nullable:"true"`
	Capacity     int        `db:"capacity" json:"capacity" doc:"Personas a la vez."`
	Opens        string     `db:"opens" json:"opens" doc:"HH:MM"`
	Closes       string     `db:"closes" json:"closes" doc:"HH:MM"`
	Block        int        `db:"slot_minutes" json:"block" doc:"Minutos por bloque."`
	MaxPerDay    int        `db:"max_per_reservation_per_day" json:"max_per_day" doc:"Bloques por reserva y dia."`
	AdvanceHours int        `db:"advance_hours" json:"advance_hours" doc:"Anticipacion minima del huesped."`
	AdvanceDays  int        `db:"advance_days" json:"advance_days" doc:"Hasta cuantos dias adelante reserva el huesped."`
	Rules        string     `db:"rules" json:"rules"`
	Photo        string     `db:"photo" json:"photo" enum:"beachPool,brightRoom,lakeRoom,antiguaArch"`
	Active       bool       `db:"is_active" json:"active"`
}

// AreaWrite es el cuerpo de POST /shared-areas y PUT /shared-areas/{id}. En
// PUT se ignoran el lugar y los campos de solo lectura (id, building).
type AreaWrite struct {
	_            struct{}   `additionalProperties:"true"`
	BuildingID   *uuid.UUID `json:"building_id,omitempty" format:"uuid" doc:"Solo al crear."`
	PropertyID   *uuid.UUID `json:"property_id,omitempty" format:"uuid" doc:"Solo al crear."`
	Name         string     `json:"name" minLength:"2" maxLength:"128"`
	Description  *string    `json:"description,omitempty" maxLength:"2000"`
	Capacity     int        `json:"capacity" minimum:"1" maximum:"500"`
	Opens        string     `json:"opens" doc:"HH:MM"`
	Closes       string     `json:"closes" doc:"HH:MM"`
	Block        int        `json:"block" minimum:"15" maximum:"720"`
	MaxPerDay    int        `json:"max_per_day" minimum:"1" maximum:"24"`
	AdvanceHours int        `json:"advance_hours" minimum:"0" maximum:"720"`
	AdvanceDays  int        `json:"advance_days,omitempty" minimum:"0" maximum:"365"`
	Rules        string     `json:"rules,omitempty" maxLength:"4000"`
	Photo        string     `json:"photo,omitempty" enum:"beachPool,brightRoom,lakeRoom,antiguaArch"`
	Active       *bool      `json:"active,omitempty"`
}

// BookingRead es un bloque reservado.
type BookingRead struct {
	ID              uuid.UUID  `db:"id" json:"id" format:"uuid"`
	AreaID          uuid.UUID  `db:"area_id" json:"area_id" format:"uuid"`
	AreaName        string     `db:"area_name" json:"area_name"`
	Date            string     `db:"date" json:"date" format:"date"`
	Start           string     `db:"start" json:"start" doc:"HH:MM"`
	End             string     `db:"end" json:"end" doc:"HH:MM"`
	GuestName       string     `db:"guest_name" json:"guest_name"`
	PropertyCode    string     `db:"property_code" json:"property_code"`
	ReservationID   *uuid.UUID `db:"reservation_id" json:"reservation_id" format:"uuid" nullable:"true"`
	ReservationCode *string    `db:"reservation_code" json:"reservation_code" nullable:"true"`
	People          int        `db:"people" json:"people"`
	ByStaff         bool       `db:"by_staff" json:"by_staff"`
	StartsAt        time.Time  `db:"starts_at" json:"starts_at"`
}

// BookingCreate es el cuerpo de POST /shared-areas/bookings (personal).
type BookingCreate struct {
	_             struct{}   `additionalProperties:"true"`
	AreaID        uuid.UUID  `json:"area_id" format:"uuid"`
	Date          string     `json:"date" format:"date"`
	Start         string     `json:"start" doc:"HH:MM"`
	Guest         string     `json:"guest,omitempty" maxLength:"200" doc:"Nombre|CODIGO_PROPIEDAD, como lo arma la web."`
	ReservationID *uuid.UUID `json:"reservation_id,omitempty" format:"uuid" doc:"En lugar de guest."`
	People        int        `json:"people" minimum:"1" maximum:"500"`
}

// GuestBookingCreate es el cuerpo de POST /guest/shared-areas/{id}/bookings.
type GuestBookingCreate struct {
	_      struct{} `additionalProperties:"true"`
	Date   string   `json:"date" format:"date"`
	Start  string   `json:"start" doc:"HH:MM"`
	People int      `json:"people" minimum:"1" maximum:"50"`
}

// InHouseGuest es un huesped hospedado hoy (GET /reservations/in-house).
type InHouseGuest struct {
	Name            string    `db:"name" json:"name"`
	PropertyCode    string    `db:"property_code" json:"property_code"`
	ReservationID   uuid.UUID `db:"reservation_id" json:"reservation_id" format:"uuid"`
	ReservationCode string    `db:"reservation_code" json:"reservation_code"`
	CheckOutDate    string    `db:"check_out_date" json:"check_out_date" format:"date"`
}

// AreaSlot es un bloque del dia con los lugares libres.
type AreaSlot struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Left  int    `json:"left" doc:"Personas que aun caben."`
	Past  bool   `json:"past" doc:"Ya paso o no cumple la anticipacion."`
}

// AreaAvailability es GET .../availability?date=.
type AreaAvailability struct {
	AreaID uuid.UUID  `json:"area_id" format:"uuid"`
	Date   string     `json:"date" format:"date"`
	Slots  []AreaSlot `json:"slots"`
}

// Stay es la reserva por la que se aparta un bloque.
type Stay struct {
	ReservationID uuid.UUID
	GuestID       *uuid.UUID
	GuestName     string
	PropertyID    uuid.UUID
	BuildingID    *uuid.UUID
	CheckIn       string
	CheckOut      string
}

// Service contiene los casos de uso de areas comunes.
type Service struct {
	db  *database.DB
	log *slog.Logger
	now func() time.Time
}

// NewService crea el servicio.
func NewService(db *database.DB, log *slog.Logger) *Service {
	return &Service{db: db, log: log, now: time.Now}
}

// SetClock cambia el reloj (pruebas).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Now es el reloj del servicio.
func (s *Service) Now() time.Time { return s.now() }

const areaSelect = `
	SELECT a.id, a.name, a.description, coalesce(b.name, p.name, '') AS building, a.building_id, a.property_id,
	       a.capacity, to_char(a.opens_at, 'HH24:MI') AS opens, to_char(a.closes_at, 'HH24:MI') AS closes,
	       a.slot_minutes, a.max_per_reservation_per_day, a.advance_hours, a.advance_days, coalesce(a.rules, '') AS rules,
	       a.photo, a.is_active
	FROM shared_areas a
	LEFT JOIN buildings b ON b.id = a.building_id
	LEFT JOIN properties p ON p.id = a.property_id`

func bookingSelect(tz string) string {
	return `
	SELECT k.id, k.area_id, a.name AS area_name,
	       to_char((k.starts_at AT TIME ZONE ` + tz + `)::date, 'YYYY-MM-DD') AS date,
	       to_char(k.starts_at AT TIME ZONE ` + tz + `, 'HH24:MI') AS start,
	       to_char(k.ends_at AT TIME ZONE ` + tz + `, 'HH24:MI') AS "end",
	       k.guest_name, coalesce(p.code, '—') AS property_code, k.reservation_id, r.code AS reservation_code,
	       k.people, k.booked_by_user_id IS NOT NULL AS by_staff, k.starts_at
	FROM shared_area_bookings k
	JOIN shared_areas a ON a.id = k.area_id
	LEFT JOIN reservations r ON r.id = k.reservation_id
	LEFT JOIN properties p ON p.id = r.property_id`
}

// tz es la zona horaria de la empresa del schema activo (como expresion SQL).
const tz = `(SELECT timezone FROM shared.tenants WHERE schema_name = current_schema())`

// Areas lista las areas (solo las activas salvo all).
func (s *Service) Areas(ctx context.Context, all bool) ([]AreaRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]AreaRead, error) {
		rows, err := q.Query(ctx, areaSelect+` WHERE a.is_active OR $1 ORDER BY a.name`, all)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[AreaRead])
	})
}

// AreasFor son las areas activas del edificio (o la propiedad) de una estancia.
func AreasFor(ctx context.Context, q database.Querier, propertyID uuid.UUID, buildingID *uuid.UUID) ([]AreaRead, error) {
	rows, err := q.Query(ctx, areaSelect+`
		WHERE a.is_active AND (a.property_id = $1 OR ($2::uuid IS NOT NULL AND a.building_id = $2)) ORDER BY a.name`,
		propertyID, buildingID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[AreaRead])
}

func areaByID(ctx context.Context, q database.Querier, id uuid.UUID, lock bool) (AreaRead, error) {
	if lock {
		if _, err := q.Exec(ctx, `SELECT 1 FROM shared_areas WHERE id = $1 FOR UPDATE`, id); err != nil {
			return AreaRead{}, err
		}
	}
	rows, err := q.Query(ctx, areaSelect+` WHERE a.id = $1`, id)
	if err != nil {
		return AreaRead{}, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[AreaRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return AreaRead{}, ErrAreaNotFound
	}
	return a, err
}

var hhmmRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

func minutesOf(hhmm string) int {
	var h, m int
	_, _ = fmt.Sscanf(hhmm, "%d:%d", &h, &m)
	return h*60 + m
}

func hhmm(min int) string {
	return fmt.Sprintf("%02d:%02d", min/60, min%60)
}

func validArea(in AreaWrite) error {
	if !hhmmRe.MatchString(in.Opens) {
		return ErrTime.WithField("body.opens")
	}
	if !hhmmRe.MatchString(in.Closes) {
		return ErrTime.WithField("body.closes")
	}
	if minutesOf(in.Closes) <= minutesOf(in.Opens) {
		return ErrHours.WithField("body.closes")
	}
	return nil
}

func photoOr(p string) string {
	if p == "" {
		return "brightRoom"
	}
	return p
}

// CreateArea da de alta un area en un edificio o una propiedad.
func (s *Service) CreateArea(ctx context.Context, actor authz.Actor, in AreaWrite) (AreaRead, error) {
	if (in.BuildingID == nil) == (in.PropertyID == nil) {
		return AreaRead{}, ErrPlace.WithField("body.building_id")
	}
	if err := validArea(in); err != nil {
		return AreaRead{}, err
	}
	days := in.AdvanceDays
	if days == 0 {
		days = 7
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (AreaRead, error) {
		var ok bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM buildings WHERE id = $1) OR EXISTS (SELECT 1 FROM properties WHERE id = $2 AND deleted_at IS NULL)`,
			in.BuildingID, in.PropertyID).Scan(&ok); err != nil {
			return AreaRead{}, err
		}
		if !ok {
			return AreaRead{}, ErrUnknownPlace.WithField("body.building_id")
		}
		active := in.Active == nil || *in.Active
		var id uuid.UUID
		if err := q.QueryRow(ctx, `
			INSERT INTO shared_areas (building_id, property_id, name, description, capacity, slot_minutes, opens_at, closes_at,
			                          max_per_reservation_per_day, advance_hours, advance_days, rules, photo, is_active)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, nullif($12, ''), $13, $14) RETURNING id`,
			in.BuildingID, in.PropertyID, strings.TrimSpace(in.Name), in.Description, in.Capacity, in.Block, in.Opens, in.Closes,
			in.MaxPerDay, in.AdvanceHours, days, strings.TrimSpace(in.Rules), photoOr(in.Photo), active).Scan(&id); err != nil {
			return AreaRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{Actor: &actor.UserID, Action: "shared_area.created", Entity: "shared_area", EntityID: &id,
			After: map[string]any{"name": in.Name}}); err != nil {
			return AreaRead{}, err
		}
		return areaByID(ctx, q, id, false)
	})
}

// SaveArea reemplaza la configuracion de un area (PUT).
func (s *Service) SaveArea(ctx context.Context, actor authz.Actor, id uuid.UUID, in AreaWrite) (AreaRead, error) {
	if err := validArea(in); err != nil {
		return AreaRead{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (AreaRead, error) {
		cur, err := areaByID(ctx, q, id, true)
		if err != nil {
			return AreaRead{}, err
		}
		active := cur.Active
		if in.Active != nil {
			active = *in.Active
		}
		days := in.AdvanceDays
		if days == 0 {
			days = cur.AdvanceDays
		}
		if _, err := q.Exec(ctx, `
			UPDATE shared_areas SET name = $2, description = $3, capacity = $4, slot_minutes = $5, opens_at = $6, closes_at = $7,
			       max_per_reservation_per_day = $8, advance_hours = $9, advance_days = $10, rules = nullif($11, ''), photo = $12,
			       is_active = $13, updated_at = now()
			WHERE id = $1`,
			id, strings.TrimSpace(in.Name), in.Description, in.Capacity, in.Block, in.Opens, in.Closes, in.MaxPerDay, in.AdvanceHours,
			days, strings.TrimSpace(in.Rules), photoOr(in.Photo), active); err != nil {
			return AreaRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{Actor: &actor.UserID, Action: "shared_area.updated", Entity: "shared_area", EntityID: &id,
			Before: map[string]any{"capacity": cur.Capacity, "opens": cur.Opens, "closes": cur.Closes, "active": cur.Active},
			After:  map[string]any{"capacity": in.Capacity, "opens": in.Opens, "closes": in.Closes, "active": active}}); err != nil {
			return AreaRead{}, err
		}
		return areaByID(ctx, q, id, false)
	})
}

// Bookings lista los bloques confirmados de from a to (inclusive).
func (s *Service) Bookings(ctx context.Context, from, to string) ([]BookingRead, error) {
	if !validDate(from) || !validDate(to) {
		return nil, ErrDate.WithField("query.from")
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]BookingRead, error) {
		return BookingsBetween(ctx, q, from, to)
	})
}

// BookingsBetween son los bloques confirmados de from a to (dias locales).
func BookingsBetween(ctx context.Context, q database.Querier, from, to string) ([]BookingRead, error) {
	rows, err := q.Query(ctx, bookingSelect(tz)+`
		WHERE k.status = 'confirmed' AND (k.starts_at AT TIME ZONE `+tz+`)::date BETWEEN $1::date AND $2::date
		ORDER BY k.starts_at, a.name`, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[BookingRead])
}

// ForReservation son los bloques confirmados de una estancia (los del huesped).
func ForReservation(ctx context.Context, q database.Querier, reservationID uuid.UUID) ([]BookingRead, error) {
	rows, err := q.Query(ctx, bookingSelect(tz)+`
		WHERE k.status = 'confirmed' AND k.reservation_id = $1 ORDER BY k.starts_at`, reservationID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[BookingRead])
}

func bookingByID(ctx context.Context, q database.Querier, id uuid.UUID) (BookingRead, error) {
	rows, err := q.Query(ctx, bookingSelect(tz)+` WHERE k.id = $1 AND k.status = 'confirmed'`, id)
	if err != nil {
		return BookingRead{}, err
	}
	b, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[BookingRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return BookingRead{}, ErrBookingNotFound
	}
	return b, err
}

// InHouse son los huespedes hospedados hoy (en casa o con entrada hoy).
func (s *Service) InHouse(ctx context.Context) ([]InHouseGuest, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]InHouseGuest, error) {
		rows, err := q.Query(ctx, `
			SELECT coalesce(g.full_name, r.external_summary, 'Huésped') AS name, p.code AS property_code,
			       r.id AS reservation_id, r.code AS reservation_code, to_char(r.check_out_date, 'YYYY-MM-DD') AS check_out_date
			FROM reservations r
			JOIN properties p ON p.id = r.property_id
			LEFT JOIN guests g ON g.id = r.primary_guest_id
			WHERE r.status IN ('confirmed', 'checked_in')
			  AND r.check_in_date <= ($1::timestamptz AT TIME ZONE `+tz+`)::date
			  AND r.check_out_date > ($1::timestamptz AT TIME ZONE `+tz+`)::date
			ORDER BY 1`, s.now())
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[InHouseGuest])
	})
}

// AreaAvailability son los bloques del dia con los lugares libres. Para el
// huesped (guest) marca como pasados los que no cumplen la anticipacion.
func (s *Service) Availability(ctx context.Context, q database.Querier, areaID uuid.UUID, date string, guest bool) (AreaAvailability, error) {
	if !validDate(date) {
		return AreaAvailability{}, ErrDate.WithField("query.date")
	}
	area, err := areaByID(ctx, q, areaID, false)
	if err != nil {
		return AreaAvailability{}, err
	}
	rows, err := q.Query(ctx, `
		SELECT to_char(starts_at AT TIME ZONE `+tz+`, 'HH24:MI'), to_char(ends_at AT TIME ZONE `+tz+`, 'HH24:MI'), people
		FROM shared_area_bookings
		WHERE area_id = $1 AND status = 'confirmed' AND (starts_at AT TIME ZONE `+tz+`)::date = $2::date`, areaID, date)
	if err != nil {
		return AreaAvailability{}, err
	}
	type used struct {
		Start, End string
		People     int
	}
	taken, err := pgx.CollectRows(rows, pgx.RowToStructByPos[used])
	if err != nil {
		return AreaAvailability{}, err
	}
	var nowLocal string
	if err := q.QueryRow(ctx, `SELECT to_char($1::timestamptz AT TIME ZONE `+tz+`, 'YYYY-MM-DD HH24:MI')`, s.now()).Scan(&nowLocal); err != nil {
		return AreaAvailability{}, err
	}
	out := AreaAvailability{AreaID: areaID, Date: date, Slots: []AreaSlot{}}
	lead := 0
	if guest {
		lead = area.AdvanceHours * 60
	}
	for start := minutesOf(area.Opens); start+area.Block <= minutesOf(area.Closes); start += area.Block {
		end := start + area.Block
		sum := 0
		for _, t := range taken {
			if minutesOf(t.Start) < end && start < minutesOf(t.End) {
				sum += t.People
			}
		}
		past := date+" "+hhmm(start) < shiftLocal(nowLocal, lead)
		out.Slots = append(out.Slots, AreaSlot{Start: hhmm(start), End: hhmm(end), Left: max(0, area.Capacity-sum), Past: past})
	}
	return out, nil
}

// shiftLocal suma minutos a "YYYY-MM-DD HH:MM".
func shiftLocal(local string, minutes int) string {
	t, err := time.Parse("2006-01-02 15:04", local)
	if err != nil {
		return local
	}
	return t.Add(time.Duration(minutes) * time.Minute).Format("2006-01-02 15:04")
}

// AvailabilityTx abre su propia transaccion (personal).
func (s *Service) AvailabilityTx(ctx context.Context, areaID uuid.UUID, date string) (AreaAvailability, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (AreaAvailability, error) {
		return s.Availability(ctx, q, areaID, date, false)
	})
}

// Book aparta un bloque a nombre de un huesped hospedado (personal).
func (s *Service) Book(ctx context.Context, actor authz.Actor, in BookingCreate) (BookingRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (BookingRead, error) {
		if !validDate(in.Date) {
			return BookingRead{}, ErrDate.WithField("body.date")
		}
		stay, err := stayFor(ctx, q, in)
		if err != nil {
			return BookingRead{}, err
		}
		b, err := s.BookTx(ctx, q, in.AreaID, in.Date, in.Start, in.People, stay, &actor.UserID)
		if err != nil {
			return BookingRead{}, err
		}
		return b, audit.Record(ctx, q, audit.Entry{Actor: &actor.UserID, Action: "shared_area.booked", Entity: "shared_area_booking",
			EntityID: &b.ID, PropertyID: &stay.PropertyID, After: map[string]any{"area": b.AreaName, "date": b.Date, "start": b.Start, "guest": b.GuestName}})
	})
}

// stayFor busca la estancia del huesped elegido en la web ("Nombre|CODIGO").
func stayFor(ctx context.Context, q database.Querier, in BookingCreate) (Stay, error) {
	var name, code string
	if in.ReservationID == nil {
		name, code, _ = strings.Cut(in.Guest, "|")
		if strings.TrimSpace(name) == "" {
			return Stay{}, ErrGuest.WithField("body.guest")
		}
	}
	var st Stay
	err := q.QueryRow(ctx, `
		SELECT r.id, r.primary_guest_id, coalesce(g.full_name, r.external_summary, 'Huésped'), r.property_id, p.building_id,
		       to_char(r.check_in_date, 'YYYY-MM-DD'), to_char(r.check_out_date, 'YYYY-MM-DD')
		FROM reservations r
		JOIN properties p ON p.id = r.property_id
		LEFT JOIN guests g ON g.id = r.primary_guest_id
		WHERE r.status IN ('confirmed', 'checked_in') AND r.check_in_date <= $1::date AND r.check_out_date >= $1::date
		  AND (($2::uuid IS NOT NULL AND r.id = $2)
		       OR ($2::uuid IS NULL AND lower(coalesce(g.full_name, r.external_summary, '')) = lower(trim($3))
		           AND ($4 = '' OR upper(p.code) = upper(trim($4)))))
		ORDER BY r.check_in_date LIMIT 1`, in.Date, in.ReservationID, name, code).
		Scan(&st.ReservationID, &st.GuestID, &st.GuestName, &st.PropertyID, &st.BuildingID, &st.CheckIn, &st.CheckOut)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stay{}, ErrGuest.WithField("body.guest")
	}
	return st, err
}

// BookTx aparta el bloque dentro de la transaccion q. by es quien reserva del
// personal; nil = el huesped desde la app (aplica anticipacion y estancia).
func (s *Service) BookTx(ctx context.Context, q database.Querier, areaID uuid.UUID, date, start string, people int, st Stay, by *uuid.UUID) (BookingRead, error) {
	if !validDate(date) {
		return BookingRead{}, ErrDate.WithField("body.date")
	}
	if !hhmmRe.MatchString(start) {
		return BookingRead{}, ErrTime.WithField("body.start")
	}
	if people < 1 {
		people = 1
	}
	area, err := areaByID(ctx, q, areaID, true)
	if err != nil {
		return BookingRead{}, err
	}
	if !area.Active {
		return BookingRead{}, ErrInactive
	}
	if by == nil && !sameplace(area, st) {
		return BookingRead{}, ErrAreaNotFound
	}
	startMin := minutesOf(start)
	endMin := startMin + area.Block
	if startMin < minutesOf(area.Opens) || endMin > minutesOf(area.Closes) {
		return BookingRead{}, apperr.New(apperr.KindValidation, fmt.Sprintf("Abre de %s a %s.", area.Opens, area.Closes)).WithField("body.start")
	}
	var nowLocal, today string
	if err := q.QueryRow(ctx, `SELECT to_char($1::timestamptz AT TIME ZONE `+tz+`, 'YYYY-MM-DD HH24:MI'),
		to_char(($1::timestamptz AT TIME ZONE `+tz+`)::date, 'YYYY-MM-DD')`, s.now()).Scan(&nowLocal, &today); err != nil {
		return BookingRead{}, err
	}
	if date+" "+start < nowLocal {
		return BookingRead{}, ErrPast.WithField("body.start")
	}
	if by == nil {
		if date < st.CheckIn || date > st.CheckOut {
			return BookingRead{}, ErrStay.WithField("body.date")
		}
		if date+" "+start < shiftLocal(nowLocal, area.AdvanceHours*60) {
			return BookingRead{}, apperr.New(apperr.KindValidation,
				fmt.Sprintf("Reserva con al menos %d horas de anticipación.", area.AdvanceHours)).WithField("body.start")
		}
		if limit := shiftLocal(today+" 00:00", area.AdvanceDays*24*60)[:10]; date > limit {
			return BookingRead{}, ErrTooFar.WithField("body.date")
		}
	}
	var used int
	if err := q.QueryRow(ctx, `
		SELECT coalesce(sum(people), 0) FROM shared_area_bookings
		WHERE area_id = $1 AND status = 'confirmed'
		  AND starts_at < (($2::date + $4::time) AT TIME ZONE `+tz+`)
		  AND ends_at > (($2::date + $3::time) AT TIME ZONE `+tz+`)`,
		areaID, date, start, hhmm(endMin)).Scan(&used); err != nil {
		return BookingRead{}, err
	}
	if used+people > area.Capacity {
		return BookingRead{}, apperr.New(apperr.KindValidation,
			fmt.Sprintf("Solo quedan %d lugares en ese horario.", max(0, area.Capacity-used))).WithField("body.people")
	}
	var perDay int
	if err := q.QueryRow(ctx, `
		SELECT count(*) FROM shared_area_bookings
		WHERE area_id = $1 AND status = 'confirmed' AND reservation_id = $2 AND (starts_at AT TIME ZONE `+tz+`)::date = $3::date`,
		areaID, st.ReservationID, date).Scan(&perDay); err != nil {
		return BookingRead{}, err
	}
	if perDay >= area.MaxPerDay {
		return BookingRead{}, apperr.New(apperr.KindValidation,
			fmt.Sprintf("%s ya tiene %d bloques ese día (máximo %d).", st.GuestName, perDay, area.MaxPerDay)).WithField("body.start")
	}
	var id uuid.UUID
	if err := q.QueryRow(ctx, `
		INSERT INTO shared_area_bookings (area_id, reservation_id, guest_id, guest_name, booked_by_user_id, people, starts_at, ends_at)
		VALUES ($1, $2, $3, $4, $5, $6, ($7::date + $8::time) AT TIME ZONE `+tz+`, ($7::date + $9::time) AT TIME ZONE `+tz+`)
		RETURNING id`,
		areaID, st.ReservationID, st.GuestID, st.GuestName, by, people, date, start, hhmm(endMin)).Scan(&id); err != nil {
		return BookingRead{}, err
	}
	return bookingByID(ctx, q, id)
}

func sameplace(a AreaRead, st Stay) bool {
	if a.PropertyID != nil && *a.PropertyID == st.PropertyID {
		return true
	}
	return a.BuildingID != nil && st.BuildingID != nil && *a.BuildingID == *st.BuildingID
}

// Cancel cancela un bloque (personal, sin limite de hora).
func (s *Service) Cancel(ctx context.Context, actor authz.Actor, id uuid.UUID) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		b, err := bookingByID(ctx, q, id)
		if err != nil {
			return err
		}
		if err := cancel(ctx, q, id, "Cancelada por el personal."); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Actor: &actor.UserID, Action: "shared_area.cancelled", Entity: "shared_area_booking",
			EntityID: &id, Before: map[string]any{"area": b.AreaName, "date": b.Date, "start": b.Start, "guest": b.GuestName}})
	})
}

// CancelForGuest cancela un bloque de la estancia hasta 1 h antes.
func (s *Service) CancelForGuest(ctx context.Context, q database.Querier, reservationID, id uuid.UUID) error {
	b, err := bookingByID(ctx, q, id)
	if err != nil {
		return err
	}
	if b.ReservationID == nil || *b.ReservationID != reservationID {
		return ErrNotYours
	}
	if b.StartsAt.Before(s.now().Add(time.Hour)) {
		return ErrLateCancel
	}
	return cancel(ctx, q, id, "Cancelada por el huésped.")
}

func cancel(ctx context.Context, q database.Querier, id uuid.UUID, reason string) error {
	_, err := q.Exec(ctx, `UPDATE shared_area_bookings SET status = 'cancelled', cancelled_at = now(), cancel_reason = $2 WHERE id = $1`, id, reason)
	return err
}

// Subscribe cancela los bloques futuros de una reserva cancelada.
func (s *Service) Subscribe(bus *events.Bus) {
	bus.Subscribe(reservations.EventCancelled, func(ctx context.Context, q database.Querier, e events.Event) error {
		var id uuid.UUID
		switch d := e.Data.(type) {
		case reservations.ReservationEvent:
			id = d.Reservation.ID
		case *reservations.ReservationEvent:
			id = d.Reservation.ID
		default:
			return nil
		}
		_, err := q.Exec(ctx, `
			UPDATE shared_area_bookings SET status = 'cancelled', cancelled_at = now(), cancel_reason = 'Se canceló la reserva.'
			WHERE reservation_id = $1 AND status = 'confirmed' AND starts_at > now()`, id)
		return err
	})
}

func validDate(d string) bool {
	_, err := time.Parse("2006-01-02", d)
	return err == nil
}
