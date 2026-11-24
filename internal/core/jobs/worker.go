package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
)

// Ajustes del worker (fase01.md §3.9).
const (
	pollInterval   = time.Second
	orphanAfter    = 15 * time.Minute
	handlerTimeout = 5 * time.Minute
	maintenanceGap = time.Minute
)

// Running es un trabajo tomado por el worker.
type Running struct {
	ID       uuid.UUID
	Type     string
	Payload  json.RawMessage
	TenantID *uuid.UUID
	Attempt  int
}

// Handler ejecuta un trabajo. El contexto trae el schema del tenant del
// trabajo (database.InTx lo aplica). Un error lo reintenta con espera
// creciente hasta agotar los intentos.
type Handler func(ctx context.Context, job Running) error

type registration struct {
	handler Handler
	// scrub borra el payload al terminar (p. ej. correos con enlaces de un
	// solo uso: no deben quedar legibles en la tabla).
	scrub bool
}

// Option ajusta el registro de un handler.
type Option func(*registration)

// ScrubPayload vacia el payload del trabajo cuando termina, con exito o sin el.
func ScrubPayload() Option { return func(r *registration) { r.scrub = true } }

// Recurring es un trabajo que el worker encola cada Every (con DedupeKey para
// no duplicarlo).
type Recurring struct {
	Type  string
	Every time.Duration
}

// Registry asocia tipos de trabajo con su handler. El registro es
// append-only: cada modulo agrega los suyos (reparto.md §2).
type Registry struct {
	handlers  map[string]registration
	recurring []Recurring
}

// NewRegistry crea un registro vacio.
func NewRegistry() *Registry {
	return &Registry{handlers: map[string]registration{}}
}

// Register asocia el handler al tipo.
func (r *Registry) Register(jobType string, h Handler, opts ...Option) {
	reg := registration{handler: h}
	for _, o := range opts {
		o(&reg)
	}
	r.handlers[jobType] = reg
}

// Every programa un trabajo recurrente.
func (r *Registry) Every(jobType string, every time.Duration) {
	r.recurring = append(r.recurring, Recurring{Type: jobType, Every: every})
}

// Worker toma y ejecuta trabajos.
type Worker struct {
	db          *database.DB
	registry    *Registry
	log         *slog.Logger
	id          string
	concurrency int
	now         func() time.Time
}

// NewWorker crea el worker.
func NewWorker(db *database.DB, registry *Registry, log *slog.Logger, concurrency int) *Worker {
	host, _ := os.Hostname()
	if concurrency <= 0 {
		concurrency = 1
	}
	return &Worker{
		db: db, registry: registry, log: log, concurrency: concurrency, now: time.Now,
		id: fmt.Sprintf("%s-%d-%s", host, os.Getpid(), uuid.NewString()[:8]),
	}
}

// Run trabaja hasta que se cancele ctx.
func (w *Worker) Run(ctx context.Context) error {
	wake := make(chan struct{}, w.concurrency)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); w.listen(ctx, wake) }()
	wg.Add(1)
	go func() { defer wg.Done(); w.maintain(ctx) }()
	for range w.concurrency {
		wg.Add(1)
		go func() { defer wg.Done(); w.loop(ctx, wake) }()
	}
	w.log.InfoContext(ctx, "Worker iniciado", "worker_id", w.id, "concurrency", w.concurrency)
	wg.Wait()
	return nil
}

// loop toma trabajos mientras haya; si no, espera la notificacion o el
// siguiente sondeo.
func (w *Worker) loop(ctx context.Context, wake <-chan struct{}) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-timer.C:
		}
		for ctx.Err() == nil {
			ran, err := w.RunOne(ctx)
			if err != nil {
				w.log.ErrorContext(ctx, "Error tomando trabajos", "error", err)
				break
			}
			if !ran {
				break
			}
		}
		timer.Reset(pollInterval)
	}
}

// listen despierta a los goroutines cuando llega `NOTIFY jobs_new`. Si la
// conexion se cae, reintenta: el sondeo cubre mientras tanto.
func (w *Worker) listen(ctx context.Context, wake chan<- struct{}) {
	for ctx.Err() == nil {
		if err := w.listenOnce(ctx, wake); err != nil && ctx.Err() == nil {
			w.log.WarnContext(ctx, "LISTEN interrumpido; se reintenta", "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (w *Worker) listenOnce(ctx context.Context, wake chan<- struct{}) error {
	conn, err := w.db.Pool().Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return err
	}
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			return err
		}
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// maintain devuelve a pending los trabajos huerfanos y encola los
// recurrentes.
func (w *Worker) maintain(ctx context.Context) {
	ticker := time.NewTicker(maintenanceGap)
	defer ticker.Stop()
	for {
		if err := w.Maintain(ctx); err != nil && ctx.Err() == nil {
			w.log.ErrorContext(ctx, "Mantenimiento de la cola", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Maintain ejecuta una pasada de mantenimiento (exportada para pruebas).
func (w *Worker) Maintain(ctx context.Context) error {
	return w.db.InTx(ctx, func(q database.Querier) error {
		tag, err := q.Exec(ctx, `
			UPDATE shared.jobs SET status = 'pending', locked_at = NULL, locked_by = NULL, updated_at = now()
			WHERE status = 'running' AND locked_at < now() - make_interval(secs => $1)`, orphanAfter.Seconds())
		if err != nil {
			return err
		}
		if n := tag.RowsAffected(); n > 0 {
			w.log.WarnContext(ctx, "Trabajos huerfanos devueltos a la cola", "count", n)
		}
		for _, r := range w.registry.recurring {
			// La clave cambia con cada periodo: uno por periodo como maximo.
			slot := w.now().Truncate(r.Every).Unix()
			if err := Enqueue(ctx, q, Job{
				Type: r.Type, ScheduledAt: w.now(), DedupeKey: fmt.Sprintf("%s:%d", r.Type, slot), MaxAttempts: 1,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// RunOne toma y ejecuta un trabajo. Devuelve false si no habia ninguno.
func (w *Worker) RunOne(ctx context.Context) (bool, error) {
	job, schema, err := w.claim(ctx)
	if err != nil || job == nil {
		return false, err
	}
	reg, ok := w.registry.handlers[job.Type]
	runErr := errors.New("no hay handler para el tipo " + job.Type)
	if ok {
		runErr = w.execute(ctx, reg.handler, *job, schema)
	}
	return true, w.finish(ctx, *job, reg, runErr)
}

func (w *Worker) execute(ctx context.Context, h Handler, job Running, schema string) (err error) {
	hctx, cancel := context.WithTimeout(ctx, handlerTimeout)
	defer cancel()
	if schema != "" {
		hctx = database.WithTenantSchema(hctx, schema)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return h(hctx, job)
}

func (w *Worker) claim(ctx context.Context) (*Running, string, error) {
	var (
		job    Running
		schema *string
	)
	err := w.db.InTx(ctx, func(q database.Querier) error {
		return q.QueryRow(ctx, `
			UPDATE shared.jobs j
			SET status = 'running', locked_at = now(), locked_by = $1, attempts = attempts + 1, updated_at = now()
			WHERE j.id = (
				SELECT id FROM shared.jobs
				WHERE status = 'pending' AND scheduled_at <= now()
				ORDER BY scheduled_at
				FOR UPDATE SKIP LOCKED
				LIMIT 1)
			RETURNING j.id, j.type, j.payload, j.tenant_id, j.attempts,
			          (SELECT schema_name FROM shared.tenants t WHERE t.id = j.tenant_id)`, w.id,
		).Scan(&job.ID, &job.Type, &job.Payload, &job.TenantID, &job.Attempt, &schema)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	s := ""
	if schema != nil {
		s = *schema
	}
	return &job, s, nil
}

// finish marca el resultado. Un fallo con intentos restantes vuelve a pending
// con espera de 1, 2, 4, 8 y 16 minutos.
func (w *Worker) finish(ctx context.Context, job Running, reg registration, runErr error) error {
	return w.db.InTx(ctx, func(q database.Querier) error {
		if runErr == nil {
			_, err := q.Exec(ctx, `
				UPDATE shared.jobs SET status = 'done', finished_at = now(), locked_at = NULL, last_error = NULL,
				       payload = CASE WHEN $2 THEN '{}'::jsonb ELSE payload END, updated_at = now()
				WHERE id = $1`, job.ID, reg.scrub)
			return err
		}
		message := runErr.Error()
		if len(message) > 2000 {
			message = message[:2000]
		}
		w.log.WarnContext(ctx, "Trabajo fallido", "job_id", job.ID, "type", job.Type, "attempt", job.Attempt, "error", message)
		backoff := time.Duration(1<<min(max(job.Attempt-1, 0), 4)) * time.Minute
		// Los CASE leen los valores previos de la fila, asi que todos usan la
		// misma condicion de "ultimo intento".
		_, err := q.Exec(ctx, `
			UPDATE shared.jobs SET
			  status = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
			  finished_at = CASE WHEN attempts >= max_attempts THEN now() END,
			  scheduled_at = CASE WHEN attempts >= max_attempts THEN scheduled_at ELSE now() + make_interval(secs => $2) END,
			  payload = CASE WHEN attempts >= max_attempts AND $4 THEN '{}'::jsonb ELSE payload END,
			  locked_at = NULL, locked_by = NULL, last_error = $3, updated_at = now()
			WHERE id = $1`, job.ID, backoff.Seconds(), message, reg.scrub)
		return err
	})
}
