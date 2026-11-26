// Package parking son los espacios de parqueo de cada edificio y su
// asignacion a las reservas (fase04.md §2-3, Persona B, bandera parking).
//
// Sigue el contrato que ya usa la web (features/parking): espacios con nivel,
// tipo y techo; asignaciones por las fechas de la estancia (la salida es
// exclusiva, igual que la reserva) y la BD rechaza los cruces (EXCLUDE).
//
// Integraciones: reservation.cancelled libera los espacios de la reserva y
// reservation.updated mueve las fechas (si el espacio sigue libre) o lo libera.
// El huesped ve su espacio (guestaccess) y seguridad ve las placas del dia.
package parking

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
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/reservations"
)

// Errores del modulo.
var (
	ErrSpaceNotFound      = apperr.New(apperr.KindNotFound, "El espacio no existe.")
	ErrAssignmentNotFound = apperr.New(apperr.KindNotFound, "La asignación no existe.")

	ErrPlate        = apperr.New(apperr.KindValidation, "Escribe la placa como P123ABC.")
	ErrReservation  = apperr.New(apperr.KindValidation, "Elige una reserva.")
	ErrBlocked      = apperr.New(apperr.KindValidation, "Este espacio está bloqueado.")
	ErrOtherPlace   = apperr.New(apperr.KindValidation, "Ese espacio es de otro edificio.")
	ErrPlace        = apperr.New(apperr.KindValidation, "Indica el edificio o la propiedad del espacio.")
	ErrUnknownPlace = apperr.New(apperr.KindValidation, "Ese edificio o propiedad no existe.")
	ErrCodeTaken    = apperr.New(apperr.KindValidation, "Ya hay un espacio con ese código en el edificio.")
	ErrTaken        = apperr.New(apperr.KindAlreadyExists, "El espacio está ocupado en esas fechas.")
)

// SpaceRead es un espacio (GET /parking/spaces).
type SpaceRead struct {
	ID         uuid.UUID  `db:"id" json:"id" format:"uuid"`
	Code       string     `db:"code" json:"code"`
	Building   string     `db:"building" json:"building" doc:"Nombre del edificio (o de la propiedad si el espacio es suyo)."`
	BuildingID *uuid.UUID `db:"building_id" json:"building_id" format:"uuid" nullable:"true"`
	PropertyID *uuid.UUID `db:"property_id" json:"property_id" format:"uuid" nullable:"true"`
	Level      string     `db:"level" json:"level"`
	Kind       string     `db:"kind" json:"kind" enum:"guest,visitor,staff"`
	Covered    bool       `db:"covered" json:"covered"`
	Blocked    bool       `db:"is_blocked" json:"blocked"`
	Active     bool       `db:"is_active" json:"active"`
}

// SpaceCreate es el cuerpo de POST /parking/spaces.
type SpaceCreate struct {
	_          struct{}   `additionalProperties:"true"`
	BuildingID *uuid.UUID `json:"building_id,omitempty" format:"uuid"`
	PropertyID *uuid.UUID `json:"property_id,omitempty" format:"uuid"`
	Code       string     `json:"code" minLength:"1" maxLength:"16"`
	Level      string     `json:"level,omitempty" maxLength:"64"`
	Kind       string     `json:"kind,omitempty" enum:"guest,visitor,staff"`
	Covered    bool       `json:"covered,omitempty"`
}

// SpaceUpdate es el cuerpo de PATCH /parking/spaces/{id}.
type SpaceUpdate struct {
	_       struct{} `additionalProperties:"true"`
	Code    *string  `json:"code,omitempty" minLength:"1" maxLength:"16"`
	Level   *string  `json:"level,omitempty" maxLength:"64"`
	Kind    *string  `json:"kind,omitempty" enum:"guest,visitor,staff"`
	Covered *bool    `json:"covered,omitempty"`
	Blocked *bool    `json:"blocked,omitempty"`
	Active  *bool    `json:"active,omitempty"`
}

// AssignmentRead es un espacio asignado a una reserva.
type AssignmentRead struct {
	ID              uuid.UUID `db:"id" json:"id" format:"uuid"`
	SpaceID         uuid.UUID `db:"spot_id" json:"space_id" format:"uuid"`
	SpaceCode       string    `db:"space_code" json:"space_code"`
	Building        string    `db:"building" json:"building"`
	Level           string    `db:"level" json:"level"`
	Covered         bool      `db:"covered" json:"covered"`
	ReservationID   uuid.UUID `db:"reservation_id" json:"reservation_id" format:"uuid"`
	ReservationCode string    `db:"reservation_code" json:"reservation_code"`
	GuestName       string    `db:"guest_name" json:"guest_name"`
	PropertyCode    string    `db:"property_code" json:"property_code"`
	Plate           string    `db:"vehicle_plate" json:"plate"`
	Description     *string   `db:"vehicle_description" json:"vehicle_description" nullable:"true"`
	From            string    `db:"from_date" json:"from" format:"date"`
	To              string    `db:"to_date" json:"to" format:"date" doc:"Exclusiva, como la salida de la reserva."`
}

// AssignmentCreate es el cuerpo de POST /parking/assignments.
type AssignmentCreate struct {
	_               struct{}  `additionalProperties:"true"`
	SpaceID         uuid.UUID `json:"space_id" format:"uuid"`
	ReservationCode string    `json:"reservation_code" maxLength:"16" doc:"Codigo de la reserva (ARG-XXXXXX)."`
	Plate           string    `json:"plate" maxLength:"16"`
	Description     *string   `json:"vehicle_description,omitempty" maxLength:"128"`
}

// StayRead es una estancia que puede recibir parqueo.
type StayRead struct {
	ReservationID   uuid.UUID `db:"id" json:"reservation_id" format:"uuid"`
	ReservationCode string    `db:"code" json:"reservation_code"`
	GuestName       string    `db:"guest_name" json:"guest_name"`
	PropertyCode    string    `db:"property_code" json:"property_code"`
	Building        string    `db:"building" json:"building"`
	From            string    `db:"check_in_date" json:"from" format:"date"`
	To              string    `db:"check_out_date" json:"to" format:"date"`
}

// Service contiene los casos de uso de parqueos.
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

// DB es la base del servicio (semilla).
func (s *Service) DB() *database.DB { return s.db }

const spaceSelect = `
	SELECT s.id, s.code, coalesce(b.name, p.name, '') AS building, s.building_id, s.property_id, s.level, s.kind,
	       s.covered, s.is_blocked, s.is_active
	FROM parking_spots s
	LEFT JOIN buildings b ON b.id = s.building_id
	LEFT JOIN properties p ON p.id = s.property_id`

const assignmentSelect = `
	SELECT a.id, a.spot_id, s.code AS space_code, coalesce(b.name, sp.name, '') AS building, s.level, s.covered,
	       a.reservation_id, r.code AS reservation_code, coalesce(g.full_name, r.external_summary, 'Huésped') AS guest_name,
	       p.code AS property_code, a.vehicle_plate, a.vehicle_description,
	       to_char(a.from_date, 'YYYY-MM-DD') AS from_date, to_char(a.to_date, 'YYYY-MM-DD') AS to_date
	FROM parking_assignments a
	JOIN parking_spots s ON s.id = a.spot_id
	LEFT JOIN buildings b ON b.id = s.building_id
	LEFT JOIN properties sp ON sp.id = s.property_id
	JOIN reservations r ON r.id = a.reservation_id
	JOIN properties p ON p.id = r.property_id
	LEFT JOIN guests g ON g.id = r.primary_guest_id`

// Spaces lista los espacios activos (y los inactivos con all).
func (s *Service) Spaces(ctx context.Context, all bool) ([]SpaceRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]SpaceRead, error) {
		rows, err := q.Query(ctx, spaceSelect+` WHERE s.is_active OR $1 ORDER BY 3, s.level, s.code`, all)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[SpaceRead])
	})
}

func spaceByID(ctx context.Context, q database.Querier, id uuid.UUID) (SpaceRead, error) {
	rows, err := q.Query(ctx, spaceSelect+` WHERE s.id = $1`, id)
	if err != nil {
		return SpaceRead{}, err
	}
	sp, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[SpaceRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return SpaceRead{}, ErrSpaceNotFound
	}
	return sp, err
}

// CreateSpace da de alta un espacio en un edificio o una propiedad.
func (s *Service) CreateSpace(ctx context.Context, actor authz.Actor, in SpaceCreate) (SpaceRead, error) {
	if (in.BuildingID == nil) == (in.PropertyID == nil) {
		return SpaceRead{}, ErrPlace.WithField("body.building_id")
	}
	kind := in.Kind
	if kind == "" {
		kind = "guest"
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (SpaceRead, error) {
		var ok bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM buildings WHERE id = $1) OR EXISTS (SELECT 1 FROM properties WHERE id = $2 AND deleted_at IS NULL)`,
			in.BuildingID, in.PropertyID).Scan(&ok); err != nil {
			return SpaceRead{}, err
		}
		if !ok {
			return SpaceRead{}, ErrUnknownPlace.WithField("body.building_id")
		}
		var id uuid.UUID
		err := q.QueryRow(ctx, `
			INSERT INTO parking_spots (building_id, property_id, code, level, kind, covered)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			in.BuildingID, in.PropertyID, strings.ToUpper(strings.TrimSpace(in.Code)), strings.TrimSpace(in.Level), kind, in.Covered).Scan(&id)
		if uniqueViolation(err) {
			return SpaceRead{}, ErrCodeTaken.WithField("body.code")
		}
		if err != nil {
			return SpaceRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{Actor: &actor.UserID, Action: "parking.space_created", Entity: "parking_spot", EntityID: &id,
			After: map[string]any{"code": in.Code}}); err != nil {
			return SpaceRead{}, err
		}
		return spaceByID(ctx, q, id)
	})
}

// UpdateSpace corrige un espacio, lo bloquea o lo desactiva.
func (s *Service) UpdateSpace(ctx context.Context, actor authz.Actor, id uuid.UUID, in SpaceUpdate) (SpaceRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (SpaceRead, error) {
		cur, err := spaceByID(ctx, q, id)
		if err != nil {
			return SpaceRead{}, err
		}
		if in.Code != nil {
			cur.Code = strings.ToUpper(strings.TrimSpace(*in.Code))
		}
		if in.Level != nil {
			cur.Level = strings.TrimSpace(*in.Level)
		}
		if in.Kind != nil {
			cur.Kind = *in.Kind
		}
		if in.Covered != nil {
			cur.Covered = *in.Covered
		}
		if in.Blocked != nil {
			cur.Blocked = *in.Blocked
		}
		if in.Active != nil {
			cur.Active = *in.Active
		}
		_, err = q.Exec(ctx, `
			UPDATE parking_spots SET code = $2, level = $3, kind = $4, covered = $5, is_blocked = $6, is_active = $7, updated_at = now()
			WHERE id = $1`, id, cur.Code, cur.Level, cur.Kind, cur.Covered, cur.Blocked, cur.Active)
		if uniqueViolation(err) {
			return SpaceRead{}, ErrCodeTaken.WithField("body.code")
		}
		if err != nil {
			return SpaceRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{Actor: &actor.UserID, Action: "parking.space_updated", Entity: "parking_spot", EntityID: &id,
			After: map[string]any{"code": cur.Code, "blocked": cur.Blocked, "active": cur.Active}}); err != nil {
			return SpaceRead{}, err
		}
		return spaceByID(ctx, q, id)
	})
}

// Assignments lista lo asignado que se cruza con [from, to].
func (s *Service) Assignments(ctx context.Context, from, to string) ([]AssignmentRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]AssignmentRead, error) {
		return Overlapping(ctx, q, from, to)
	})
}

// Overlapping son las asignaciones que tocan los dias de from a to (inclusive).
func Overlapping(ctx context.Context, q database.Querier, from, to string) ([]AssignmentRead, error) {
	rows, err := q.Query(ctx, assignmentSelect+`
		WHERE a.from_date <= $2::date AND a.to_date > $1::date ORDER BY s.code, a.from_date`, from, to)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[AssignmentRead])
}

// ForReservation son los espacios de una reserva (lo que ve el huesped).
func ForReservation(ctx context.Context, q database.Querier, reservationID uuid.UUID) ([]AssignmentRead, error) {
	rows, err := q.Query(ctx, assignmentSelect+` WHERE a.reservation_id = $1 ORDER BY a.from_date`, reservationID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[AssignmentRead])
}

// Stays son las reservas confirmadas o en casa de los proximos 14 dias que
// aun no tienen parqueo.
func (s *Service) Stays(ctx context.Context) ([]StayRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]StayRead, error) {
		today, err := Today(ctx, q, s.now())
		if err != nil {
			return nil, err
		}
		rows, err := q.Query(ctx, `
			SELECT r.id, r.code, coalesce(g.full_name, r.external_summary, 'Huésped') AS guest_name, p.code AS property_code,
			       coalesce(b.name, p.name) AS building,
			       to_char(r.check_in_date, 'YYYY-MM-DD') AS check_in_date, to_char(r.check_out_date, 'YYYY-MM-DD') AS check_out_date
			FROM reservations r
			JOIN properties p ON p.id = r.property_id
			LEFT JOIN buildings b ON b.id = p.building_id
			LEFT JOIN guests g ON g.id = r.primary_guest_id
			WHERE r.status IN ('confirmed', 'checked_in') AND r.check_out_date > $1::date AND r.check_in_date <= $1::date + 14
			  AND NOT EXISTS (SELECT 1 FROM parking_assignments a WHERE a.reservation_id = r.id)
			ORDER BY r.check_in_date, r.code`, today)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[StayRead])
	})
}

var plateRe = regexp.MustCompile(`^[A-Z0-9-]{5,10}$`)

// NormalizePlate deja la placa en mayusculas y sin espacios.
func NormalizePlate(p string) string {
	return strings.Join(strings.Fields(strings.ToUpper(p)), "")
}

// Assign da un espacio a una reserva por las fechas de su estancia.
func (s *Service) Assign(ctx context.Context, actor authz.Actor, in AssignmentCreate) (AssignmentRead, error) {
	plate := NormalizePlate(in.Plate)
	if !plateRe.MatchString(plate) {
		return AssignmentRead{}, ErrPlate.WithField("body.plate")
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (AssignmentRead, error) {
		space, err := spaceByID(ctx, q, in.SpaceID)
		if errors.Is(err, ErrSpaceNotFound) {
			return AssignmentRead{}, ErrSpaceNotFound.WithField("body.space_id")
		}
		if err != nil {
			return AssignmentRead{}, err
		}
		if space.Blocked || !space.Active {
			return AssignmentRead{}, ErrBlocked.WithField("body.space_id")
		}
		var rid, propID uuid.UUID
		var buildingID *uuid.UUID
		var from, to string
		err = q.QueryRow(ctx, `
			SELECT r.id, r.property_id, p.building_id, to_char(r.check_in_date, 'YYYY-MM-DD'), to_char(r.check_out_date, 'YYYY-MM-DD')
			FROM reservations r JOIN properties p ON p.id = r.property_id
			WHERE upper(r.code) = upper($1) AND r.status IN ('pending', 'confirmed', 'checked_in')`,
			strings.TrimSpace(in.ReservationCode)).Scan(&rid, &propID, &buildingID, &from, &to)
		if errors.Is(err, pgx.ErrNoRows) {
			return AssignmentRead{}, ErrReservation.WithField("body.reservation_code")
		}
		if err != nil {
			return AssignmentRead{}, err
		}
		if (space.BuildingID != nil && (buildingID == nil || *buildingID != *space.BuildingID)) ||
			(space.PropertyID != nil && *space.PropertyID != propID) {
			return AssignmentRead{}, ErrOtherPlace.WithField("body.space_id")
		}
		if err := clash(ctx, q, space.ID, from, to, nil); err != nil {
			return AssignmentRead{}, err
		}
		var id uuid.UUID
		err = q.QueryRow(ctx, `
			INSERT INTO parking_assignments (spot_id, reservation_id, vehicle_plate, vehicle_description, from_date, to_date, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			space.ID, rid, plate, in.Description, from, to, actor.UserID).Scan(&id)
		if exclusionViolation(err) {
			return AssignmentRead{}, ErrTaken.WithField("body.space_id")
		}
		if err != nil {
			return AssignmentRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{Actor: &actor.UserID, Action: "parking.assigned", Entity: "parking_assignment", EntityID: &id,
			PropertyID: &propID, After: map[string]any{"space": space.Code, "plate": plate, "from": from, "to": to}}); err != nil {
			return AssignmentRead{}, err
		}
		return assignmentByID(ctx, q, id)
	})
}

// clash devuelve 409 con quien ocupa el espacio si se cruza con [from, to).
func clash(ctx context.Context, q database.Querier, spotID uuid.UUID, from, to string, except *uuid.UUID) error {
	var plate, name string
	err := q.QueryRow(ctx, `
		SELECT a.vehicle_plate, coalesce(g.full_name, r.code)
		FROM parking_assignments a JOIN reservations r ON r.id = a.reservation_id LEFT JOIN guests g ON g.id = r.primary_guest_id
		WHERE a.spot_id = $1 AND a.from_date < $3::date AND $2::date < a.to_date AND ($4::uuid IS NULL OR a.id <> $4)
		LIMIT 1`, spotID, from, to, except).Scan(&plate, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrTaken.WithMessage(fmt.Sprintf("Ocupado por %s (%s) en esas fechas.", plate, name)).WithField("body.space_id")
}

func assignmentByID(ctx context.Context, q database.Querier, id uuid.UUID) (AssignmentRead, error) {
	rows, err := q.Query(ctx, assignmentSelect+` WHERE a.id = $1`, id)
	if err != nil {
		return AssignmentRead{}, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[AssignmentRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return AssignmentRead{}, ErrAssignmentNotFound
	}
	return a, err
}

// Release libera un espacio asignado.
func (s *Service) Release(ctx context.Context, actor authz.Actor, id uuid.UUID) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		a, err := assignmentByID(ctx, q, id)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `DELETE FROM parking_assignments WHERE id = $1`, id); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{Actor: &actor.UserID, Action: "parking.released", Entity: "parking_assignment", EntityID: &id,
			Before: map[string]any{"space": a.SpaceCode, "plate": a.Plate, "reservation": a.ReservationCode}})
	})
}

// Subscribe engancha los cambios de reservas.
func (s *Service) Subscribe(bus *events.Bus) {
	bus.Subscribe(reservations.EventCancelled, s.onCancelled)
	bus.Subscribe(reservations.EventUpdated, s.onUpdated)
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

func (s *Service) onCancelled(ctx context.Context, q database.Querier, e events.Event) error {
	ev, ok := reservationOf(e)
	if !ok {
		return nil
	}
	_, err := q.Exec(ctx, `DELETE FROM parking_assignments WHERE reservation_id = $1`, ev.Reservation.ID)
	return err
}

// onUpdated sigue las fechas nuevas de la estancia; si el espacio ya no esta
// libre en ellas, lo libera (la web lo muestra para asignar otro).
func (s *Service) onUpdated(ctx context.Context, q database.Querier, e events.Event) error {
	ev, ok := reservationOf(e)
	if !ok {
		return nil
	}
	r := ev.Reservation
	if ev.Previous != nil && ev.Previous.CheckInDate == r.CheckInDate && ev.Previous.CheckOutDate == r.CheckOutDate {
		return nil
	}
	rows, err := q.Query(ctx, `SELECT id, spot_id FROM parking_assignments WHERE reservation_id = $1`, r.ID)
	if err != nil {
		return err
	}
	type row struct {
		ID, Spot uuid.UUID
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		return err
	}
	for _, a := range list {
		err := clash(ctx, q, a.Spot, r.CheckInDate, r.CheckOutDate, &a.ID)
		switch {
		case err == nil:
			_, err = q.Exec(ctx, `UPDATE parking_assignments SET from_date = $2, to_date = $3 WHERE id = $1`, a.ID, r.CheckInDate, r.CheckOutDate)
		case isTaken(err):
			_, err = q.Exec(ctx, `DELETE FROM parking_assignments WHERE id = $1`, a.ID)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Today es la fecha local de la empresa del schema activo.
func Today(ctx context.Context, q database.Querier, now time.Time) (string, error) {
	var today string
	err := q.QueryRow(ctx, `
		SELECT to_char(($1::timestamptz AT TIME ZONE timezone)::date, 'YYYY-MM-DD')
		FROM shared.tenants WHERE schema_name = current_schema()`, now).Scan(&today)
	return today, err
}

func isTaken(err error) bool {
	var ae *apperr.Error
	return errors.As(err, &ae) && ae.Kind == apperr.KindAlreadyExists
}

func uniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func exclusionViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23P01"
}
