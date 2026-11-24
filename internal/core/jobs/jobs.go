// Package jobs es la cola de trabajos en segundo plano sobre PostgreSQL
// (tabla shared.jobs, sin Redis; fase01.md §3.9).
//
// Los servicios encolan con Enqueue dentro de su propia transaccion: el
// trabajo existe solo si el cambio que lo origina se confirma. El worker
// (`argos worker`) toma trabajos con SELECT ... FOR UPDATE SKIP LOCKED, asi
// varios procesos pueden trabajar a la vez sin tomar el mismo.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
)

// Tipos de trabajo de la fase 1. Cada modulo declara los suyos.
const (
	TypeSendEmail      = "send_email"
	TypeStorageCleanup = "storage_cleanup"
	// TypeRetentionCleanup borra de S3 los archivos con expires_at vencido
	// (fotos del documento de huespedes, fase 4) y deja su registro deleted.
	TypeRetentionCleanup = "retention_cleanup"
)

// notifyChannel despierta al worker cuando se encola un trabajo.
const notifyChannel = "jobs_new"

// Job es un trabajo por encolar.
type Job struct {
	Type    string
	Payload any
	// TenantID es la empresa del trabajo: el handler corre con su search_path.
	// nil = trabajo de plataforma.
	TenantID *uuid.UUID
	// ScheduledAt programa el trabajo; cero = ahora.
	ScheduledAt time.Time
	// DedupeKey evita encolar dos veces el mismo trabajo pendiente.
	DedupeKey string
	// MaxAttempts; cero = 5.
	MaxAttempts int
}

// Enqueue inserta el trabajo usando q (la transaccion del servicio). Si ya hay
// uno pendiente o en curso con la misma DedupeKey, no hace nada.
func Enqueue(ctx context.Context, q database.Querier, j Job) error {
	payload, err := json.Marshal(j.Payload)
	if err != nil {
		return fmt.Errorf("serializando el trabajo %s: %w", j.Type, err)
	}
	if j.Payload == nil {
		payload = []byte("{}")
	}
	scheduled := j.ScheduledAt
	if scheduled.IsZero() {
		scheduled = time.Now()
	}
	maxAttempts := j.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	var dedupe *string
	if j.DedupeKey != "" {
		dedupe = &j.DedupeKey
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO shared.jobs (tenant_id, type, payload, scheduled_at, max_attempts, dedupe_key)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (dedupe_key) WHERE status IN ('pending', 'running') DO NOTHING`,
		j.TenantID, j.Type, payload, scheduled, maxAttempts, dedupe,
	); err != nil {
		return fmt.Errorf("encolando %s: %w", j.Type, err)
	}
	// pg_notify dentro de la transaccion se entrega al confirmar.
	_, err = q.Exec(ctx, "SELECT pg_notify($1, $2)", notifyChannel, j.Type)
	return err
}

// Email es el payload de send_email.
type Email struct {
	To       string         `json:"to"`
	Template string         `json:"template"`
	Data     map[string]any `json:"data"`
}

// EnqueueEmail encola un correo con plantilla.
func EnqueueEmail(ctx context.Context, q database.Querier, tenantID *uuid.UUID, e Email) error {
	return Enqueue(ctx, q, Job{Type: TypeSendEmail, Payload: e, TenantID: tenantID})
}
