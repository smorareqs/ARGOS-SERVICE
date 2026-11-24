package health

import (
	"context"
	"errors"
	"time"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/migrate"
	"github.com/smorareqs/argos-service/internal/core/storage"
)

// readyTimeout acota cada verificacion de /health/ready.
const readyTimeout = 5 * time.Second

// Ready es la respuesta de /health/ready (fase06.md §5): el balanceador solo
// manda trafico a una instancia con BD, S3 y migraciones al dia.
type Ready struct {
	Status            string `json:"status" enum:"ready,not_ready"`
	Database          string `json:"database" enum:"up,down"`
	Storage           string `json:"storage" enum:"up,down,memory"`
	Migrations        string `json:"migrations" enum:"up_to_date,pending,unknown"`
	PendingMigrations int    `json:"pending_migrations" doc:"Migraciones sin aplicar en shared y en todas las empresas."`
	Queue             Queue  `json:"queue"`
}

// Queue resume la cola de trabajos (observabilidad del worker).
type Queue struct {
	Pending       int `json:"pending"`
	Running       int `json:"running"`
	Failed24h     int `json:"failed_24h"`
	OldestPending int `json:"oldest_pending_seconds" doc:"Antigüedad del trabajo pendiente más viejo que ya debía correr."`
}

// Readiness son las dependencias de /health/ready.
type Readiness struct {
	DB      *database.DB
	Storage *storage.Service
	Shared  migrate.Set
	Tenant  migrate.Set
}

// WithReadiness habilita Ready.
func (s *Service) WithReadiness(r Readiness) *Service {
	s.ready = &r
	return s
}

// Ready verifica BD, S3, migraciones y la cola.
func (s *Service) Ready(ctx context.Context) Ready {
	out := Ready{Status: "not_ready", Database: "down", Storage: "down", Migrations: "unknown"}
	if s.ready == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := s.db.Ping(ctx); err == nil {
		out.Database = "up"
	}
	out.Storage = s.storageStatus(ctx)
	if out.Database == "up" {
		if n, err := s.pendingMigrations(ctx); err == nil {
			out.PendingMigrations, out.Migrations = n, "up_to_date"
			if n > 0 {
				out.Migrations = "pending"
			}
		}
		out.Queue, _ = s.queue(ctx)
	}
	if out.Database == "up" && out.Storage != "down" && out.Migrations == "up_to_date" {
		out.Status = "ready"
	}
	return out
}

func (s *Service) storageStatus(ctx context.Context) string {
	backend := s.ready.Storage.Backend()
	if _, ok := backend.(*storage.Memory); ok {
		return "memory"
	}
	_, err := backend.Head(ctx, "health/ready-probe")
	if err == nil || errors.Is(err, storage.ErrNotFound) {
		return "up"
	}
	return "down"
}

func (s *Service) pendingMigrations(ctx context.Context) (int, error) {
	return database.InTxValue(ctx, s.ready.DB, func(q database.Querier) (int, error) {
		states, err := s.ready.Shared.Status(ctx, q, "shared")
		if err != nil {
			return 0, err
		}
		n := migrate.Pending(states)
		rows, err := q.Query(ctx, `SELECT schema_name FROM shared.tenants ORDER BY schema_name`)
		if err != nil {
			return 0, err
		}
		var schemas []string
		for rows.Next() {
			var sc string
			if err := rows.Scan(&sc); err != nil {
				rows.Close()
				return 0, err
			}
			schemas = append(schemas, sc)
		}
		rows.Close()
		for _, sc := range schemas {
			states, err := s.ready.Tenant.Status(ctx, q, sc)
			if err != nil {
				return 0, err
			}
			n += migrate.Pending(states)
		}
		return n, nil
	})
}

func (s *Service) queue(ctx context.Context) (Queue, error) {
	var out Queue
	err := s.ready.DB.InTx(ctx, func(q database.Querier) error {
		return q.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE status = 'pending')::int,
			       count(*) FILTER (WHERE status = 'running')::int,
			       count(*) FILTER (WHERE status = 'failed' AND updated_at > now() - interval '24 hours')::int,
			       coalesce(extract(epoch FROM now() - min(scheduled_at) FILTER (WHERE status = 'pending' AND scheduled_at <= now())), 0)::int
			FROM shared.jobs`).Scan(&out.Pending, &out.Running, &out.Failed24h, &out.OldestPending)
	})
	return out, err
}
