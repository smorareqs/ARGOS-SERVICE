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

const tenantColumns = `id, slug, name, schema_name, is_active, timezone, currency, logo_key, features, settings, created_at, updated_at`

const selectTenant = `SELECT ` + tenantColumns + ` FROM shared.tenants`

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
		INSERT INTO shared.tenants (slug, name, schema_name, is_active, features, timezone)
		VALUES ($1, $2, $3, $4, $5, COALESCE(NULLIF($6, ''), 'America/Guatemala'))
		RETURNING `+tenantColumns,
		t.Slug, t.Name, t.SchemaName, t.IsActive, t.Features, t.Timezone,
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

// ListWithStats devuelve una pagina de tenants con sus numeros, por nombre.
func (r *Repository) ListWithStats(ctx context.Context, limit, offset int) ([]TenantStats, error) {
	rows, err := r.q.Query(ctx, `
		SELECT t.id, t.slug, t.name, t.schema_name, t.is_active, t.timezone, t.currency, t.logo_key, t.features,
		       t.settings, t.created_at, t.updated_at,
		       (SELECT count(*) FROM shared.memberships m WHERE m.tenant_id = t.id)::int AS members,
		       (SELECT count(*) FROM shared.memberships m WHERE m.tenant_id = t.id AND m.status = 'active')::int AS active_members,
		       (SELECT max(rt.last_used_at) FROM shared.refresh_tokens rt WHERE rt.tenant_id = t.id) AS last_activity
		FROM shared.tenants t ORDER BY t.name LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[TenantStats])
}

// SetFeatures reemplaza las banderas de funcionalidad del tenant.
func (r *Repository) SetFeatures(ctx context.Context, id uuid.UUID, features map[string]bool) error {
	_, err := r.q.Exec(ctx,
		"UPDATE shared.tenants SET features = $2, updated_at = now() WHERE id = $1", id, features)
	return err
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

// Update guarda nombre, estado y banderas.
func (r *Repository) Update(ctx context.Context, t Tenant) error {
	_, err := r.q.Exec(ctx, `
		UPDATE shared.tenants SET name = $2, is_active = $3, features = $4, updated_at = now() WHERE id = $1`,
		t.ID, t.Name, t.IsActive, t.Features)
	return err
}

// SaveSettings guarda los datos que edita la empresa.
func (r *Repository) SaveSettings(ctx context.Context, t Tenant) error {
	_, err := r.q.Exec(ctx, `
		UPDATE shared.tenants SET name = $2, timezone = $3, currency = $4, logo_key = $5, settings = $6, updated_at = now()
		WHERE id = $1`, t.ID, t.Name, t.Timezone, t.Currency, t.LogoKey, t.Settings)
	return err
}

// GetForUpdate devuelve el tenant bloqueado para la transaccion o nil.
func (r *Repository) GetForUpdate(ctx context.Context, id uuid.UUID) (*Tenant, error) {
	return r.one(ctx, selectTenant+" WHERE id = $1 FOR UPDATE", id)
}

// RevokeSessions revoca todas las sesiones abiertas en el tenant.
func (r *Repository) RevokeSessions(ctx context.Context, id uuid.UUID) error {
	_, err := r.q.Exec(ctx, `
		UPDATE shared.refresh_tokens SET revoked_at = now(), updated_at = now()
		WHERE tenant_id = $1 AND revoked_at IS NULL`, id)
	return err
}

// LogoFile es un archivo subido para el logo (tabla files del tenant).
type LogoFile struct {
	ID     uuid.UUID `db:"id"`
	S3Key  string    `db:"s3_key"`
	Module string    `db:"module"`
	Kind   string    `db:"kind"`
	Status string    `db:"status"`
}

// FileByID devuelve un archivo del schema del tenant activo o nil.
func (r *Repository) FileByID(ctx context.Context, id uuid.UUID) (*LogoFile, error) {
	rows, err := r.q.Query(ctx, `SELECT id, s3_key, module, kind, status FROM files WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return nil, err
	}
	f, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[LogoFile])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// DiscardFileByKey marca como borrado el archivo de esa clave (el job
// storage_cleanup borra el objeto).
func (r *Repository) DiscardFileByKey(ctx context.Context, key string) error {
	_, err := r.q.Exec(ctx, `
		UPDATE files SET status = 'deleted', deleted_at = now(), updated_at = now()
		WHERE s3_key = $1 AND deleted_at IS NULL`, key)
	return err
}

// AttachFile asocia el archivo a su entidad.
func (r *Repository) AttachFile(ctx context.Context, id uuid.UUID, entityType string, entityID uuid.UUID) error {
	_, err := r.q.Exec(ctx, `
		UPDATE files SET entity_type = $2, entity_id = $3, updated_at = now() WHERE id = $1`, id, entityType, entityID)
	return err
}
