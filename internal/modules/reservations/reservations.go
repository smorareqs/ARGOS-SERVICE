package reservations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/optional"
	"github.com/smorareqs/argos-service/internal/core/paging"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// registeredSQL indica si el huesped (alias g) tiene su registro completo:
// correo, documento y las dos fotos vigentes.
const registeredSQL = `(g.email IS NOT NULL AND g.document_number IS NOT NULL
	AND EXISTS (SELECT 1 FROM files f WHERE f.entity_type = 'guest' AND f.entity_id = g.id AND f.kind = 'id_front'
	            AND f.status = 'ready' AND f.deleted_at IS NULL)
	AND EXISTS (SELECT 1 FROM files f WHERE f.entity_type = 'guest' AND f.entity_id = g.id AND f.kind = 'id_back'
	            AND f.status = 'ready' AND f.deleted_at IS NULL))`

const selectReservation = `
	SELECT r.id, r.code, r.property_id, p.code AS property_code, p.name AS property_name,
	       r.channel_id, c.name AS channel_name, c.kind AS channel_kind, c.requires_deposit, c.has_checkin,
	       r.source, r.external_ref, r.external_summary, r.primary_guest_id,
	       pg.full_name AS guest_name, pg.email AS guest_email,
	       (SELECT count(*)::int FROM reservation_guests rg WHERE rg.reservation_id = r.id) AS guests_count,
	       (SELECT count(*)::int FROM reservation_guests rg JOIN guests g ON g.id = rg.guest_id
	         WHERE rg.reservation_id = r.id AND NOT ` + registeredSQL + `) AS guests_pending,
	       r.status, (r.primary_guest_id IS NULL AND r.status NOT IN ('cancelled', 'no_show')) AS incomplete,
	       to_char(r.check_in_date, 'YYYY-MM-DD') AS check_in_date, to_char(r.check_out_date, 'YYYY-MM-DD') AS check_out_date,
	       to_char(r.check_in_time, 'HH24:MI') AS check_in_time, to_char(r.check_out_time, 'HH24:MI') AS check_out_time,
	       (r.check_out_date - r.check_in_date) AS nights, r.checked_in_at, r.checked_out_at,
	       r.adults::int AS adults, r.children::int AS children, r.total_amount::float8 AS total_amount,
	       trim(r.currency) AS currency, r.deposit_amount::float8 AS deposit_amount, r.deposit_status,
	       r.notes, r.cancelled_at, r.cancel_reason, r.created_at, r.updated_at, r.manual_fields
	FROM reservations r
	JOIN properties p ON p.id = r.property_id
	JOIN booking_channels c ON c.id = r.channel_id
	LEFT JOIN guests pg ON pg.id = r.primary_guest_id`

// Reservations lista las reservas del alcance. Con from/to trae las estancias
// que tocan ese rango (calendario W-H1); sin ellos, todas.
func (s *Service) Reservations(ctx context.Context, actor authz.Actor, f ReservationFilter) ([]ReservationRead, *string, error) {
	limit := paging.Limit(f.Limit)
	key, err := paging.Decode(f.Cursor, 2)
	if err != nil {
		return nil, nil, err
	}
	var afterDate, afterID *string
	if key != nil {
		afterDate, afterID = &key[0], &key[1]
	}
	for _, d := range []string{f.From, f.To} {
		if d != "" && !validDate(d) {
			return nil, nil, ErrInvalidDate
		}
	}
	order, cmp := "ASC", ">"
	if f.Desc {
		order, cmp = "DESC", "<"
	}
	items, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]ReservationRead, error) {
		t, err := s.tenant(ctx, q)
		if err != nil {
			return nil, err
		}
		v, err := s.visibility(ctx, q, actor, t.Today)
		if err != nil {
			return nil, err
		}
		search := ""
		if term := strings.ToLower(strings.TrimSpace(f.Query)); term != "" {
			search = "%" + term + "%"
		}
		args := append(v.args(), nullDate(f.From), nullDate(f.To), f.PropertyID, f.ChannelID, f.GuestID,
			f.Statuses, f.Incomplete, search, afterDate, afterID, limit+1)
		rows, err := q.Query(ctx, selectReservation+`
			WHERE `+visibleSQL+`
			  AND ($5::date IS NULL OR r.check_out_date > $5::date)
			  AND ($6::date IS NULL OR r.check_in_date < $6::date)
			  AND ($7::uuid IS NULL OR r.property_id = $7)
			  AND ($8::uuid IS NULL OR r.channel_id = $8)
			  AND ($9::uuid IS NULL OR r.primary_guest_id = $9
			       OR EXISTS (SELECT 1 FROM reservation_guests x WHERE x.reservation_id = r.id AND x.guest_id = $9))
			  AND (coalesce(cardinality($10::text[]), 0) = 0 OR r.status = ANY($10::text[]))
			  AND (NOT $11::bool OR (r.primary_guest_id IS NULL AND r.status NOT IN ('cancelled', 'no_show')))
			  AND ($12::text = '' OR lower(r.code) LIKE $12 OR lower(p.code) LIKE $12 OR lower(p.name) LIKE $12
			       OR lower(coalesce(pg.full_name, '')) LIKE $12 OR lower(coalesce(r.external_summary, '')) LIKE $12)
			  AND ($13::date IS NULL OR (r.check_in_date, r.id) `+cmp+` ($13::date, $14::uuid))
			ORDER BY r.check_in_date `+order+`, r.id `+order+`
			LIMIT $15`, args...)
		if err != nil {
			return nil, err
		}
		list, err := pgx.CollectRows(rows, pgx.RowToStructByName[ReservationRead])
		return redactList(list, v.contact), err
	})
	if err != nil {
		return nil, nil, err
	}
	items, next := paging.Page(items, limit, func(r ReservationRead) []string {
		return []string{r.CheckInDate, r.ID.String()}
	})
	return items, next, nil
}

// Reservation devuelve la ficha (404 fuera de alcance).
func (s *Service) Reservation(ctx context.Context, actor authz.Actor, id uuid.UUID) (ReservationDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (ReservationDetail, error) {
		r, v, err := s.visible(ctx, q, actor, id)
		if err != nil {
			return ReservationDetail{}, err
		}
		return s.detail(ctx, q, actor, v, r)
	})
}

// visible lee una reserva del alcance del actor.
func (s *Service) visible(ctx context.Context, q database.Querier, actor authz.Actor, id uuid.UUID) (ReservationRead, visibility, error) {
	t, err := s.tenant(ctx, q)
	if err != nil {
		return ReservationRead{}, visibility{}, err
	}
	v, err := s.visibility(ctx, q, actor, t.Today)
	if err != nil {
		return ReservationRead{}, visibility{}, err
	}
	rows, err := q.Query(ctx, selectReservation+` WHERE `+visibleSQL+` AND r.id = $5`, append(v.args(), id)...)
	if err != nil {
		return ReservationRead{}, v, err
	}
	r, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[ReservationRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return ReservationRead{}, v, ErrReservationNotFound
	}
	return redact(r, v.contact), v, err
}

func (s *Service) detail(ctx context.Context, q database.Querier, actor authz.Actor, v visibility, r ReservationRead) (ReservationDetail, error) {
	out := ReservationDetail{ReservationRead: r, Deposit: DepositRead{Status: r.DepositStatus, Transactions: []DepositTx{}}}
	var err error
	if out.Guests, err = reservationGuests(ctx, q, r.ID, v.contact); err != nil {
		return out, err
	}
	if out.Events, err = reservationEvents(ctx, q, r.ID); err != nil {
		return out, err
	}
	if actor.Permissions[authz.DepositsRead] {
		if out.Deposit, err = depositOf(ctx, q, r); err != nil {
			return out, err
		}
	}
	out.Inspections, err = s.inspections(ctx, q, actor.TenantID, r.ID)
	return out, err
}

// reservationByID lee una reserva sin filtro de alcance (uso interno).
func reservationByID(ctx context.Context, q database.Querier, id uuid.UUID, lock bool) (ReservationRead, error) {
	if lock {
		if _, err := q.Exec(ctx, `SELECT 1 FROM reservations WHERE id = $1 FOR UPDATE`, id); err != nil {
			return ReservationRead{}, err
		}
	}
	rows, err := q.Query(ctx, selectReservation+` WHERE r.id = $1`, id)
	if err != nil {
		return ReservationRead{}, err
	}
	r, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[ReservationRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return ReservationRead{}, ErrReservationNotFound
	}
	return r, err
}

func redact(r ReservationRead, contact bool) ReservationRead {
	if !contact {
		r.GuestEmail = nil
	}
	return r
}

func redactList(list []ReservationRead, contact bool) []ReservationRead {
	for i := range list {
		list[i] = redact(list[i], contact)
	}
	return list
}

func nullDate(d string) *string {
	if d == "" {
		return nil
	}
	return &d
}

// --- Alta ------------------------------------------------------------------------

// Create da de alta una reserva manual (W-H4). Con huesped principal queda
// confirmada (y avisa al huesped si send_email); sin el, por confirmar.
func (s *Service) Create(ctx context.Context, actor authz.Actor, in ReservationCreate) (ReservationDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (ReservationDetail, error) {
		t, err := s.tenant(ctx, q)
		if err != nil {
			return ReservationDetail{}, err
		}
		row, err := s.newRow(ctx, q, actor, t, in)
		if err != nil {
			return ReservationDetail{}, err
		}
		r, err := s.insertWithGuest(ctx, q, actor, row)
		if err != nil {
			return ReservationDetail{}, err
		}
		id := r.ID
		if r.Status == "confirmed" {
			if r, err = s.confirmed(ctx, q, actor, t, r, in.SendEmail == nil || *in.SendEmail); err != nil {
				return ReservationDetail{}, err
			}
		}
		_, v, err := s.visible(ctx, q, actor, id)
		if err != nil {
			return ReservationDetail{}, err
		}
		return s.detail(ctx, q, actor, v, r)
	})
}

// insertWithGuest inserta la reserva con su huesped principal y registra el
// alta (linea de tiempo y bitacora).
func (s *Service) insertWithGuest(ctx context.Context, q database.Querier, actor authz.Actor, row row) (ReservationRead, error) {
	id, err := insertReservation(ctx, q, row)
	if err != nil {
		return ReservationRead{}, err
	}
	if row.guestID != nil {
		if _, err := q.Exec(ctx, `INSERT INTO reservation_guests (reservation_id, guest_id, is_primary) VALUES ($1, $2, true)`,
			id, row.guestID); err != nil {
			return ReservationRead{}, err
		}
		if err := refreshDocumentExpiry(ctx, q, *row.guestID); err != nil {
			return ReservationRead{}, err
		}
	}
	if err := addEvent(ctx, q, id, "created", actorID(actor), map[string]any{"source": row.source}); err != nil {
		return ReservationRead{}, err
	}
	r, err := reservationByID(ctx, q, id, false)
	if err != nil {
		return ReservationRead{}, err
	}
	return r, audit.Record(ctx, q, audit.Entry{
		Actor: actorID(actor), Action: "reservation.created", Entity: "reservation", EntityID: &id, PropertyID: &r.PropertyID,
		After: map[string]any{"code": r.Code, "check_in": r.CheckInDate, "check_out": r.CheckOutDate, "channel": r.ChannelName},
	})
}

// row son los valores de una reserva nueva ya validados.
type row struct {
	propertyID, channelID     uuid.UUID
	guestID                   *uuid.UUID
	checkIn, checkOut         string
	checkInTime, checkOutTime string
	adults, children          int
	total, deposit            *float64
	depositStatus, status     string
	source                    string
	externalRef, summary      *string
	notes                     *string
	currency                  string
	createdBy                 *uuid.UUID
}

func (s *Service) newRow(ctx context.Context, q database.Querier, actor authz.Actor, t tenantInfo, in ReservationCreate) (row, error) {
	r := row{
		propertyID: in.PropertyID, channelID: in.ChannelID, checkIn: in.CheckInDate, checkOut: in.CheckOutDate,
		checkInTime: t.CheckinTime, checkOutTime: t.CheckoutTime, adults: 1, children: in.Children,
		total: in.TotalAmount, source: "manual", externalRef: trimmed(in.ExternalRef), notes: trimmed(in.Notes),
		currency: t.Currency, createdBy: actorID(actor),
	}
	if in.Adults != nil {
		r.adults = *in.Adults
	}
	if err := r.setTimes(in.CheckInTime, in.CheckOutTime); err != nil {
		return r, err
	}
	if err := validStay(r.checkIn, r.checkOut); err != nil {
		return r, err
	}
	if ok, err := propertyInScope(ctx, q, actor, r.propertyID); err != nil || !ok {
		return r, errOr(err, ErrPropertyNotFound)
	}
	ch, err := activeChannel(ctx, q, r.channelID, "body.channel_id")
	if err != nil {
		return r, err
	}
	r.deposit = in.DepositAmount
	if r.deposit == nil && ch.RequiresDeposit {
		r.deposit = ch.DefaultDepositAmount
	}
	r.depositStatus = depositStatusFor(r.deposit)
	if r.guestID, err = s.resolveGuest(ctx, q, actor, in.PrimaryGuestID, in.Guest); err != nil {
		return r, err
	}
	r.status = "pending"
	if r.guestID != nil {
		r.status = "confirmed"
	}
	return r, overlapCheck(ctx, q, r.propertyID, r.checkIn, r.checkOut, nil)
}

func (r *row) setTimes(in, out *string) error {
	if in != nil {
		if !validClock(*in) {
			return ErrInvalidTime.WithField("body.check_in_time")
		}
		r.checkInTime = *in
	}
	if out != nil {
		if !validClock(*out) {
			return ErrInvalidTime.WithField("body.check_out_time")
		}
		r.checkOutTime = *out
	}
	return nil
}

func validStay(in, out string) error {
	if !validDate(in) {
		return ErrInvalidDate.WithField("body.check_in_date")
	}
	if !validDate(out) {
		return ErrInvalidDate.WithField("body.check_out_date")
	}
	if out <= in {
		return ErrDatesOrder.WithField("body.check_out_date")
	}
	return nil
}

func depositStatusFor(amount *float64) string {
	if amount != nil && *amount > 0 {
		return "pending"
	}
	return "none"
}

func errOr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

// insertReservation inserta con un codigo nuevo (reintenta si se repite).
func insertReservation(ctx context.Context, q database.Querier, r row) (uuid.UUID, error) {
	for range 5 {
		code, err := newCode()
		if err != nil {
			return uuid.Nil, err
		}
		var taken bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM reservations WHERE code = $1)`, code).Scan(&taken); err != nil {
			return uuid.Nil, err
		}
		if taken {
			continue
		}
		var id uuid.UUID
		err = q.QueryRow(ctx, `
			INSERT INTO reservations (property_id, channel_id, external_ref, external_summary, code, primary_guest_id, status,
			       check_in_date, check_out_date, check_in_time, check_out_time, adults, children, total_amount, currency,
			       deposit_amount, deposit_status, source, notes, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20)
			RETURNING id`,
			r.propertyID, r.channelID, r.externalRef, r.summary, code, r.guestID, r.status, r.checkIn, r.checkOut,
			r.checkInTime, r.checkOutTime, r.adults, r.children, r.total, r.currency, r.deposit, r.depositStatus,
			r.source, r.notes, r.createdBy).Scan(&id)
		if overlapViolation(err) {
			return uuid.Nil, ErrOverlapGeneric.WithField("body.check_in_date")
		}
		return id, err
	}
	return uuid.Nil, fmt.Errorf("no se pudo generar un codigo de reserva unico")
}

// overlapCheck explica con que reserva se cruzan las fechas (la base tambien
// lo impide con ex_reservations_overlap).
func overlapCheck(ctx context.Context, q database.Querier, propertyID uuid.UUID, in, out string, except *uuid.UUID) error {
	msg, err := overlapWith(ctx, q, propertyID, in, out, except)
	if err != nil || msg == "" {
		return err
	}
	return ErrOverlapGeneric.WithMessage("Se cruza con " + msg + ".").WithField("body.check_in_date")
}

// overlapWith describe la primera reserva viva que se cruza ("ARG-7K3P9Q de
// Ana Lopez, 5 dic al 8 dic"); vacio si ninguna.
func overlapWith(ctx context.Context, q database.Querier, propertyID uuid.UUID, in, out string, except *uuid.UUID) (string, error) {
	var code, from, to string
	var guest *string
	err := q.QueryRow(ctx, `
		SELECT r.code, to_char(r.check_in_date, 'YYYY-MM-DD'), to_char(r.check_out_date, 'YYYY-MM-DD'), g.full_name
		FROM reservations r LEFT JOIN guests g ON g.id = r.primary_guest_id
		WHERE r.property_id = $1 AND r.status NOT IN ('cancelled', 'no_show')
		  AND daterange(r.check_in_date, r.check_out_date) && daterange($2::date, $3::date)
		  AND ($4::uuid IS NULL OR r.id <> $4)
		ORDER BY r.check_in_date LIMIT 1`, propertyID, in, out, except).Scan(&code, &from, &to, &guest)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if guest != nil {
		code += " de " + *guest
	}
	return fmt.Sprintf("%s (%s al %s)", code, shortDate(from), shortDate(to)), nil
}

var monthsES = [...]string{"ene", "feb", "mar", "abr", "may", "jun", "jul", "ago", "sep", "oct", "nov", "dic"}

// shortDate convierte "2026-12-05" en "5 dic".
func shortDate(day string) string {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day
	}
	return fmt.Sprintf("%d %s", t.Day(), monthsES[t.Month()-1])
}

// propertyInScope indica si la propiedad existe, esta activa y es del alcance.
func propertyInScope(ctx context.Context, q database.Querier, actor authz.Actor, id uuid.UUID) (bool, error) {
	scope, err := authz.ResolvePropertyScope(ctx, q, actor)
	if err != nil {
		return false, err
	}
	all, ids := scope.SQLArgs()
	var ok bool
	err = q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM properties WHERE id = $1 AND deleted_at IS NULL AND is_active
		               AND ($2::bool OR id = ANY($3::uuid[])))`, id, all, ids).Scan(&ok)
	return ok, err
}

// --- Edicion -----------------------------------------------------------------------

// Update corrige fechas, horas, canal, personas, montos o notas. Las fechas
// solo cambian antes del check-in (la salida tambien con el huesped en casa).
func (s *Service) Update(ctx context.Context, actor authz.Actor, id uuid.UUID, in ReservationUpdate) (ReservationDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (ReservationDetail, error) {
		cur, v, err := s.visible(ctx, q, actor, id)
		if err != nil {
			return ReservationDetail{}, err
		}
		if cur.Status == "cancelled" || cur.Status == "no_show" {
			return ReservationDetail{}, ErrTransition
		}
		next, fields, err := mergeReservation(cur, in)
		if err != nil {
			return ReservationDetail{}, err
		}
		if len(fields) == 0 {
			return s.detail(ctx, q, actor, v, cur)
		}
		if err := s.checkUpdate(ctx, q, cur, next, fields); err != nil {
			return ReservationDetail{}, err
		}
		if err := writeUpdate(ctx, q, cur, next, fields); err != nil {
			return ReservationDetail{}, err
		}
		r, err := s.afterUpdate(ctx, q, actor, cur, fields)
		if err != nil {
			return ReservationDetail{}, err
		}
		return s.detail(ctx, q, actor, v, redact(r, v.contact))
	})
}

// writeUpdate guarda los cambios. En una reserva importada, mover las fechas a
// mano las protege de la sincronizacion iCal (manual_fields).
func writeUpdate(ctx context.Context, q database.Querier, cur, next ReservationRead, fields []string) error {
	manual := cur.ManualFields
	if cur.Source == "ical" && slices.Contains(fields, "dates") && !slices.Contains(manual, "dates") {
		manual = append(manual, "dates")
	}
	_, err := q.Exec(ctx, `
		UPDATE reservations SET channel_id = $2, check_in_date = $3, check_out_date = $4, check_in_time = $5,
		       check_out_time = $6, adults = $7, children = $8, total_amount = $9, deposit_amount = $10,
		       deposit_status = $11, notes = $12, manual_fields = $13, updated_at = now()
		WHERE id = $1`, cur.ID, next.ChannelID, next.CheckInDate, next.CheckOutDate, next.CheckInTime, next.CheckOutTime,
		next.Adults, next.Children, next.TotalAmount, next.DepositAmount, next.DepositStatus, next.Notes, manual)
	if overlapViolation(err) {
		return ErrOverlapGeneric.WithField("body.check_in_date")
	}
	return err
}

func (s *Service) checkUpdate(ctx context.Context, q database.Querier, cur, next ReservationRead, fields []string) error {
	if slices.Contains(fields, "dates") {
		if err := validStay(next.CheckInDate, next.CheckOutDate); err != nil {
			return err
		}
		datesLocked := cur.Status == "checked_out" || (cur.Status == "checked_in" && next.CheckInDate != cur.CheckInDate)
		if datesLocked {
			return ErrTransition.WithField("body.check_in_date")
		}
		if err := overlapCheck(ctx, q, cur.PropertyID, next.CheckInDate, next.CheckOutDate, &cur.ID); err != nil {
			return err
		}
	}
	if slices.Contains(fields, "channel_id") {
		if _, err := activeChannel(ctx, q, next.ChannelID, "body.channel_id"); err != nil {
			return err
		}
	}
	return nil
}

// afterUpdate registra el cambio y avisa a B si movio la estancia.
func (s *Service) afterUpdate(ctx context.Context, q database.Querier, actor authz.Actor, cur ReservationRead, fields []string) (ReservationRead, error) {
	if err := addEvent(ctx, q, cur.ID, "updated", actorID(actor), map[string]any{"fields": fields}); err != nil {
		return ReservationRead{}, err
	}
	r, err := reservationByID(ctx, q, cur.ID, false)
	if err != nil {
		return ReservationRead{}, err
	}
	if err := audit.Record(ctx, q, audit.Entry{
		Actor: actorID(actor), Action: "reservation.updated", Entity: "reservation", EntityID: &r.ID, PropertyID: &r.PropertyID,
		Before: map[string]any{"check_in": cur.CheckInDate, "check_out": cur.CheckOutDate},
		After:  map[string]any{"fields": fields, "check_in": r.CheckInDate, "check_out": r.CheckOutDate},
	}); err != nil {
		return ReservationRead{}, err
	}
	moved := slices.ContainsFunc(fields, func(f string) bool { return f == "dates" || f == "times" || f == "channel_id" })
	if !moved || (r.Status != "confirmed" && r.Status != "checked_in") {
		return r, nil
	}
	if r.CheckInDate != cur.CheckInDate {
		if err := s.scheduleReminder(ctx, q, actor.TenantID, r); err != nil {
			return ReservationRead{}, err
		}
	}
	prev := cur
	return r, s.bus.Publish(ctx, q, events.Event{
		Type: EventUpdated, TenantID: actor.TenantID, ActorID: actorID(actor),
		Data: ReservationEvent{Reservation: r, Previous: &prev},
	})
}

func mergeReservation(cur ReservationRead, in ReservationUpdate) (ReservationRead, []string, error) {
	fields := []string{}
	if in.ChannelID != nil && *in.ChannelID != cur.ChannelID {
		cur.ChannelID, fields = *in.ChannelID, append(fields, "channel_id")
	}
	if (in.CheckInDate != nil && *in.CheckInDate != cur.CheckInDate) || (in.CheckOutDate != nil && *in.CheckOutDate != cur.CheckOutDate) {
		if in.CheckInDate != nil {
			cur.CheckInDate = *in.CheckInDate
		}
		if in.CheckOutDate != nil {
			cur.CheckOutDate = *in.CheckOutDate
		}
		fields = append(fields, "dates")
	}
	timesChanged, err := mergeTimes(&cur, in)
	if err != nil {
		return cur, nil, err
	}
	if timesChanged {
		fields = append(fields, "times")
	}
	fields = append(fields, mergeCounts(&cur, in)...)
	return cur, fields, nil
}

func mergeTimes(cur *ReservationRead, in ReservationUpdate) (bool, error) {
	changed := false
	if in.CheckInTime != nil && *in.CheckInTime != cur.CheckInTime {
		if !validClock(*in.CheckInTime) {
			return false, ErrInvalidTime.WithField("body.check_in_time")
		}
		cur.CheckInTime, changed = *in.CheckInTime, true
	}
	if in.CheckOutTime != nil && *in.CheckOutTime != cur.CheckOutTime {
		if !validClock(*in.CheckOutTime) {
			return false, ErrInvalidTime.WithField("body.check_out_time")
		}
		cur.CheckOutTime, changed = *in.CheckOutTime, true
	}
	return changed, nil
}

func mergeCounts(cur *ReservationRead, in ReservationUpdate) []string {
	fields := []string{}
	if in.Adults != nil && *in.Adults != cur.Adults {
		cur.Adults, fields = *in.Adults, append(fields, "adults")
	}
	if in.Children != nil && *in.Children != cur.Children {
		cur.Children, fields = *in.Children, append(fields, "children")
	}
	if in.TotalAmount.Sent {
		cur.TotalAmount, fields = optional.Apply(cur.TotalAmount, in.TotalAmount), append(fields, "total_amount")
	}
	if in.DepositAmount.Sent {
		cur.DepositAmount, fields = optional.Apply(cur.DepositAmount, in.DepositAmount), append(fields, "deposit_amount")
		if cur.DepositStatus == "none" || cur.DepositStatus == "pending" {
			cur.DepositStatus = depositStatusFor(cur.DepositAmount)
		}
	}
	if in.Notes.Sent {
		cur.Notes, fields = trimmed(optional.Apply(cur.Notes, in.Notes)), append(fields, "notes")
	}
	return fields
}

// --- Linea de tiempo ------------------------------------------------------------------

func addEvent(ctx context.Context, q database.Querier, id uuid.UUID, kind string, actor *uuid.UUID, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO reservation_events (reservation_id, type, actor_user_id, data) VALUES ($1, $2, $3, $4)`,
		id, kind, actor, raw)
	return err
}

func reservationEvents(ctx context.Context, q database.Querier, id uuid.UUID) ([]EventRead, error) {
	rows, err := q.Query(ctx, `
		SELECT e.id, e.type, e.actor_user_id, u.full_name AS actor_name, e.actor_guest_id, e.data, e.created_at
		FROM reservation_events e LEFT JOIN shared.users u ON u.id = e.actor_user_id
		WHERE e.reservation_id = $1 ORDER BY e.created_at, e.id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[EventRead])
}
