// Package retention es la limpieza de datos viejos y el pulso del worker
// (fase06.md §5, Persona B).
//
// Plazos (retention_cleanup, cada hora):
//   - Fotos del documento del huesped: 90 dias despues de la salida (las marca
//     reservations con expires_at; files.Retention las borra).
//   - Evidencias (fotos de tareas, inspecciones, incidencias, marcajes y chat):
//     24 meses desde que se subieron. Aqui se les pone expires_at y
//     files.Retention borra el objeto de S3 en la misma corrida.
//   - Archivos pendientes que nunca se subieron: 24 h (files.Cleanup).
//   - Avisos leidos: 180 dias.
//   - Trabajos terminados o cancelados: 30 dias; fallidos: 90 dias.
//
// worker_stats (cada 5 min) deja en el log JSON el tamano de la cola, los
// fallos de la ultima hora por tipo y el pendiente mas viejo, para alertar
// desde los logs sin otra herramienta.
package retention

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
)

// JobWorkerStats es el pulso del worker.
const JobWorkerStats = "worker_stats"

// Plazos.
const (
	EvidenceMonths    = 24
	ReadNoticeDays    = 180
	FinishedJobDays   = 30
	FailedJobDays     = 90
	evidenceModuleSQL = `('tasks', 'reservations', 'maintenance', 'attendance', 'chat')`
)

// Result es lo que limpio una corrida.
type Result struct {
	EvidenceMarked int
	NoticesDeleted int
	JobsDeleted    int
}

// Service limpia y mide.
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

// Run marca las evidencias vencidas y borra avisos y trabajos viejos. Despues
// hay que correr files.Retention para borrar los objetos marcados.
func (s *Service) Run(ctx context.Context) (Result, error) {
	var res Result
	now := s.now()
	type tenant struct {
		ID     uuid.UUID `db:"id"`
		Schema string    `db:"schema_name"`
	}
	tenants, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]tenant, error) {
		rows, err := q.Query(ctx, `SELECT id, schema_name FROM shared.tenants ORDER BY slug`)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[tenant])
	})
	if err != nil {
		return res, err
	}
	for _, t := range tenants {
		tctx := database.WithTenantSchema(ctx, t.Schema)
		if err := s.db.InTx(tctx, func(q database.Querier) error {
			tag, err := q.Exec(tctx, `
				UPDATE files SET expires_at = $1::timestamptz, updated_at = now()
				WHERE status = 'ready' AND deleted_at IS NULL AND expires_at IS NULL
				  AND module IN `+evidenceModuleSQL+` AND created_at < $1::timestamptz - make_interval(months => $2)`,
				now, EvidenceMonths)
			if err != nil {
				return err
			}
			res.EvidenceMarked += int(tag.RowsAffected())
			tag, err = q.Exec(tctx, `
				DELETE FROM notifications WHERE read_at IS NOT NULL AND read_at < $1::timestamptz - make_interval(days => $2)`,
				now, ReadNoticeDays)
			if err != nil {
				return err
			}
			res.NoticesDeleted += int(tag.RowsAffected())
			return nil
		}); err != nil {
			return res, fmt.Errorf("retencion de %s: %w", t.Schema, err)
		}
	}
	err = s.db.InTx(ctx, func(q database.Querier) error {
		tag, err := q.Exec(ctx, `
			DELETE FROM shared.jobs
			WHERE (status IN ('done', 'cancelled') AND coalesce(finished_at, updated_at) < $1::timestamptz - make_interval(days => $2))
			   OR (status = 'failed' AND coalesce(finished_at, updated_at) < $1::timestamptz - make_interval(days => $3))`,
			now, FinishedJobDays, FailedJobDays)
		if err != nil {
			return err
		}
		res.JobsDeleted = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return res, err
	}
	if res.EvidenceMarked+res.NoticesDeleted+res.JobsDeleted > 0 {
		s.log.InfoContext(ctx, "Retencion", "evidence_marked", res.EvidenceMarked,
			"notices_deleted", res.NoticesDeleted, "jobs_deleted", res.JobsDeleted)
	}
	return res, nil
}

// Stats es el estado de la cola.
type Stats struct {
	Pending       int            `json:"pending"`
	Running       int            `json:"running"`
	OldestPending int            `json:"oldest_pending_seconds"`
	FailedLastH   map[string]int `json:"failed_last_hour"`
	DoneLastH     int            `json:"done_last_hour"`
	StuckRunning  int            `json:"stuck_running" doc:"Corriendo hace mas de 15 min."`
}

// WorkerStats mide la cola y la deja en el log (WARN si algo se ve mal).
func (s *Service) WorkerStats(ctx context.Context) (Stats, error) {
	st := Stats{FailedLastH: map[string]int{}}
	err := s.db.InTx(ctx, func(q database.Querier) error {
		if err := q.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE status = 'pending')::int,
			       count(*) FILTER (WHERE status = 'running')::int,
			       coalesce(extract(epoch FROM now() - min(scheduled_at) FILTER (WHERE status = 'pending' AND scheduled_at <= now())), 0)::int,
			       count(*) FILTER (WHERE status = 'done' AND finished_at > now() - interval '1 hour')::int,
			       count(*) FILTER (WHERE status = 'running' AND updated_at < now() - interval '15 minutes')::int
			FROM shared.jobs`).Scan(&st.Pending, &st.Running, &st.OldestPending, &st.DoneLastH, &st.StuckRunning); err != nil {
			return err
		}
		rows, err := q.Query(ctx, `
			SELECT type, count(*)::int FROM shared.jobs
			WHERE status = 'failed' AND updated_at > now() - interval '1 hour' GROUP BY type`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var typ string
			var n int
			if err := rows.Scan(&typ, &n); err != nil {
				return err
			}
			st.FailedLastH[typ] = n
		}
		return rows.Err()
	})
	if err != nil {
		return st, err
	}
	failed := 0
	for _, n := range st.FailedLastH {
		failed += n
	}
	attrs := []any{
		"pending", st.Pending, "running", st.Running, "oldest_pending_seconds", st.OldestPending,
		"done_last_hour", st.DoneLastH, "failed_last_hour", failed, "failed_by_type", st.FailedLastH, "stuck_running", st.StuckRunning,
	}
	if failed > 0 || st.OldestPending > 600 || st.StuckRunning > 0 {
		s.log.WarnContext(ctx, "Worker: revisar la cola", attrs...)
	} else {
		s.log.InfoContext(ctx, "Worker: cola al dia", attrs...)
	}
	return st, nil
}
