package reservations

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Ciclo de vida (fase04.md §3): pending -> confirmed -> checked_in ->
// checked_out; cancelled desde pending o confirmed; no_show desde confirmed el
// dia de llegada o despues.

// Confirm confirma una reserva por confirmar (necesita huesped principal) y
// avisa al huesped si send_email (por defecto si).
func (s *Service) Confirm(ctx context.Context, actor authz.Actor, id uuid.UUID, in ReservationConfirm) (ReservationDetail, error) {
	return s.transition(ctx, actor, id, func(q database.Querier, t tenantInfo, r ReservationRead) (ReservationRead, error) {
		if r.Status != "pending" {
			return r, ErrTransition
		}
		if r.PrimaryGuestID == nil {
			return r, ErrGuestRequired
		}
		if err := setStatus(ctx, q, r.ID, "confirmed"); err != nil {
			return r, err
		}
		r.Status = "confirmed"
		return s.confirmed(ctx, q, actor, t, r, in.SendEmail == nil || *in.SendEmail)
	})
}

// confirmed registra la confirmacion: linea de tiempo, bitacora, correo al
// huesped, recordatorio del dia anterior y evento para B.
func (s *Service) confirmed(ctx context.Context, q database.Querier, actor authz.Actor, t tenantInfo, r ReservationRead, send bool) (ReservationRead, error) {
	sent := false
	if send {
		var err error
		if sent, err = s.sendConfirmation(ctx, q, t, r); err != nil {
			return r, err
		}
	}
	if err := addEvent(ctx, q, r.ID, "confirmed", actorID(actor), map[string]any{"email_sent": sent}); err != nil {
		return r, err
	}
	if err := audit.Record(ctx, q, audit.Entry{
		Actor: actorID(actor), Action: "reservation.confirmed", Entity: "reservation", EntityID: &r.ID, PropertyID: &r.PropertyID,
		After: map[string]any{"code": r.Code, "email_sent": sent},
	}); err != nil {
		return r, err
	}
	if err := s.scheduleReminder(ctx, q, t.ID, r); err != nil {
		return r, err
	}
	return r, s.bus.Publish(ctx, q, events.Event{
		Type: EventConfirmed, TenantID: t.ID, ActorID: actorID(actor), Data: ReservationEvent{Reservation: r},
	})
}

// Cancel cancela una reserva viva; si estaba confirmada avisa al huesped.
func (s *Service) Cancel(ctx context.Context, actor authz.Actor, id uuid.UUID, in Reason) (ReservationDetail, error) {
	reason := strings.TrimSpace(in.Reason)
	return s.transition(ctx, actor, id, func(q database.Querier, t tenantInfo, r ReservationRead) (ReservationRead, error) {
		if r.Status != "pending" && r.Status != "confirmed" {
			return r, ErrTransition
		}
		if reason == "" {
			return r, ErrReasonRequired.WithField("body.reason")
		}
		was := r.Status
		if _, err := q.Exec(ctx, `
			UPDATE reservations SET status = 'cancelled', cancelled_at = now(), cancel_reason = $2, deposit_status = CASE WHEN deposit_status = 'pending' THEN 'none' ELSE deposit_status END, updated_at = now()
			WHERE id = $1`, r.ID, reason); err != nil {
			return r, err
		}
		sent := false
		if was == "confirmed" {
			var err error
			if sent, err = s.sendCancellation(ctx, q, t, r); err != nil {
				return r, err
			}
		}
		return s.stopped(ctx, q, actor, t, r.ID, "cancelled", map[string]any{"reason": reason, "email_sent": sent})
	})
}

// NoShow marca que el huesped no llego (el dia de entrada o despues).
func (s *Service) NoShow(ctx context.Context, actor authz.Actor, id uuid.UUID, in Reason) (ReservationDetail, error) {
	reason := strings.TrimSpace(in.Reason)
	return s.transition(ctx, actor, id, func(q database.Querier, t tenantInfo, r ReservationRead) (ReservationRead, error) {
		if r.Status != "confirmed" {
			return r, ErrTransition
		}
		if t.Today < r.CheckInDate {
			return r, ErrTooEarly
		}
		if _, err := q.Exec(ctx, `
			UPDATE reservations SET status = 'no_show', cancelled_at = now(), cancel_reason = nullif($2, ''), deposit_status = CASE WHEN deposit_status = 'pending' THEN 'none' ELSE deposit_status END, updated_at = now()
			WHERE id = $1`, r.ID, reason); err != nil {
			return r, err
		}
		return s.stopped(ctx, q, actor, t, r.ID, "no_show", map[string]any{"reason": reason})
	})
}

// stopped registra la cancelacion o el no-show y avisa a B (libera alarmas,
// accesos y la limpieza).
func (s *Service) stopped(ctx context.Context, q database.Querier, actor authz.Actor, t tenantInfo, id uuid.UUID, kind string, data map[string]any) (ReservationRead, error) {
	if err := addEvent(ctx, q, id, kind, actorID(actor), data); err != nil {
		return ReservationRead{}, err
	}
	r, err := reservationByID(ctx, q, id, false)
	if err != nil {
		return r, err
	}
	if err := audit.Record(ctx, q, audit.Entry{
		Actor: actorID(actor), Action: "reservation." + kind, Entity: "reservation", EntityID: &id, PropertyID: &r.PropertyID,
		After: map[string]any{"code": r.Code, "reason": data["reason"]},
	}); err != nil {
		return r, err
	}
	return r, s.bus.Publish(ctx, q, events.Event{
		Type: EventCancelled, TenantID: t.ID, ActorID: actorID(actor), Data: ReservationEvent{Reservation: r},
	})
}

// CheckIn registra la entrada. En canales con check-in formal exige a todos
// los huespedes registrados y, si el canal pide deposito, el deposito
// recibido.
func (s *Service) CheckIn(ctx context.Context, actor authz.Actor, id uuid.UUID) (ReservationDetail, error) {
	return s.transition(ctx, actor, id, func(q database.Querier, t tenantInfo, r ReservationRead) (ReservationRead, error) {
		if r.Status != "confirmed" {
			return r, ErrTransition
		}
		if t.Today < r.CheckInDate {
			return r, ErrTooEarly.WithMessage("La entrada es el " + shortDate(r.CheckInDate) + ".")
		}
		if err := checkinReady(ctx, q, r); err != nil {
			return r, err
		}
		if _, err := q.Exec(ctx, `
			UPDATE reservations SET status = 'checked_in', checked_in_at = now(), updated_at = now() WHERE id = $1`, r.ID); err != nil {
			return r, err
		}
		return s.moved(ctx, q, actor, t.ID, r.ID, "checked_in", EventCheckedIn, nil)
	})
}

func checkinReady(ctx context.Context, q database.Querier, r ReservationRead) error {
	if !r.HasCheckin {
		return nil
	}
	if r.GuestsCount == 0 || r.GuestsPending > 0 {
		names, err := pendingNames(ctx, q, r.ID)
		if err != nil {
			return err
		}
		msg := "Faltan datos de huéspedes para el check-in"
		if len(names) > 0 {
			msg += ": " + strings.Join(names, ", ")
		}
		return ErrGuestsPending.WithMessage(msg + ".")
	}
	if r.RequiresDeposit && r.DepositAmount != nil && *r.DepositAmount > 0 && r.DepositStatus != "received" {
		return ErrDepositPending
	}
	return nil
}

func pendingNames(ctx context.Context, q database.Querier, id uuid.UUID) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT g.full_name FROM reservation_guests rg JOIN guests g ON g.id = rg.guest_id
		WHERE rg.reservation_id = $1 AND NOT `+registeredSQL+` ORDER BY rg.is_primary DESC, g.full_name`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// CheckOut registra la salida. Desde confirmada solo en canales sin
// check-in formal. Las fotos del documento se programan para borrarse a los
// 90 dias de esta salida.
func (s *Service) CheckOut(ctx context.Context, actor authz.Actor, id uuid.UUID) (ReservationDetail, error) {
	return s.transition(ctx, actor, id, func(q database.Querier, t tenantInfo, r ReservationRead) (ReservationRead, error) {
		allowed := r.Status == "checked_in" || (r.Status == "confirmed" && !r.HasCheckin && t.Today >= r.CheckInDate)
		if !allowed {
			return r, ErrTransition
		}
		if _, err := q.Exec(ctx, `
			UPDATE reservations SET status = 'checked_out', checked_out_at = now(), updated_at = now() WHERE id = $1`, r.ID); err != nil {
			return r, err
		}
		if err := refreshReservationDocs(ctx, q, r.ID); err != nil {
			return r, err
		}
		data := map[string]any{"inspection_required": r.DepositAmount != nil && *r.DepositAmount > 0}
		return s.moved(ctx, q, actor, t.ID, r.ID, "checked_out", EventCheckedOut, data)
	})
}

// moved registra una entrada o salida y avisa a B.
func (s *Service) moved(ctx context.Context, q database.Querier, actor authz.Actor, tenantID, id uuid.UUID, kind, event string, data map[string]any) (ReservationRead, error) {
	if err := addEvent(ctx, q, id, kind, actorID(actor), data); err != nil {
		return ReservationRead{}, err
	}
	r, err := reservationByID(ctx, q, id, false)
	if err != nil {
		return r, err
	}
	if err := audit.Record(ctx, q, audit.Entry{
		Actor: actorID(actor), Action: "reservation." + kind, Entity: "reservation", EntityID: &id, PropertyID: &r.PropertyID,
		After: map[string]any{"code": r.Code},
	}); err != nil {
		return r, err
	}
	return r, s.bus.Publish(ctx, q, events.Event{
		Type: event, TenantID: tenantID, ActorID: actorID(actor), Data: ReservationEvent{Reservation: r},
	})
}

func refreshReservationDocs(ctx context.Context, q database.Querier, id uuid.UUID) error {
	rows, err := q.Query(ctx, `SELECT guest_id FROM reservation_guests WHERE reservation_id = $1`, id)
	if err != nil {
		return err
	}
	guests, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, g := range guests {
		if err := refreshDocumentExpiry(ctx, q, g); err != nil {
			return err
		}
	}
	return nil
}

// Resend reenvia el correo de confirmacion al huesped principal.
func (s *Service) Resend(ctx context.Context, actor authz.Actor, id uuid.UUID) (ReservationDetail, error) {
	return s.transition(ctx, actor, id, func(q database.Querier, t tenantInfo, r ReservationRead) (ReservationRead, error) {
		if r.Status != "confirmed" && r.Status != "checked_in" {
			return r, ErrTransition
		}
		sent, err := s.sendConfirmation(ctx, q, t, r)
		if err != nil {
			return r, err
		}
		if !sent {
			return r, ErrNoEmail
		}
		return r, addEvent(ctx, q, r.ID, "email_sent", actorID(actor), map[string]any{"template": "confirmation"})
	})
}

// transition corre un paso del ciclo con la reserva bloqueada y devuelve la
// ficha actualizada.
func (s *Service) transition(ctx context.Context, actor authz.Actor, id uuid.UUID,
	fn func(q database.Querier, t tenantInfo, r ReservationRead) (ReservationRead, error),
) (ReservationDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (ReservationDetail, error) {
		if _, _, err := s.visible(ctx, q, actor, id); err != nil {
			return ReservationDetail{}, err
		}
		t, err := s.tenant(ctx, q)
		if err != nil {
			return ReservationDetail{}, err
		}
		r, err := reservationByID(ctx, q, id, true)
		if err != nil {
			return ReservationDetail{}, err
		}
		if _, err := fn(q, t, r); err != nil {
			return ReservationDetail{}, err
		}
		r, v, err := s.visible(ctx, q, actor, id)
		if err != nil {
			return ReservationDetail{}, err
		}
		return s.detail(ctx, q, actor, v, r)
	})
}

func setStatus(ctx context.Context, q database.Querier, id uuid.UUID, status string) error {
	_, err := q.Exec(ctx, `UPDATE reservations SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	return err
}

// --- Estados automaticos ---------------------------------------------------------------

// AutoResult es lo que hizo una pasada de estados automaticos.
type AutoResult struct {
	CheckedIn, CheckedOut int
}

// RunAutoStatus marca la entrada y la salida de las reservas de canales sin
// check-in formal (Booking.com, Airbnb) a su hora, para que el Inicio, las
// alarmas y las limpiezas de B sepan quien esta en casa. Corre en el schema de
// la empresa.
func (s *Service) RunAutoStatus(ctx context.Context, q database.Querier, tenantID uuid.UUID) (AutoResult, error) {
	var res AutoResult
	system := authz.Actor{TenantID: tenantID}
	for _, step := range []struct {
		from, to, kind, event, when string
		count                       *int
	}{
		{"confirmed", "checked_in", "checked_in", EventCheckedIn, "r.check_in_date + r.check_in_time", &res.CheckedIn},
		{"checked_in", "checked_out", "checked_out", EventCheckedOut, "r.check_out_date + r.check_out_time", &res.CheckedOut},
	} {
		rows, err := q.Query(ctx, `
			UPDATE reservations r SET status = $1::text,
			       checked_in_at = CASE WHEN $1::text = 'checked_in' THEN now() ELSE r.checked_in_at END,
			       checked_out_at = CASE WHEN $1::text = 'checked_out' THEN now() ELSE r.checked_out_at END, updated_at = now()
			FROM booking_channels c
			WHERE c.id = r.channel_id AND NOT c.has_checkin AND r.status = $2::text
			  AND `+step.when+` <= ($3::timestamptz AT TIME ZONE
			      (SELECT timezone FROM shared.tenants WHERE schema_name = current_schema()))
			RETURNING r.id`, step.to, step.from, s.now())
		if err != nil {
			return res, err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return res, err
		}
		for _, id := range ids {
			if step.to == "checked_out" {
				if err := refreshReservationDocs(ctx, q, id); err != nil {
					return res, err
				}
			}
			if _, err := s.moved(ctx, q, system, tenantID, id, step.kind, step.event, map[string]any{"auto": true}); err != nil {
				return res, err
			}
		}
		*step.count = len(ids)
	}
	return res, nil
}
