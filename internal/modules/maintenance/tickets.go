package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
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

const selectTicket = `
	SELECT t.id, t.number, t.property_id, p.code AS property_code, p.name AS property_name,
	       coalesce(t.building_id, p.building_id) AS building_id, b.name AS building_name,
	       t.asset_id, a.name AS asset_name, a.category AS asset_category, t.plan_id, mp.name AS plan_name,
	       t.title, t.description, t.urgency, t.status,
	       t.reported_by_user_id, ur.full_name AS reporter_name, t.assigned_to, ua.full_name AS assignee_name,
	       t.vendor_id, v.name AS vendor_name, t.task_id, t.resolution, t.cost::float8 AS cost,
	       (SELECT count(*)::int FROM files f WHERE f.entity_type = 'maintenance_ticket' AND f.entity_id = t.id
	          AND f.status = 'ready' AND f.deleted_at IS NULL) AS photos_count,
	       t.resolved_at, t.closed_at, t.created_at, t.updated_at
	FROM maintenance_tickets t
	LEFT JOIN properties p ON p.id = t.property_id
	LEFT JOIN buildings b ON b.id = coalesce(t.building_id, p.building_id)
	LEFT JOIN assets a ON a.id = t.asset_id
	LEFT JOIN maintenance_plans mp ON mp.id = t.plan_id
	LEFT JOIN shared.users ur ON ur.id = t.reported_by_user_id
	LEFT JOIN shared.users ua ON ua.id = t.assigned_to
	LEFT JOIN vendors v ON v.id = t.vendor_id`

// ticketScope: sus propiedades, o las que reporto o tiene asignadas
// ($1 todas, $2 ids, $3 el actor).
const ticketScope = `($1::bool OR t.property_id = ANY($2::uuid[]) OR t.assigned_to = $3 OR t.reported_by_user_id = $3
	OR t.building_id IN (SELECT building_id FROM properties WHERE id = ANY($2::uuid[]) AND building_id IS NOT NULL))`

// activeStatuses son los estados de una incidencia abierta.
var activeStatuses = []string{"open", "assigned", "in_progress"}

// Tickets devuelve una pagina de incidencias del alcance, las mas nuevas primero.
func (s *Service) Tickets(ctx context.Context, actor authz.Actor, f TicketFilter) ([]TicketRead, *string, error) {
	limit := paging.Limit(f.Limit)
	key, err := paging.Decode(f.Cursor, 2)
	if err != nil {
		return nil, nil, err
	}
	var afterAt, afterID *string
	if key != nil {
		afterAt, afterID = &key[0], &key[1]
	}
	statuses := expandStatuses(f.Statuses)
	var assignee *uuid.UUID
	if f.Mine {
		assignee = &actor.UserID
	} else {
		assignee = f.AssigneeID
	}
	items, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]TicketRead, error) {
		scope, err := scopeOf(ctx, q, actor)
		if err != nil {
			return nil, err
		}
		all, ids := scope.SQLArgs()
		search, number := searchTerms(f.Query)
		rows, err := q.Query(ctx, selectTicket+`
			WHERE `+ticketScope+`
			  AND (coalesce(cardinality($4::text[]), 0) = 0 OR t.status = ANY($4::text[]))
			  AND ($5::text = '' OR t.urgency = $5)
			  AND ($6::uuid IS NULL OR t.property_id = $6)
			  AND ($7::uuid IS NULL OR t.asset_id = $7)
			  AND ($8::uuid IS NULL OR t.vendor_id = $8)
			  AND ($9::uuid IS NULL OR t.assigned_to = $9)
			  AND ($10::text = '' OR lower(t.title) LIKE $10 OR lower(coalesce(p.code, '')) LIKE $10
			       OR lower(coalesce(p.name, b.name, '')) LIKE $10
			       OR lower(coalesce(a.name, '')) LIKE $10 OR t.number = $11::int)
			  AND ($12::timestamptz IS NULL OR (t.created_at, t.id) < ($12::timestamptz, $13::uuid))
			ORDER BY t.created_at DESC, t.id DESC
			LIMIT $14`,
			all, ids, actor.UserID, statuses, f.Urgency, f.PropertyID, f.AssetID, f.VendorID, assignee,
			search, number, afterAt, afterID, limit+1)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[TicketRead])
	})
	if err != nil {
		return nil, nil, err
	}
	items, next := paging.Page(items, limit, func(t TicketRead) []string {
		return []string{t.CreatedAt.Format(time.RFC3339Nano), t.ID.String()}
	})
	return items, next, nil
}

// Ticket devuelve la incidencia con su linea de tiempo y fotos.
func (s *Service) Ticket(ctx context.Context, actor authz.Actor, id uuid.UUID) (TicketDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TicketDetail, error) {
		t, err := s.visibleTicket(ctx, q, actor, id)
		if err != nil {
			return TicketDetail{}, err
		}
		return s.detail(ctx, q, actor.TenantID, t)
	})
}

// CreateTicket reporta una incidencia sobre una propiedad del alcance.
func (s *Service) CreateTicket(ctx context.Context, actor authz.Actor, in TicketCreate) (TicketDetail, error) {
	title, description := trim(in.Title), trim(in.Description)
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TicketDetail, error) {
		ok, err := s.propertyInScope(ctx, q, actor, in.PropertyID)
		if err != nil {
			return TicketDetail{}, err
		}
		if !ok {
			return TicketDetail{}, ErrPropertyNotFound
		}
		if err := assetOfPlace(ctx, q, in.AssetID, &in.PropertyID, nil); err != nil {
			return TicketDetail{}, err
		}
		var id uuid.UUID
		if err := q.QueryRow(ctx, `
			INSERT INTO maintenance_tickets (property_id, asset_id, title, description, urgency, reported_by_user_id)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			in.PropertyID, in.AssetID, title, description, in.Urgency, actor.UserID).Scan(&id); err != nil {
			return TicketDetail{}, err
		}
		if n, err := attachFiles(ctx, q, in.PhotoIDs, "ticket_photo", "maintenance_ticket", id, actor.UserID); err != nil {
			return TicketDetail{}, err
		} else if n != len(in.PhotoIDs) {
			return TicketDetail{}, ErrInvalidFiles.WithField("body.photo_file_ids")
		}
		t, err := s.created(ctx, q, actor.TenantID, &actor.UserID, id, map[string]any{"photos": len(in.PhotoIDs)})
		if err != nil {
			return TicketDetail{}, err
		}
		return s.detail(ctx, q, actor.TenantID, t)
	})
}

// ReportFromInspection crea la incidencia de un hallazgo de inspeccion
// (reservations, fase 4) dentro de la transaccion de quien llama. El alcance
// ya lo valido la reserva.
func (s *Service) ReportFromInspection(ctx context.Context, q database.Querier, actor authz.Actor,
	propertyID uuid.UUID, title, description, urgency string,
) (TicketRead, error) {
	title = trim(title)
	if len([]rune(title)) > 160 {
		title = string([]rune(title)[:160])
	}
	var reporter *uuid.UUID
	if actor.UserID != uuid.Nil {
		reporter = &actor.UserID
	}
	var id uuid.UUID
	if err := q.QueryRow(ctx, `
		INSERT INTO maintenance_tickets (property_id, title, description, urgency, reported_by_user_id)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`, propertyID, title, trim(description), urgency, reporter).Scan(&id); err != nil {
		return TicketRead{}, err
	}
	return s.created(ctx, q, actor.TenantID, reporter, id, map[string]any{"source": "inspection"})
}

// created registra el alta del ticket (evento, bitacora y aviso) y lo devuelve.
func (s *Service) created(ctx context.Context, q database.Querier, tenantID uuid.UUID, by *uuid.UUID, id uuid.UUID, data map[string]any) (TicketRead, error) {
	if err := addEvent(ctx, q, id, "created", by, data); err != nil {
		return TicketRead{}, err
	}
	t, err := ticketByID(ctx, q, id)
	if err != nil {
		return TicketRead{}, err
	}
	if err := audit.Record(ctx, q, audit.Entry{
		Actor: by, Action: "ticket.created", Entity: "maintenance_ticket", EntityID: &id, PropertyID: t.PropertyID,
		After: map[string]any{"number": t.Number, "title": t.Title, "urgency": t.Urgency, "plan_id": t.PlanID},
	}); err != nil {
		return TicketRead{}, err
	}
	return t, s.bus.Publish(ctx, q, events.Event{
		Type: EventTicketCreated, TenantID: tenantID, ActorID: by, Data: TicketEvent{Ticket: t},
	})
}

// UpdateTicket corrige titulo, descripcion, urgencia, activo o costo.
func (s *Service) UpdateTicket(ctx context.Context, actor authz.Actor, id uuid.UUID, in TicketUpdate) (TicketDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TicketDetail, error) {
		cur, err := s.visibleTicket(ctx, q, actor, id)
		if err != nil {
			return TicketDetail{}, err
		}
		v, fields := mergeTicket(cur, in)
		if slices.Contains(fields, "asset_id") {
			if err := assetOfPlace(ctx, q, v.AssetID, cur.PropertyID, cur.BuildingID); err != nil {
				return TicketDetail{}, err
			}
		}
		if len(fields) == 0 {
			return s.detail(ctx, q, actor.TenantID, cur)
		}
		if _, err := q.Exec(ctx, `
			UPDATE maintenance_tickets SET title = $2, description = $3, urgency = $4, asset_id = $5, cost = $6, updated_at = now()
			WHERE id = $1`, id, v.Title, v.Description, v.Urgency, v.AssetID, v.Cost); err != nil {
			return TicketDetail{}, err
		}
		data := map[string]any{"fields": fields}
		if slices.Contains(fields, "urgency") {
			data["urgency"] = v.Urgency
		}
		if slices.Contains(fields, "cost") {
			data["cost"] = v.Cost
		}
		if err := addEvent(ctx, q, id, "updated", &actor.UserID, data); err != nil {
			return TicketDetail{}, err
		}
		t, err := ticketByID(ctx, q, id)
		if err != nil {
			return TicketDetail{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "ticket.updated", Entity: "maintenance_ticket", EntityID: &id,
			PropertyID: t.PropertyID, After: data,
		}); err != nil {
			return TicketDetail{}, err
		}
		return s.detail(ctx, q, actor.TenantID, t)
	})
}

// mergeTicket aplica el PATCH y devuelve los campos que cambiaron.
func mergeTicket(cur TicketRead, in TicketUpdate) (TicketRead, []string) {
	fields := []string{}
	if in.Title != nil && trim(*in.Title) != cur.Title {
		cur.Title, fields = trim(*in.Title), append(fields, "title")
	}
	if in.Description != nil && trim(*in.Description) != cur.Description {
		cur.Description, fields = trim(*in.Description), append(fields, "description")
	}
	if in.Urgency != nil && *in.Urgency != cur.Urgency {
		cur.Urgency, fields = *in.Urgency, append(fields, "urgency")
	}
	if asset := optional.Apply(cur.AssetID, in.AssetID); in.AssetID.Sent && !sameID(asset, cur.AssetID) {
		cur.AssetID, fields = asset, append(fields, "asset_id")
	}
	if cost := optional.Apply(cur.Cost, in.Cost); in.Cost.Sent && !sameCost(cost, cur.Cost) {
		cur.Cost, fields = cost, append(fields, "cost")
	}
	return cur, fields
}

// Assignables lista quienes pueden atender incidencias (maintenance.execute),
// con su carga abierta.
func (s *Service) Assignables(ctx context.Context, actor authz.Actor) ([]Assignable, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]Assignable, error) {
		rows, err := q.Query(ctx, `
			SELECT u.id AS user_id, u.full_name, r.name AS role_name,
			       (SELECT count(*)::int FROM maintenance_tickets t
			         WHERE t.assigned_to = u.id AND t.status IN ('assigned', 'in_progress')) AS open_tickets
			FROM shared.memberships m
			JOIN shared.users u ON u.id = m.user_id
			JOIN shared.roles r ON r.id = m.role_id
			WHERE m.tenant_id = $1 AND m.status = 'active'
			  AND EXISTS (SELECT 1 FROM shared.role_permissions rp WHERE rp.role_id = r.id AND rp.permission = $2)
			ORDER BY open_tickets, u.full_name`, actor.TenantID, string(authz.MaintenanceExecute))
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[Assignable])
	})
}

// canExecute indica si la persona es miembro activo con maintenance.execute.
func canExecute(ctx context.Context, q database.Querier, tenantID, userID uuid.UUID) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM shared.memberships m
			JOIN shared.role_permissions rp ON rp.role_id = m.role_id AND rp.permission = $3
			WHERE m.tenant_id = $1 AND m.user_id = $2 AND m.status = 'active')`,
		tenantID, userID, string(authz.MaintenanceExecute)).Scan(&ok)
	return ok, err
}

// --- Lectura -----------------------------------------------------------------------

func (s *Service) detail(ctx context.Context, q database.Querier, tenantID uuid.UUID, t TicketRead) (TicketDetail, error) {
	rows, err := q.Query(ctx, `
		SELECT e.id, e.type, e.actor_user_id, u.full_name AS actor_name, e.data, e.created_at
		FROM ticket_events e LEFT JOIN shared.users u ON u.id = e.actor_user_id
		WHERE e.ticket_id = $1 ORDER BY e.created_at, e.id`, t.ID)
	if err != nil {
		return TicketDetail{}, err
	}
	evs, err := pgx.CollectRows(rows, pgx.RowToStructByName[TicketEventRead])
	if err != nil {
		return TicketDetail{}, err
	}
	photos, err := s.photos(ctx, q, tenantID, t.ID)
	if err != nil {
		return TicketDetail{}, err
	}
	return TicketDetail{TicketRead: t, Events: evs, Photos: photos}, nil
}

func (s *Service) photos(ctx context.Context, q database.Querier, tenantID, ticketID uuid.UUID) ([]PhotoRead, error) {
	rows, err := q.Query(ctx, `
		SELECT id, kind, s3_key, created_at FROM files
		WHERE entity_type = 'maintenance_ticket' AND entity_id = $1 AND module = 'maintenance'
		  AND status = 'ready' AND deleted_at IS NULL
		ORDER BY created_at`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PhotoRead{}
	for rows.Next() {
		var (
			p         PhotoRead
			kind, key string
		)
		if err := rows.Scan(&p.FileID, &kind, &key, &p.CreatedAt); err != nil {
			return nil, err
		}
		p.Stage = "report"
		if kind == "ticket_resolution" {
			p.Stage = "resolution"
		}
		p.URL = s.signed(ctx, tenantID, key)
		out = append(out, p)
	}
	return out, rows.Err()
}

// visibleTicket devuelve el ticket si esta en el alcance (si no, 404).
func (s *Service) visibleTicket(ctx context.Context, q database.Querier, actor authz.Actor, id uuid.UUID) (TicketRead, error) {
	scope, err := scopeOf(ctx, q, actor)
	if err != nil {
		return TicketRead{}, err
	}
	all, ids := scope.SQLArgs()
	rows, err := q.Query(ctx, selectTicket+` WHERE `+ticketScope+` AND t.id = $4`, all, ids, actor.UserID, id)
	if err != nil {
		return TicketRead{}, err
	}
	t, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[TicketRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return TicketRead{}, ErrTicketNotFound
	}
	return t, err
}

func ticketByID(ctx context.Context, q database.Querier, id uuid.UUID) (TicketRead, error) {
	rows, err := q.Query(ctx, selectTicket+` WHERE t.id = $1`, id)
	if err != nil {
		return TicketRead{}, err
	}
	t, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[TicketRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return TicketRead{}, ErrTicketNotFound
	}
	return t, err
}

// addEvent agrega un paso a la linea de tiempo.
func addEvent(ctx context.Context, q database.Querier, ticketID uuid.UUID, kind string, actor *uuid.UUID, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO ticket_events (ticket_id, type, actor_user_id, data) VALUES ($1, $2, $3, $4)`,
		ticketID, kind, actor, raw)
	return err
}

// assetOfPlace valida que el activo sea de la propiedad, de su edificio o
// del edificio del ticket.
func assetOfPlace(ctx context.Context, q database.Querier, assetID, propertyID, buildingID *uuid.UUID) error {
	if assetID == nil {
		return nil
	}
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM assets a WHERE a.id = $1 AND a.is_active
		  AND (a.property_id = $2 OR a.building_id = (SELECT building_id FROM properties WHERE id = $2)
		       OR a.building_id = $3))`,
		*assetID, propertyID, buildingID).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return ErrUnknownAsset.WithField("body.asset_id")
	}
	return nil
}

// expandStatuses traduce "active" a los tres estados abiertos.
func expandStatuses(in []string) []string {
	out := []string{}
	for _, s := range in {
		if s == "active" {
			out = append(out, activeStatuses...)
		} else if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// searchTerms arma el LIKE y, si es un folio ("#42" o "42"), el numero.
func searchTerms(query string) (string, *int) {
	query = strings.ToLower(trim(query))
	if query == "" {
		return "", nil
	}
	if n, err := strconv.Atoi(strings.TrimPrefix(query, "#")); err == nil {
		return "%" + query + "%", &n
	}
	return "%" + query + "%", nil
}

func sameID(a, b *uuid.UUID) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func sameCost(a, b *float64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
