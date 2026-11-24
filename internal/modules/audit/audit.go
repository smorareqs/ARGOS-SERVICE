// Package audit registra quien cambio que: la bitacora de cada empresa
// (tabla audit_log del schema del tenant) y la de la plataforma
// (shared.platform_audit_log). Ambas son de solo insercion.
//
// Los servicios llaman a Record o RecordPlatform con su propia transaccion,
// asi el registro existe solo si el cambio se confirma.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/reqctx"
)

// Entry es un cambio dentro de una empresa.
type Entry struct {
	Actor      *uuid.UUID
	Action     string // p. ej. "member.role_changed"
	Entity     string // p. ej. "membership"
	EntityID   *uuid.UUID
	PropertyID *uuid.UUID
	Before     any
	After      any
}

// Record inserta en audit_log del tenant activo (el search_path de q).
func Record(ctx context.Context, q database.Querier, e Entry) error {
	before, err := optionalJSON(e.Before)
	if err != nil {
		return err
	}
	after, err := optionalJSON(e.After)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `
		INSERT INTO audit_log (actor_user_id, action, entity, entity_id, property_id, before, after, ip, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::inet, $9)`,
		e.Actor, e.Action, e.Entity, e.EntityID, e.PropertyID, before, after,
		reqctx.ValidIP(reqctx.IP(ctx)), reqctx.OptionalString(reqctx.UserAgent(ctx)))
	if err != nil {
		return fmt.Errorf("registrando en la bitacora: %w", err)
	}
	return nil
}

// PlatformEntry es una accion de administracion de la plataforma.
type PlatformEntry struct {
	Actor    *uuid.UUID
	TenantID *uuid.UUID
	Action   string // p. ej. "tenant.created"
	Entity   string
	EntityID *uuid.UUID
	Metadata map[string]any
}

// RecordPlatform inserta en shared.platform_audit_log.
func RecordPlatform(ctx context.Context, q database.Querier, e PlatformEntry) error {
	meta := e.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	_, err := q.Exec(ctx, `
		INSERT INTO shared.platform_audit_log (actor_user_id, tenant_id, action, entity, entity_id, metadata, ip)
		VALUES ($1, $2, $3, $4, $5, $6, $7::inet)`,
		e.Actor, e.TenantID, e.Action, e.Entity, e.EntityID, meta, reqctx.ValidIP(reqctx.IP(ctx)))
	if err != nil {
		return fmt.Errorf("registrando en la bitacora de plataforma: %w", err)
	}
	return nil
}

// PlatformRecord es una fila de la bitacora de plataforma para leer (W-P4).
type PlatformRecord struct {
	ID         uuid.UUID      `db:"id" json:"id" format:"uuid"`
	ActorID    *uuid.UUID     `db:"actor_user_id" json:"actor_id" format:"uuid" nullable:"true"`
	ActorName  *string        `db:"actor_name" json:"actor_name" nullable:"true"`
	ActorEmail *string        `db:"actor_email" json:"actor_email" nullable:"true"`
	TenantID   *uuid.UUID     `db:"tenant_id" json:"tenant_id" format:"uuid" nullable:"true"`
	TenantName *string        `db:"tenant_name" json:"tenant_name" nullable:"true"`
	Action     string         `db:"action" json:"action"`
	Entity     string         `db:"entity" json:"entity"`
	EntityID   *uuid.UUID     `db:"entity_id" json:"entity_id" format:"uuid" nullable:"true"`
	Metadata   map[string]any `db:"metadata" json:"metadata"`
	IP         *string        `db:"ip" json:"ip" nullable:"true"`
	CreatedAt  time.Time      `db:"created_at" json:"created_at"`
}

// PlatformFilter filtra la bitacora de plataforma.
type PlatformFilter struct {
	TenantID *uuid.UUID
	Action   string
	Limit    int
	Offset   int
}

// ListPlatform devuelve la bitacora de plataforma, lo mas reciente primero.
func ListPlatform(ctx context.Context, q database.Querier, f PlatformFilter) ([]PlatformRecord, error) {
	rows, err := q.Query(ctx, `
		SELECT l.id, l.actor_user_id, u.full_name AS actor_name, u.email AS actor_email,
		       l.tenant_id, t.name AS tenant_name, l.action, l.entity, l.entity_id, l.metadata,
		       host(l.ip) AS ip, l.created_at
		FROM shared.platform_audit_log l
		LEFT JOIN shared.users u ON u.id = l.actor_user_id
		LEFT JOIN shared.tenants t ON t.id = l.tenant_id
		WHERE ($1::uuid IS NULL OR l.tenant_id = $1)
		  AND ($2 = '' OR l.action LIKE $2 || '%')
		ORDER BY l.created_at DESC, l.id
		LIMIT $3 OFFSET $4`, f.TenantID, f.Action, f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[PlatformRecord])
}

func optionalJSON(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("serializando la bitacora: %w", err)
	}
	return b, nil
}
