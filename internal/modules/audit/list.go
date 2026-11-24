package audit

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/paging"
)

// Lectura de la bitacora de la empresa (W-N1, GET /audit con audit.read).

// EntryRead es una fila de la bitacora de la empresa.
type EntryRead struct {
	ID           uuid.UUID      `db:"id" json:"id" format:"uuid"`
	ActorID      *uuid.UUID     `db:"actor_user_id" json:"actor_id" format:"uuid" nullable:"true"`
	ActorName    *string        `db:"actor_name" json:"actor_name" nullable:"true" doc:"null = el sistema."`
	ActorEmail   *string        `db:"actor_email" json:"actor_email" nullable:"true"`
	ActorRole    *string        `db:"actor_role" json:"actor_role" nullable:"true" doc:"Rol actual de la persona en la empresa."`
	Action       string         `db:"action" json:"action" doc:"<entidad>.<verbo>, p. ej. property.updated."`
	Entity       string         `db:"entity" json:"entity"`
	EntityID     *uuid.UUID     `db:"entity_id" json:"entity_id" format:"uuid" nullable:"true"`
	EntityLabel  *string        `db:"entity_label" json:"entity_label" nullable:"true" doc:"Nombre legible de la entidad (codigo, nombre o correo)."`
	PropertyID   *uuid.UUID     `db:"property_id" json:"property_id" format:"uuid" nullable:"true"`
	PropertyCode *string        `db:"property_code" json:"property_code" nullable:"true"`
	PropertyName *string        `db:"property_name" json:"property_name" nullable:"true"`
	Before       map[string]any `db:"before" json:"before" nullable:"true"`
	After        map[string]any `db:"after" json:"after" nullable:"true"`
	IP           *string        `db:"ip" json:"ip" nullable:"true"`
	UserAgent    *string        `db:"user_agent" json:"user_agent" nullable:"true"`
	CreatedAt    time.Time      `db:"created_at" json:"created_at"`
}

// Filter filtra la bitacora. Action es un prefijo (p. ej. "property.").
type Filter struct {
	Entity     string
	EntityID   *uuid.UUID
	PropertyID *uuid.UUID
	ActorID    *uuid.UUID
	Action     string
	From, To   *time.Time
	Cursor     string
	Limit      int
}

const selectRecord = `SELECT l.id, l.actor_user_id, u.full_name AS actor_name, u.email AS actor_email,
       (SELECT r.name FROM shared.memberships m JOIN shared.roles r ON r.id = m.role_id
        WHERE m.user_id = l.actor_user_id AND m.tenant_id = $1) AS actor_role,
       l.action, l.entity, l.entity_id,
       CASE l.entity
         WHEN 'property' THEN (SELECT x.code FROM properties x WHERE x.id = l.entity_id)
         WHEN 'building' THEN (SELECT x.name FROM buildings x WHERE x.id = l.entity_id)
         WHEN 'checklist_template' THEN (SELECT x.name FROM checklist_templates x WHERE x.id = l.entity_id)
         WHEN 'employee' THEN (SELECT eu.full_name FROM employees x JOIN shared.users eu ON eu.id = x.user_id
                               WHERE x.id = l.entity_id)
         WHEN 'user' THEN (SELECT x.full_name FROM shared.users x WHERE x.id = l.entity_id)
         WHEN 'membership' THEN (SELECT mu.full_name FROM shared.memberships x JOIN shared.users mu ON mu.id = x.user_id
                                 WHERE x.id = l.entity_id)
         WHEN 'role' THEN (SELECT x.name FROM shared.roles x WHERE x.id = l.entity_id)
         WHEN 'invitation' THEN (SELECT x.email FROM shared.invitations x WHERE x.id = l.entity_id)
         -- Fase 3: mantenimiento y alertas.
         WHEN 'vendor' THEN (SELECT x.name FROM vendors x WHERE x.id = l.entity_id)
         WHEN 'asset' THEN (SELECT x.name FROM assets x WHERE x.id = l.entity_id)
         WHEN 'maintenance_plan' THEN (SELECT x.name FROM maintenance_plans x WHERE x.id = l.entity_id)
         WHEN 'maintenance_ticket' THEN (SELECT '#' || x.number || ' ' || x.title FROM maintenance_tickets x WHERE x.id = l.entity_id)
         WHEN 'alert' THEN (SELECT x.message FROM alerts x WHERE x.id = l.entity_id)
         WHEN 'reservation' THEN (SELECT x.code FROM reservations x WHERE x.id = l.entity_id)
         WHEN 'guest' THEN (SELECT x.full_name FROM guests x WHERE x.id = l.entity_id)
         WHEN 'booking_channel' THEN (SELECT x.name FROM booking_channels x WHERE x.id = l.entity_id)
         WHEN 'ical_feed' THEN (SELECT c.name || ' · ' || p.code FROM property_ical_feeds x JOIN booking_channels c ON c.id = x.channel_id
                                JOIN properties p ON p.id = x.property_id WHERE x.id = l.entity_id)
         WHEN 'payroll_period' THEN (SELECT x.name FROM payroll_periods x WHERE x.id = l.entity_id)
         WHEN 'payroll_entry' THEN (SELECT x.full_name || ' · ' || pp.name FROM payroll_entries x
                                    JOIN payroll_periods pp ON pp.id = x.period_id WHERE x.id = l.entity_id)
         WHEN 'payroll_settings' THEN 'Configuración de nómina'
         WHEN 'disciplinary_action' THEN (SELECT du.full_name FROM disciplinary_actions x JOIN employees de ON de.id = x.employee_id
                                         JOIN shared.users du ON du.id = de.user_id WHERE x.id = l.entity_id)
       END AS entity_label,
       l.property_id, p.code AS property_code, p.name AS property_name,
       l.before, l.after, host(l.ip) AS ip, l.user_agent, l.created_at
FROM audit_log l
LEFT JOIN shared.users u ON u.id = l.actor_user_id
LEFT JOIN properties p ON p.id = l.property_id`

// List devuelve una pagina de la bitacora de la empresa, lo mas reciente
// primero. q opera en el schema de la empresa tenantID.
func List(ctx context.Context, q database.Querier, tenantID uuid.UUID, f Filter) ([]EntryRead, *string, error) {
	limit := paging.Limit(f.Limit)
	key, err := paging.Decode(f.Cursor, 2)
	if err != nil {
		return nil, nil, err
	}
	var afterAt *time.Time
	var afterID *uuid.UUID
	if key != nil {
		at, err1 := time.Parse(time.RFC3339Nano, key[0])
		id, err2 := uuid.Parse(key[1])
		if err1 != nil || err2 != nil {
			return nil, nil, paging.ErrInvalidCursor
		}
		afterAt, afterID = &at, &id
	}
	rows, err := q.Query(ctx, selectRecord+`
		WHERE ($2::text = '' OR l.entity = $2)
		  AND ($3::uuid IS NULL OR l.entity_id = $3)
		  AND ($4::uuid IS NULL OR l.property_id = $4)
		  AND ($5::uuid IS NULL OR l.actor_user_id = $5)
		  AND ($6::text = '' OR l.action LIKE $6 || '%')
		  AND ($7::timestamptz IS NULL OR l.created_at >= $7)
		  AND ($8::timestamptz IS NULL OR l.created_at < $8)
		  AND ($9::timestamptz IS NULL OR (l.created_at, l.id) < ($9, $10::uuid))
		ORDER BY l.created_at DESC, l.id DESC
		LIMIT $11`,
		tenantID, f.Entity, f.EntityID, f.PropertyID, f.ActorID, f.Action, f.From, f.To, afterAt, afterID, limit+1)
	if err != nil {
		return nil, nil, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByName[EntryRead])
	if err != nil {
		return nil, nil, err
	}
	items, next := paging.Page(items, limit, func(r EntryRead) []string {
		return []string{r.CreatedAt.UTC().Format(time.RFC3339Nano), r.ID.String()}
	})
	return items, next, nil
}

// Service expone la lectura de la bitacora con su propia transaccion.
type Service struct {
	db *database.DB
}

// NewService crea el servicio.
func NewService(db *database.DB) *Service {
	return &Service{db: db}
}

// List es List en una transaccion del schema de la empresa del contexto.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, f Filter) ([]EntryRead, *string, error) {
	type result struct {
		items []EntryRead
		next  *string
	}
	r, err := database.InTxValue(ctx, s.db, func(q database.Querier) (result, error) {
		items, next, err := List(ctx, q, tenantID, f)
		return result{items, next}, err
	})
	return r.items, r.next, err
}
