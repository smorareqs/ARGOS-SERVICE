package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
)

// Env identifica el conjunto de migraciones.
type Env string

// Conjuntos de migraciones.
const (
	EnvShared Env = "shared"
	EnvTenant Env = "tenant"
)

// AllTenants selecciona todos los tenants en las operaciones de EnvTenant.
const AllTenants = "all"

// SchemaStatus es el estado de las migraciones de un schema.
type SchemaStatus struct {
	Schema string
	States []State
}

// Runner orquesta las migraciones contra la base de datos: una transaccion
// por schema, shared antes que los tenants.
type Runner struct {
	db   *database.DB
	sets map[Env]Set
	log  *slog.Logger
}

// NewRunner carga los conjuntos embebidos.
func NewRunner(db *database.DB, log *slog.Logger) (*Runner, error) {
	shared, err := Shared()
	if err != nil {
		return nil, err
	}
	tenant, err := Tenant()
	if err != nil {
		return nil, err
	}
	return &Runner{db: db, sets: map[Env]Set{EnvShared: shared, EnvTenant: tenant}, log: log}, nil
}

// Up aplica las migraciones pendientes. En EnvTenant, tenant es un slug o
// AllTenants; en EnvShared se ignora.
func (r *Runner) Up(ctx context.Context, env Env, tenant string) error {
	set, schemas, err := r.target(ctx, env, tenant)
	if err != nil {
		return err
	}
	for _, schema := range schemas {
		applied, err := database.InTxValue(ctx, r.db, func(q database.Querier) ([]Migration, error) {
			if env == EnvShared {
				if _, err := q.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
					return nil, fmt.Errorf("creando el schema %s: %w", schema, err)
				}
			}
			return set.Up(ctx, q, schema)
		})
		if err != nil {
			return err
		}
		for _, m := range applied {
			r.log.InfoContext(ctx, "Migracion aplicada", "schema", schema, "migracion", m.ID())
		}
		if len(applied) == 0 {
			r.log.InfoContext(ctx, "Sin migraciones pendientes", "schema", schema)
		}
	}
	return nil
}

// Down revierte la ultima migracion aplicada de cada schema seleccionado.
func (r *Runner) Down(ctx context.Context, env Env, tenant string) error {
	set, schemas, err := r.target(ctx, env, tenant)
	if err != nil {
		return err
	}
	for _, schema := range schemas {
		reverted, err := database.InTxValue(ctx, r.db, func(q database.Querier) (*Migration, error) {
			return set.Down(ctx, q, schema)
		})
		if err != nil {
			return err
		}
		if reverted == nil {
			r.log.InfoContext(ctx, "Nada que revertir", "schema", schema)
			continue
		}
		r.log.InfoContext(ctx, "Migracion revertida", "schema", schema, "migracion", reverted.ID())
	}
	return nil
}

// Status devuelve el estado de cada schema seleccionado.
func (r *Runner) Status(ctx context.Context, env Env, tenant string) ([]SchemaStatus, error) {
	set, schemas, err := r.target(ctx, env, tenant)
	if err != nil {
		return nil, err
	}
	result := make([]SchemaStatus, 0, len(schemas))
	for _, schema := range schemas {
		states, err := database.InTxValue(ctx, r.db, func(q database.Querier) ([]State, error) {
			return set.Status(ctx, q, schema)
		})
		if err != nil {
			return nil, err
		}
		result = append(result, SchemaStatus{Schema: schema, States: states})
	}
	return result, nil
}

func (r *Runner) target(ctx context.Context, env Env, tenant string) (Set, []string, error) {
	set, ok := r.sets[env]
	if !ok {
		return Set{}, nil, fmt.Errorf("conjunto de migraciones desconocido: %q", env)
	}
	if env == EnvShared {
		return set, []string{database.SharedSchema}, nil
	}
	schemas, err := r.tenantSchemas(ctx, tenant)
	return set, schemas, err
}

func (r *Runner) tenantSchemas(ctx context.Context, slug string) ([]string, error) {
	if slug == "" {
		return nil, errors.New("indique el tenant (slug) o " + AllTenants)
	}
	return database.InTxValue(ctx, r.db, func(q database.Querier) ([]string, error) {
		var rows pgx.Rows
		var err error
		if slug == AllTenants {
			rows, err = q.Query(ctx, "SELECT schema_name FROM shared.tenants ORDER BY created_at")
		} else {
			rows, err = q.Query(ctx, "SELECT schema_name FROM shared.tenants WHERE slug = $1", slug)
		}
		if err != nil {
			return nil, fmt.Errorf("listando tenants: %w", err)
		}
		schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, fmt.Errorf("listando tenants: %w", err)
		}
		if slug != AllTenants && len(schemas) == 0 {
			return nil, fmt.Errorf("no existe el tenant %q", slug)
		}
		return schemas, nil
	})
}
