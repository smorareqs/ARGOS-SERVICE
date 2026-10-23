package tenants

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
)

// SchemaPrefix se antepone al slug para nombrar el schema del tenant.
const SchemaPrefix = "tenant_"

const selectTenant = `SELECT id, slug, name, schema_name, is_active, created_at, updated_at
FROM shared.tenants`

// Repository persiste y recupera tenants. Incluye la creacion fisica del
// schema dedicado (DDL), que es una operacion de infraestructura de datos.
type Repository struct {
	q database.Querier
}

// NewRepository crea un repositorio sobre q (normalmente una transaccion).
func NewRepository(q database.Querier) *Repository {
	return &Repository{q: q}
}

// BuildSchemaName construye el nombre de schema a partir del slug.
func BuildSchemaName(slug string) string {
	return SchemaPrefix + slug
}

// GetByID devuelve el tenant o nil si no existe.
func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*Tenant, error) {
	return r.one(ctx, selectTenant+" WHERE id = $1", id)
}

// GetBySlug devuelve el tenant o nil si no existe.
func (r *Repository) GetBySlug(ctx context.Context, slug string) (*Tenant, error) {
	return r.one(ctx, selectTenant+" WHERE slug = $1", slug)
}

// ListAll devuelve una pagina de tenants ordenada por fecha de alta.
func (r *Repository) ListAll(ctx context.Context, limit, offset int) ([]Tenant, error) {
	rows, err := r.q.Query(ctx, selectTenant+" ORDER BY created_at LIMIT $1 OFFSET $2", limit, offset)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Tenant])
}

// Add inserta el tenant y devuelve la fila resultante.
func (r *Repository) Add(ctx context.Context, t Tenant) (Tenant, error) {
	rows, err := r.q.Query(ctx, `
		INSERT INTO shared.tenants (slug, name, schema_name, is_active)
		VALUES ($1, $2, $3, $4)
		RETURNING id, slug, name, schema_name, is_active, created_at, updated_at`,
		t.Slug, t.Name, t.SchemaName, t.IsActive,
	)
	if err != nil {
		return Tenant{}, err
	}
	tenant, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[Tenant])
	if database.IsUniqueViolation(err) {
		return Tenant{}, ErrTenantSlugAlreadyExists
	}
	return tenant, err
}

// CreateSchema crea el schema fisico del tenant. Falla si ya existe para no
// adoptar nunca datos de un schema previo.
func (r *Repository) CreateSchema(ctx context.Context, schemaName string) error {
	_, err := r.q.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schemaName}.Sanitize())
	if database.IsDuplicateSchema(err) {
		return ErrTenantSlugAlreadyExists
	}
	return err
}

// SchemaExists indica si el schema existe en la base de datos.
func (r *Repository) SchemaExists(ctx context.Context, schemaName string) (bool, error) {
	var exists bool
	err := r.q.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)",
		schemaName,
	).Scan(&exists)
	return exists, err
}

func (r *Repository) one(ctx context.Context, sql string, args ...any) (*Tenant, error) {
	rows, err := r.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	tenant, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[Tenant])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &tenant, nil
}
