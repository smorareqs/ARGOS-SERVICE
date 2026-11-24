package reservations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/ical"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

const selectFeed = `
	SELECT f.id, f.property_id, p.code AS property_code, p.name AS property_name, f.channel_id,
	       c.name AS channel_name, c.kind AS channel_kind, f.url, f.is_active, f.last_synced_at, f.last_success_at,
	       f.last_error, f.events_count, f.created_at, f.updated_at
	FROM property_ical_feeds f
	JOIN properties p ON p.id = f.property_id
	JOIN booking_channels c ON c.id = f.channel_id`

// Feeds lista los calendarios (de una propiedad o todos), con error primero.
func (s *Service) Feeds(ctx context.Context, actor authz.Actor, propertyID *uuid.UUID) ([]FeedRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]FeedRead, error) {
		if propertyID != nil {
			if ok, err := propertyInScope(ctx, q, actor, *propertyID); err != nil || !ok {
				return nil, errOr(err, ErrPropertyNotFound)
			}
		}
		rows, err := q.Query(ctx, selectFeed+`
			WHERE p.deleted_at IS NULL AND ($1::uuid IS NULL OR f.property_id = $1)
			ORDER BY f.last_error IS NULL, p.code, c.name`, propertyID)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[FeedRead])
	})
}

// CreateFeed agrega la URL iCal de una propiedad en un canal.
func (s *Service) CreateFeed(ctx context.Context, actor authz.Actor, propertyID uuid.UUID, in FeedCreate) (FeedRead, error) {
	u, err := s.fetcher.ValidURL(in.URL)
	if err != nil {
		return FeedRead{}, ErrFeedURL.WithField("body.url")
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (FeedRead, error) {
		if ok, err := propertyInScope(ctx, q, actor, propertyID); err != nil || !ok {
			return FeedRead{}, errOr(err, ErrPropertyNotFound)
		}
		if _, err := activeChannel(ctx, q, in.ChannelID, "body.channel_id"); err != nil {
			return FeedRead{}, err
		}
		var id uuid.UUID
		err := q.QueryRow(ctx, `INSERT INTO property_ical_feeds (property_id, channel_id, url) VALUES ($1, $2, $3) RETURNING id`,
			propertyID, in.ChannelID, u).Scan(&id)
		if uniqueViolation(err, "uq_property_ical_feeds_property_id_channel_id") {
			return FeedRead{}, ErrFeedTaken.WithField("body.channel_id")
		}
		if err != nil {
			return FeedRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: actorID(actor), Action: "ical_feed.created", Entity: "ical_feed", EntityID: &id, PropertyID: &propertyID,
			After: map[string]any{"channel_id": in.ChannelID},
		}); err != nil {
			return FeedRead{}, err
		}
		return feedByID(ctx, q, id)
	})
}

// UpdateFeed cambia la URL o pausa el calendario.
func (s *Service) UpdateFeed(ctx context.Context, actor authz.Actor, id uuid.UUID, in FeedUpdate) (FeedRead, error) {
	var u *string
	if in.URL != nil {
		v, err := s.fetcher.ValidURL(*in.URL)
		if err != nil {
			return FeedRead{}, ErrFeedURL.WithField("body.url")
		}
		u = &v
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (FeedRead, error) {
		cur, err := feedByID(ctx, q, id)
		if err != nil {
			return FeedRead{}, err
		}
		if ok, err := propertyInScope(ctx, q, actor, cur.PropertyID); err != nil || !ok {
			return FeedRead{}, errOr(err, ErrFeedNotFound)
		}
		if _, err := q.Exec(ctx, `
			UPDATE property_ical_feeds SET url = coalesce($2, url), is_active = coalesce($3, is_active),
			       last_error = CASE WHEN $2::text IS NOT NULL AND $2 <> url THEN NULL ELSE last_error END, updated_at = now()
			WHERE id = $1`, id, u, in.IsActive); err != nil {
			return FeedRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: actorID(actor), Action: "ical_feed.updated", Entity: "ical_feed", EntityID: &id, PropertyID: &cur.PropertyID,
			After: map[string]any{"url_changed": u != nil, "is_active": in.IsActive},
		}); err != nil {
			return FeedRead{}, err
		}
		return feedByID(ctx, q, id)
	})
}

// DeleteFeed quita el calendario (las reservas importadas se quedan).
func (s *Service) DeleteFeed(ctx context.Context, actor authz.Actor, id uuid.UUID) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		cur, err := feedByID(ctx, q, id)
		if err != nil {
			return err
		}
		if ok, err := propertyInScope(ctx, q, actor, cur.PropertyID); err != nil || !ok {
			return errOr(err, ErrFeedNotFound)
		}
		if _, err := q.Exec(ctx, `DELETE FROM property_ical_feeds WHERE id = $1`, id); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actorID(actor), Action: "ical_feed.deleted", Entity: "ical_feed", EntityID: &id, PropertyID: &cur.PropertyID,
			After: map[string]any{"channel": cur.ChannelName},
		})
	})
}

func feedByID(ctx context.Context, q database.Querier, id uuid.UUID) (FeedRead, error) {
	rows, err := q.Query(ctx, selectFeed+` WHERE f.id = $1`, id)
	if err != nil {
		return FeedRead{}, err
	}
	f, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[FeedRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return FeedRead{}, ErrFeedNotFound
	}
	return f, err
}

// --- Sincronizacion --------------------------------------------------------------------

// SyncNow lee ahora los calendarios activos (todos o uno) de la empresa de la
// sesion ("Sincronizar iCal ahora", W-H1).
func (s *Service) SyncNow(ctx context.Context, actor authz.Actor, feedID *uuid.UUID) ([]SyncResult, error) {
	feeds, err := s.Feeds(ctx, actor, nil)
	if err != nil {
		return nil, err
	}
	out := []SyncResult{}
	for _, f := range feeds {
		if (feedID != nil && f.ID != *feedID) || (feedID == nil && !f.IsActive) {
			continue
		}
		out = append(out, s.syncFeed(ctx, actor.TenantID, f))
	}
	if feedID != nil && len(out) == 0 {
		return nil, ErrFeedNotFound
	}
	return out, nil
}

// RunIcalSync lee todos los calendarios activos de la empresa del contexto
// (job ical_sync, cada 30 minutos).
func (s *Service) RunIcalSync(ctx context.Context, tenantID uuid.UUID) ([]SyncResult, error) {
	feeds, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]FeedRead, error) {
		rows, err := q.Query(ctx, selectFeed+` WHERE f.is_active AND p.deleted_at IS NULL AND p.is_active ORDER BY f.id`)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[FeedRead])
	})
	if err != nil {
		return nil, err
	}
	out := make([]SyncResult, 0, len(feeds))
	for _, f := range feeds {
		out = append(out, s.syncFeed(ctx, tenantID, f))
	}
	return out, nil
}

// syncFeed descarga fuera de la transaccion y aplica en una sola.
func (s *Service) syncFeed(ctx context.Context, tenantID uuid.UUID, f FeedRead) SyncResult {
	res := SyncResult{FeedID: f.ID, PropertyCode: f.PropertyCode, ChannelName: f.ChannelName, Conflicts: []string{}}
	loc, today, err := s.feedClock(ctx)
	if err == nil {
		var evs []ical.Event
		if evs, err = s.fetcher.Fetch(ctx, f.URL, loc); err == nil {
			err = s.db.InTx(ctx, func(q database.Querier) error {
				return s.applyFeed(ctx, q, tenantID, f, evs, today, &res)
			})
		}
	}
	if err != nil {
		msg := "No se pudo sincronizar el calendario."
		var ie *ical.Error
		if errors.As(err, &ie) {
			msg = ie.Message
		} else {
			s.log.ErrorContext(ctx, "Sincronizacion iCal", "feed_id", f.ID, "error", err)
		}
		res.Error = &msg
	}
	res.OK = res.Error == nil
	s.recordSync(ctx, f.ID, res)
	return res
}

func (s *Service) feedClock(ctx context.Context) (*time.Location, string, error) {
	t, err := database.InTxValue(ctx, s.db, func(q database.Querier) (tenantInfo, error) { return s.tenant(ctx, q) })
	if err != nil {
		return nil, "", err
	}
	loc, err := time.LoadLocation(t.Timezone)
	if err != nil {
		loc = time.UTC
	}
	return loc, t.Today, nil
}

// recordSync guarda el resultado en el calendario (W-H6).
func (s *Service) recordSync(ctx context.Context, id uuid.UUID, res SyncResult) {
	lastError := res.Error
	if lastError == nil && len(res.Conflicts) > 0 {
		msg := "No se importaron eventos que se cruzan con reservas de ARGOS: " + strings.Join(res.Conflicts, "; ") + "."
		lastError = &msg
	}
	err := s.db.InTx(ctx, func(q database.Querier) error {
		_, err := q.Exec(ctx, `
			UPDATE property_ical_feeds SET last_synced_at = now(), last_error = $2,
			       last_success_at = CASE WHEN $3 THEN now() ELSE last_success_at END,
			       events_count = CASE WHEN $3 THEN $4 ELSE events_count END, updated_at = now()
			WHERE id = $1`, id, lastError, res.OK, res.Events)
		return err
	})
	if err != nil {
		s.log.ErrorContext(ctx, "Guardando la sincronizacion iCal", "feed_id", id, "error", err)
	}
}

// applyFeed crea, mueve o cancela las reservas importadas de un calendario.
func (s *Service) applyFeed(ctx context.Context, q database.Querier, tenantID uuid.UUID, f FeedRead, evs []ical.Event, today string, res *SyncResult) error {
	seen := []string{}
	for _, e := range evs {
		if e.Cancelled || blockEvent(e) || e.End <= today {
			continue
		}
		res.Events++
		seen = append(seen, e.UID)
		if err := s.applyEvent(ctx, q, tenantID, f, e, res); err != nil {
			return err
		}
	}
	return s.cancelMissing(ctx, q, tenantID, f, seen, today, res)
}

func (s *Service) applyEvent(ctx context.Context, q database.Querier, tenantID uuid.UUID, f FeedRead, e ical.Event, res *SyncResult) error {
	var id uuid.UUID
	err := q.QueryRow(ctx, `SELECT id FROM reservations WHERE property_id = $1 AND external_ref = $2`, f.PropertyID, e.UID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.importEvent(ctx, q, f, e, res)
	}
	if err != nil {
		return err
	}
	cur, err := reservationByID(ctx, q, id, true)
	if err != nil {
		return err
	}
	live := cur.Status == "pending" || cur.Status == "confirmed" || cur.Status == "checked_in"
	if !live || slices.Contains(cur.ManualFields, "dates") || (cur.CheckInDate == e.Start && cur.CheckOutDate == e.End) {
		return nil
	}
	return s.moveImported(ctx, q, tenantID, f, e, cur, res)
}

// moveImported aplica las fechas nuevas de un evento a su reserva importada.
func (s *Service) moveImported(ctx context.Context, q database.Querier, tenantID uuid.UUID, f FeedRead, e ical.Event, cur ReservationRead, res *SyncResult) error {
	id := cur.ID
	if conflict, err := overlapWith(ctx, q, f.PropertyID, e.Start, e.End, &id); err != nil || conflict != "" {
		if err == nil {
			res.Conflicts = append(res.Conflicts, conflictText(e, conflict))
		}
		return err
	}
	if _, err := q.Exec(ctx, `UPDATE reservations SET check_in_date = $2, check_out_date = $3, external_summary = $4, updated_at = now() WHERE id = $1`,
		id, e.Start, e.End, nullable(e.Summary)); err != nil {
		return err
	}
	res.Updated++
	if err := addEvent(ctx, q, id, "synced", nil, map[string]any{
		"channel": f.ChannelName, "from": cur.CheckInDate + "/" + cur.CheckOutDate, "to": e.Start + "/" + e.End,
	}); err != nil {
		return err
	}
	if cur.Status == "pending" {
		return nil
	}
	r, err := reservationByID(ctx, q, id, false)
	if err != nil {
		return err
	}
	return s.bus.Publish(ctx, q, events.Event{Type: EventUpdated, TenantID: tenantID, Data: ReservationEvent{Reservation: r, Previous: &cur}})
}

// importEvent crea la reserva por completar (sin huesped) de un evento nuevo.
func (s *Service) importEvent(ctx context.Context, q database.Querier, f FeedRead, e ical.Event, res *SyncResult) error {
	if conflict, err := overlapWith(ctx, q, f.PropertyID, e.Start, e.End, nil); err != nil || conflict != "" {
		if err == nil {
			res.Conflicts = append(res.Conflicts, conflictText(e, conflict))
		}
		return err
	}
	t, err := s.tenant(ctx, q)
	if err != nil {
		return err
	}
	ch, err := channelByID(ctx, q, f.ChannelID)
	if err != nil {
		return err
	}
	deposit := (*float64)(nil)
	if ch.RequiresDeposit {
		deposit = ch.DefaultDepositAmount
	}
	uid, summary := e.UID, nullable(e.Summary)
	id, err := insertReservation(ctx, q, row{
		propertyID: f.PropertyID, channelID: f.ChannelID, checkIn: e.Start, checkOut: e.End,
		checkInTime: t.CheckinTime, checkOutTime: t.CheckoutTime, adults: 1, deposit: deposit,
		depositStatus: depositStatusFor(deposit), status: "pending", source: "ical", externalRef: &uid, summary: summary,
		currency: t.Currency,
	})
	if err != nil {
		return err
	}
	res.Created++
	return addEvent(ctx, q, id, "imported", nil, map[string]any{"channel": f.ChannelName, "summary": e.Summary})
}

// cancelMissing cancela las reservas importadas que ya no estan en el
// calendario (y aun no empiezan).
func (s *Service) cancelMissing(ctx context.Context, q database.Querier, tenantID uuid.UUID, f FeedRead, seen []string, today string, res *SyncResult) error {
	reason := "Ya no aparece en el calendario de " + f.ChannelName
	rows, err := q.Query(ctx, `
		UPDATE reservations SET status = 'cancelled', cancelled_at = now(), cancel_reason = $5, updated_at = now()
		WHERE property_id = $1 AND channel_id = $2 AND source = 'ical' AND status IN ('pending', 'confirmed')
		  AND check_in_date >= $3::date AND NOT (external_ref = ANY($4::text[]))
		RETURNING id`, f.PropertyID, f.ChannelID, today, seen, reason)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := addEvent(ctx, q, id, "cancelled", nil, map[string]any{"reason": reason, "auto": true}); err != nil {
			return err
		}
		r, err := reservationByID(ctx, q, id, false)
		if err != nil {
			return err
		}
		if err := s.bus.Publish(ctx, q, events.Event{Type: EventCancelled, TenantID: tenantID, Data: ReservationEvent{Reservation: r}}); err != nil {
			return err
		}
	}
	res.Cancelled = len(ids)
	return nil
}

func conflictText(e ical.Event, with string) string {
	return fmt.Sprintf("%s al %s se cruza con %s", shortDate(e.Start), shortDate(e.End), with)
}

func nullable(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

// blockEvent indica eventos que no son estancias: los bloqueos que Airbnb
// exporta como "Airbnb (Not available)". Booking.com exporta sus reservas como
// "CLOSED - Not available", y esas si se importan.
func blockEvent(e ical.Event) bool {
	return strings.Contains(strings.ToLower(e.Summary), "airbnb (not available)")
}
