// Package alerts guarda las alertas operativas de cada empresa (fase03.md §2 y
// §3): un activo con falla, un preventivo vencido, una tarea atrasada, stock
// bajo, una discrepancia de inventario. Cada causa tiene a lo mas una alerta
// viva (indice uq_alerts_open_entity): levantarla otra vez solo actualiza el
// mensaje. Se reconocen ("ya lo vi") y se resuelven a mano o solas cuando la
// causa desaparece (ResolveFor).
//
// Los modulos levantan alertas con Service.Raise dentro de su propia
// transaccion; cada alerta nueva publica el evento EventRaised, al que se
// suscribe notificaciones (Persona B).
package alerts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/paging"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Tipos de alerta (CHECK ck_alerts_type).
const (
	TypeAssetFailure       = "asset_failure"
	TypeMaintenanceOverdue = "maintenance_overdue"
	TypeTaskLate           = "task_late"
	TypeLowStock           = "low_stock"
	TypeDiscrepancyOpen    = "discrepancy_open"
	TypeCheckoutAlarm      = "checkout_alarm_unattended"
)

// Severidades.
const (
	SeverityLow    = "low"
	SeverityMedium = "medium"
	SeverityHigh   = "high"
)

// Estados.
const (
	StatusActive       = "active"
	StatusAcknowledged = "acknowledged"
	StatusResolved     = "resolved"
)

// EventRaised se publica al crear una alerta (no al actualizar una viva).
// Data: AlertRead.
const EventRaised = "alert.raised"

// Errores del modulo.
var (
	ErrAlertNotFound   = apperr.New(apperr.KindNotFound, "La alerta no existe.")
	ErrAlreadyResolved = apperr.New(apperr.KindValidation, "La alerta ya está resuelta.")
	ErrInvalidAlert    = apperr.New(apperr.KindValidation, "Alerta inválida.")
)

// AlertRead es una alerta con la propiedad y las personas resueltas.
type AlertRead struct {
	ID         uuid.UUID `db:"id" json:"id" format:"uuid"`
	Type       string    `db:"type" json:"type" enum:"asset_failure,maintenance_overdue,task_late,low_stock,discrepancy_open,checkout_alarm_unattended"`
	Severity   string    `db:"severity" json:"severity" enum:"low,medium,high"`
	Status     string    `db:"status" json:"status" enum:"active,acknowledged,resolved"`
	EntityType string    `db:"entity_type" json:"entity_type" doc:"Que la causa: asset, maintenance_plan, maintenance_ticket, task, inventory_item, inventory_discrepancy, reservation."`
	EntityID   uuid.UUID `db:"entity_id" json:"entity_id" format:"uuid"`

	PropertyID   *uuid.UUID `db:"property_id" json:"property_id" format:"uuid" nullable:"true"`
	PropertyCode *string    `db:"property_code" json:"property_code" nullable:"true"`
	PropertyName *string    `db:"property_name" json:"property_name" nullable:"true"`

	Message            string     `db:"message" json:"message"`
	CreatedAt          time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt          time.Time  `db:"updated_at" json:"updated_at"`
	AcknowledgedAt     *time.Time `db:"acknowledged_at" json:"acknowledged_at" nullable:"true"`
	AcknowledgedByName *string    `db:"acknowledged_by_name" json:"acknowledged_by_name" nullable:"true"`
	ResolvedAt         *time.Time `db:"resolved_at" json:"resolved_at" nullable:"true"`
	ResolvedByName     *string    `db:"resolved_by_name" json:"resolved_by_name" nullable:"true" doc:"Vacio si se resolvio sola."`
}

// Raise es una alerta por levantar.
type Raise struct {
	Type       string
	Severity   string
	EntityType string
	EntityID   uuid.UUID
	PropertyID *uuid.UUID
	Message    string
}

// Filter son los filtros de la lista.
type Filter struct {
	// Status: active, acknowledged, resolved u "open" (las dos primeras). Vacio = open.
	Status     string
	Type       string
	Severity   string
	PropertyID *uuid.UUID
	Cursor     string
	Limit      int
}

// TypeCount es el resumen de un tipo.
type TypeCount struct {
	Type  string `json:"type" enum:"asset_failure,maintenance_overdue,task_late,low_stock,discrepancy_open,checkout_alarm_unattended"`
	Open  int    `json:"open" doc:"Activas y reconocidas."`
	High  int    `json:"high" doc:"Abiertas de severidad alta."`
	Fresh int    `json:"fresh" doc:"Activas sin reconocer."`
}

// Summary son los contadores de alertas abiertas del alcance (campana, Inicio
// y tarjetas de W-L1).
type Summary struct {
	Open   int         `json:"open"`
	High   int         `json:"high"`
	Fresh  int         `json:"fresh" doc:"Activas que nadie ha reconocido."`
	ByType []TypeCount `json:"by_type" doc:"Solo los tipos con alertas abiertas."`
}

// Service contiene los casos de uso de alertas.
type Service struct {
	db  *database.DB
	bus *events.Bus
	log *slog.Logger
}

// NewService crea el servicio.
func NewService(db *database.DB, bus *events.Bus, log *slog.Logger) *Service {
	return &Service{db: db, bus: bus, log: log}
}

const selectAlert = `
	SELECT a.id, a.type, a.severity, a.status, a.entity_type, a.entity_id, a.property_id,
	       p.code AS property_code, p.name AS property_name, a.message, a.created_at, a.updated_at,
	       a.acknowledged_at, ua.full_name AS acknowledged_by_name, a.resolved_at, ur.full_name AS resolved_by_name
	FROM alerts a
	LEFT JOIN properties p ON p.id = a.property_id
	LEFT JOIN shared.users ua ON ua.id = a.acknowledged_by
	LEFT JOIN shared.users ur ON ur.id = a.resolved_by`

// Raise levanta la alerta de esa causa con q (la transaccion de quien la
// detecta). Si ya hay una viva, actualiza mensaje y severidad y devuelve
// created = false. tenantID va en el evento.
func (s *Service) Raise(ctx context.Context, q database.Querier, tenantID uuid.UUID, r Raise) (AlertRead, bool, error) {
	if r.Type == "" || r.EntityType == "" || r.EntityID == uuid.Nil || r.Message == "" {
		return AlertRead{}, false, ErrInvalidAlert
	}
	if r.Severity == "" {
		r.Severity = SeverityMedium
	}
	if len([]rune(r.Message)) > 255 {
		r.Message = string([]rune(r.Message)[:254]) + "…"
	}
	var (
		id      uuid.UUID
		created bool
	)
	err := q.QueryRow(ctx, `
		INSERT INTO alerts (type, severity, entity_type, entity_id, property_id, message)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (type, entity_type, entity_id) WHERE status <> 'resolved' DO UPDATE
		   SET severity = EXCLUDED.severity, message = EXCLUDED.message,
		       property_id = EXCLUDED.property_id, updated_at = now()
		RETURNING id, (xmax = 0)`,
		r.Type, r.Severity, r.EntityType, r.EntityID, r.PropertyID, r.Message).Scan(&id, &created)
	if err != nil {
		return AlertRead{}, false, fmt.Errorf("levantando la alerta %s: %w", r.Type, err)
	}
	a, err := byID(ctx, q, id)
	if err != nil || !created {
		return a, created, err
	}
	return a, true, s.bus.Publish(ctx, q, events.Event{Type: EventRaised, TenantID: tenantID, Data: a})
}

// ResolveFor resuelve la alerta viva de esa causa (la causa desaparecio: el
// activo volvio a operar, el stock se repuso). by = nil si la resuelve el
// sistema. Devuelve false si no habia ninguna.
func ResolveFor(ctx context.Context, q database.Querier, alertType, entityType string, entityID uuid.UUID, by *uuid.UUID) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE alerts SET status = 'resolved', resolved_at = now(), resolved_by = $4, updated_at = now()
		WHERE type = $1 AND entity_type = $2 AND entity_id = $3 AND status <> 'resolved'`,
		alertType, entityType, entityID, by)
	if err != nil {
		return false, fmt.Errorf("resolviendo la alerta %s: %w", alertType, err)
	}
	return tag.RowsAffected() > 0, nil
}

// List devuelve una pagina de alertas del alcance, las mas nuevas primero.
func (s *Service) List(ctx context.Context, actor authz.Actor, f Filter) ([]AlertRead, *string, error) {
	limit := paging.Limit(f.Limit)
	key, err := paging.Decode(f.Cursor, 2)
	if err != nil {
		return nil, nil, err
	}
	items, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]AlertRead, error) {
		scope, err := authz.ResolvePropertyScope(ctx, q, actor)
		if err != nil {
			return nil, err
		}
		all, ids := scope.SQLArgs()
		var afterAt, afterID *string
		if key != nil {
			afterAt, afterID = &key[0], &key[1]
		}
		rows, err := q.Query(ctx, selectAlert+`
			WHERE ($1::bool OR a.property_id = ANY($2::uuid[]))
			  AND CASE $3::text WHEN 'open' THEN a.status <> 'resolved' ELSE a.status = $3 END
			  AND ($4::text = '' OR a.type = $4)
			  AND ($5::text = '' OR a.severity = $5)
			  AND ($6::uuid IS NULL OR a.property_id = $6)
			  AND ($7::timestamptz IS NULL OR (a.created_at, a.id) < ($7::timestamptz, $8::uuid))
			ORDER BY a.created_at DESC, a.id DESC
			LIMIT $9`,
			all, ids, statusOrOpen(f.Status), f.Type, f.Severity, f.PropertyID, afterAt, afterID, limit+1)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[AlertRead])
	})
	if err != nil {
		return nil, nil, err
	}
	items, next := paging.Page(items, limit, func(a AlertRead) []string {
		return []string{a.CreatedAt.Format(time.RFC3339Nano), a.ID.String()}
	})
	return items, next, nil
}

// Summarize cuenta las alertas abiertas del alcance por tipo.
func (s *Service) Summarize(ctx context.Context, actor authz.Actor) (Summary, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Summary, error) {
		scope, err := authz.ResolvePropertyScope(ctx, q, actor)
		if err != nil {
			return Summary{}, err
		}
		return Summarize(ctx, q, scope)
	})
}

// Summarize es el resumen con una transaccion ya abierta (lo reusa el Inicio).
func Summarize(ctx context.Context, q database.Querier, scope authz.PropertyScope) (Summary, error) {
	all, ids := scope.SQLArgs()
	out := Summary{ByType: []TypeCount{}}
	rows, err := q.Query(ctx, `
		SELECT type, count(*)::int, count(*) FILTER (WHERE severity = 'high')::int,
		       count(*) FILTER (WHERE status = 'active')::int
		FROM alerts a
		WHERE a.status <> 'resolved' AND ($1::bool OR a.property_id = ANY($2::uuid[]))
		GROUP BY type ORDER BY count(*) FILTER (WHERE severity = 'high') DESC, count(*) DESC, type`, all, ids)
	if err != nil {
		return Summary{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var c TypeCount
		if err := rows.Scan(&c.Type, &c.Open, &c.High, &c.Fresh); err != nil {
			return Summary{}, err
		}
		out.ByType = append(out.ByType, c)
		out.Open += c.Open
		out.High += c.High
		out.Fresh += c.Fresh
	}
	return out, rows.Err()
}

// Acknowledge marca la alerta como vista: deja de contar como nueva pero
// sigue abierta hasta resolverse.
func (s *Service) Acknowledge(ctx context.Context, actor authz.Actor, id uuid.UUID) (AlertRead, error) {
	return s.transition(ctx, actor, id, "alert.acknowledged", `
		UPDATE alerts SET status = 'acknowledged', acknowledged_by = $2, acknowledged_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'active'`)
}

// Resolve cierra la alerta a mano. Si la causa sigue (el job la vuelve a
// detectar), se levanta una nueva.
func (s *Service) Resolve(ctx context.Context, actor authz.Actor, id uuid.UUID) (AlertRead, error) {
	return s.transition(ctx, actor, id, "alert.resolved", `
		UPDATE alerts SET status = 'resolved', resolved_by = $2, resolved_at = now(), updated_at = now()
		WHERE id = $1 AND status <> 'resolved'`)
}

func (s *Service) transition(ctx context.Context, actor authz.Actor, id uuid.UUID, action, update string) (AlertRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (AlertRead, error) {
		cur, err := s.visible(ctx, q, actor, id)
		if err != nil {
			return AlertRead{}, err
		}
		if cur.Status == StatusResolved {
			return AlertRead{}, ErrAlreadyResolved
		}
		tag, err := q.Exec(ctx, update, id, actor.UserID)
		if err != nil {
			return AlertRead{}, err
		}
		if tag.RowsAffected() == 0 {
			return cur, nil // ya estaba reconocida
		}
		after, err := byID(ctx, q, id)
		if err != nil {
			return AlertRead{}, err
		}
		return after, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: action, Entity: "alert", EntityID: &id, PropertyID: after.PropertyID,
			Before: map[string]any{"status": cur.Status}, After: map[string]any{"status": after.Status, "type": after.Type},
		})
	})
}

// visible devuelve la alerta si esta en el alcance del actor (si no, 404).
func (s *Service) visible(ctx context.Context, q database.Querier, actor authz.Actor, id uuid.UUID) (AlertRead, error) {
	a, err := byID(ctx, q, id)
	if err != nil {
		return AlertRead{}, err
	}
	if !actor.Scoped {
		return a, nil
	}
	scope, err := authz.ResolvePropertyScope(ctx, q, actor)
	if err != nil {
		return AlertRead{}, err
	}
	if scope.All || (a.PropertyID != nil && scope.Allows(*a.PropertyID)) {
		return a, nil
	}
	return AlertRead{}, ErrAlertNotFound
}

func byID(ctx context.Context, q database.Querier, id uuid.UUID) (AlertRead, error) {
	rows, err := q.Query(ctx, selectAlert+` WHERE a.id = $1`, id)
	if err != nil {
		return AlertRead{}, err
	}
	a, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[AlertRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return AlertRead{}, ErrAlertNotFound
	}
	return a, err
}

func statusOrOpen(s string) string {
	if s == "" {
		return "open"
	}
	return s
}

// Top devuelve las n alertas abiertas mas importantes del alcance: severidad
// alta primero, sin reconocer primero, las mas nuevas primero (Inicio).
func Top(ctx context.Context, q database.Querier, scope authz.PropertyScope, n int) ([]AlertRead, error) {
	all, ids := scope.SQLArgs()
	rows, err := q.Query(ctx, selectAlert+`
		WHERE a.status <> 'resolved' AND ($1::bool OR a.property_id = ANY($2::uuid[]))
		ORDER BY CASE a.severity WHEN 'high' THEN 0 WHEN 'medium' THEN 1 ELSE 2 END,
		         a.status = 'acknowledged', a.created_at DESC
		LIMIT $3`, all, ids, n)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[AlertRead])
}
