package authz

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
)

const selectMembershipView = `SELECT m.id AS membership_id, m.status,
       t.id AS tenant_id, t.name AS tenant_name, t.logo_key AS tenant_logo_key,
       t.schema_name AS tenant_schema, t.is_active AS tenant_active,
       t.timezone AS tenant_timezone, t.currency AS tenant_currency, t.features AS tenant_features,
       r.id AS role_id, r.key AS role_key, r.name AS role_name, r.permissions_version
FROM shared.memberships m
JOIN shared.tenants t ON t.id = m.tenant_id
JOIN shared.roles r ON r.id = m.role_id`

// Repository persiste roles, permisos y membresias. Es pasivo: no contiene
// reglas de negocio.
type Repository struct {
	q database.Querier
}

// NewRepository crea un repositorio sobre q (normalmente una transaccion).
func NewRepository(q database.Querier) *Repository {
	return &Repository{q: q}
}

// UpsertSystemRole crea el rol de sistema o actualiza su nombre, y devuelve la
// fila resultante.
func (r *Repository) UpsertSystemRole(ctx context.Context, tenantID uuid.UUID, key RoleKey, name string) (Role, error) {
	rows, err := r.q.Query(ctx, `
		INSERT INTO shared.roles (tenant_id, key, name, is_system)
		VALUES ($1, $2, $3, true)
		ON CONFLICT (tenant_id, key) DO UPDATE SET name = EXCLUDED.name, is_system = true
		RETURNING id, tenant_id, key, name, description, is_system, permissions_version,
		          created_at, updated_at`,
		tenantID, key, name,
	)
	if err != nil {
		return Role{}, err
	}
	return pgx.CollectOneRow(rows, pgx.RowToStructByName[Role])
}

// RoleByKey devuelve el rol del tenant con esa clave o nil.
func (r *Repository) RoleByKey(ctx context.Context, tenantID uuid.UUID, key RoleKey) (*Role, error) {
	rows, err := r.q.Query(ctx, `
		SELECT id, tenant_id, key, name, description, is_system, permissions_version, created_at, updated_at
		FROM shared.roles WHERE tenant_id = $1 AND key = $2`, tenantID, key)
	if err != nil {
		return nil, err
	}
	role, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[Role])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &role, nil
}

// Permissions devuelve los permisos del rol.
func (r *Repository) Permissions(ctx context.Context, roleID uuid.UUID) ([]Permission, error) {
	rows, err := r.q.Query(ctx,
		"SELECT permission FROM shared.role_permissions WHERE role_id = $1 ORDER BY permission", roleID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[Permission])
}

// ReplacePermissions reemplaza los permisos del rol y sube su version.
func (r *Repository) ReplacePermissions(ctx context.Context, roleID uuid.UUID, perms []Permission) error {
	if _, err := r.q.Exec(ctx, "DELETE FROM shared.role_permissions WHERE role_id = $1", roleID); err != nil {
		return err
	}
	values := make([]string, len(perms))
	for i, p := range perms {
		values[i] = string(p)
	}
	if _, err := r.q.Exec(ctx, `
		INSERT INTO shared.role_permissions (role_id, permission)
		SELECT $1, unnest($2::text[])`, roleID, values); err != nil {
		return err
	}
	_, err := r.q.Exec(ctx, `
		UPDATE shared.roles SET permissions_version = permissions_version + 1, updated_at = now()
		WHERE id = $1`, roleID)
	return err
}

// TenantIDs devuelve los ids de todos los tenants.
func (r *Repository) TenantIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := r.q.Query(ctx, "SELECT id FROM shared.tenants ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// UpsertMembership crea la membresia o actualiza su rol y estado.
func (r *Repository) UpsertMembership(ctx context.Context, m Membership) (Membership, error) {
	rows, err := r.q.Query(ctx, `
		INSERT INTO shared.memberships (user_id, tenant_id, role_id, status, invited_by, joined_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (user_id, tenant_id) DO UPDATE
		SET role_id = EXCLUDED.role_id, status = EXCLUDED.status, updated_at = now()
		RETURNING id, user_id, tenant_id, role_id, status, invited_by, joined_at, created_at, updated_at`,
		m.UserID, m.TenantID, m.RoleID, m.Status, m.InvitedBy, m.JoinedAt,
	)
	if err != nil {
		return Membership{}, err
	}
	return pgx.CollectOneRow(rows, pgx.RowToStructByName[Membership])
}

// MembershipsOfUser devuelve todas las membresias del usuario, por nombre de
// tenant.
func (r *Repository) MembershipsOfUser(ctx context.Context, userID uuid.UUID) ([]MembershipView, error) {
	rows, err := r.q.Query(ctx, selectMembershipView+" WHERE m.user_id = $1 ORDER BY t.name", userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[MembershipView])
}

// MembershipOfUser devuelve la membresia del usuario en el tenant o nil.
func (r *Repository) MembershipOfUser(ctx context.Context, userID, tenantID uuid.UUID) (*MembershipView, error) {
	rows, err := r.q.Query(ctx, selectMembershipView+" WHERE m.user_id = $1 AND m.tenant_id = $2", userID, tenantID)
	if err != nil {
		return nil, err
	}
	view, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[MembershipView])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &view, nil
}
