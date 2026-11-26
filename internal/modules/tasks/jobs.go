package tasks

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/jobs"
)

// HandleJob corre mark_late_tasks: sin empresa encola uno por cada empresa
// activa; con empresa marca sus tareas retrasadas.
func (s *Service) HandleJob(ctx context.Context, job jobs.Running) error {
	if job.TenantID == nil {
		return s.fanOut(ctx, job.Type)
	}
	if job.Type != JobMarkLate {
		return fmt.Errorf("trabajo desconocido %s", job.Type)
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		_, err := s.MarkLate(ctx, q, *job.TenantID)
		return err
	})
}

func (s *Service) fanOut(ctx context.Context, jobType string) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		rows, err := q.Query(ctx, `SELECT id FROM shared.tenants WHERE is_active ORDER BY slug`)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := jobs.Enqueue(ctx, q, jobs.Job{
				Type: jobType, TenantID: &id, DedupeKey: jobType + ":" + id.String(), MaxAttempts: 3,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// MarkLate marca is_late (fase02.md §3) en la hora local de la empresa: las
// que pasaron su hora de inicio sin iniciar o su hora de fin sin terminar.
// Cada una recibe su evento late y se publica EventLate. Devuelve cuantas marco.
func (s *Service) MarkLate(ctx context.Context, q database.Querier, tenantID uuid.UUID) (int, error) {
	rows, err := q.Query(ctx, `
		WITH now_local AS (SELECT ($1::timestamptz AT TIME ZONE `+tenantTZ+`) AS ts)
		UPDATE tasks t SET is_late = true, updated_at = now()
		FROM now_local n
		WHERE NOT t.is_late AND t.status IN ('pending', 'in_progress', 'returned')
		  AND ((t.status = 'pending' AND t.scheduled_date + coalesce(t.window_start, '08:00'::time) < n.ts)
		       OR t.scheduled_date + coalesce(t.window_end, '17:00'::time) < n.ts)
		RETURNING t.id, CASE WHEN t.status = 'pending' AND t.scheduled_date + coalesce(t.window_end, '17:00'::time) >= n.ts
		                     THEN 'not_started' ELSE 'not_finished' END`, s.now())
	if err != nil {
		return 0, err
	}
	type late struct {
		ID     uuid.UUID
		Reason string
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[late])
	if err != nil {
		return 0, err
	}
	for _, l := range list {
		if err := addEvent(ctx, q, l.ID, "late", nil, map[string]any{"reason": l.Reason}); err != nil {
			return 0, err
		}
		t, err := s.byID(ctx, q, l.ID)
		if err != nil {
			return 0, err
		}
		if err := s.bus.Publish(ctx, q, events.Event{Type: EventLate, TenantID: tenantID, Data: TaskEvent{Task: t}}); err != nil {
			return 0, err
		}
	}
	return len(list), nil
}
