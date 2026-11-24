package authz

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RoleSummary es un rol con sus permisos y cuantas personas lo tienen.
type RoleSummary struct {
	Role
	Members     int          `db:"members"`
	Permissions []Permission `db:"permissions"`
}

// MemberRow es un miembro de la empresa con los datos de su cuenta y su rol.
type MemberRow struct {
	ID          uuid.UUID        `db:"id"`
	UserID      uuid.UUID        `db:"user_id"`
	FullName    string           `db:"full_name"`
	Email       string           `db:"email"`
	Phone       *string          `db:"phone"`
	TOTPEnabled bool             `db:"totp_enabled"`
	UserActive  bool             `db:"user_active"`
	RoleID      uuid.UUID        `db:"role_id"`
	RoleKey     RoleKey          `db:"role_key"`
	RoleName    string           `db:"role_name"`
	Status      MembershipStatus `db:"status"`
	LastSeenAt  *time.Time       `db:"last_seen_at"`
	JoinedAt    *time.Time       `db:"joined_at"`
	CreatedAt   time.Time        `db:"created_at"`
	// PropertiesCount: asignadas o propias (tablas del schema de la empresa).
	PropertiesCount int `db:"properties_count"`
}

const roleColumns = `r.id, r.tenant_id, r.key, r.name, r.description, r.is_system, r.scoped, r.permissions_version,
       r.created_at, r.updated_at`

const selectRoleSummary = `SELECT ` + roleColumns + `,
       (SELECT count(*) FROM shared.memberships m WHERE m.role_id = r.id)::int AS members,
       COALESCE((SELECT array_agg(rp.permission ORDER BY rp.permission)
                 FROM shared.role_permissions rp WHERE rp.role_id = r.id), '{}') AS permissions
FROM shared.roles r`

const selectMember = `SELECT m.id, m.user_id, u.full_name, u.email, u.phone, u.totp_enabled, u.is_active AS user_active,
       m.role_id, r.key AS role_key, r.name AS role_name, m.status,
       (SELECT max(rt.last_used_at) FROM shared.refresh_tokens rt
        WHERE rt.user_id = m.user_id AND rt.tenant_id = m.tenant_id) AS last_seen_at,
       m.joined_at, m.created_at,
       (SELECT count(*) FROM (SELECT property_id FROM property_assignments WHERE user_id = m.user_id
                              UNION SELECT property_id FROM property_owners WHERE user_id = m.user_id) x)::int
         AS properties_count
FROM shared.memberships m
JOIN shared.users u ON u.id = m.user_id
JOIN shared.roles r ON r.id = m.role_id`

// Roles devuelve los roles del tenant (sin el de huesped), sistema primero.
func (r *Repository) Roles(ctx context.Context, tenantID uuid.UUID) ([]RoleSummary, error) {
	rows, err := r.q.Query(ctx, selectRoleSummary+`
		WHERE r.tenant_id = $1 AND r.key <> 'guest'
		ORDER BY r.is_system DESC, r.created_at, r.name`, tenantID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[RoleSummary])
}

// RoleSummaryByID devuelve el rol del tenant o nil.
func (r *Repository) RoleSummaryByID(ctx context.Context, tenantID, id uuid.UUID) (*RoleSummary, error) {
	rows, err := r.q.Query(ctx, selectRoleSummary+` WHERE r.tenant_id = $1 AND r.id = $2`, tenantID, id)
	if err != nil {
		return nil, err
	}
	return collectOne[RoleSummary](rows)
}

// RoleKeyExists indica si la clave ya existe en el tenant.
func (r *Repository) RoleKeyExists(ctx context.Context, tenantID uuid.UUID, key RoleKey) (bool, error) {
	var exists bool
	err := r.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM shared.roles WHERE tenant_id = $1 AND key = $2)`,
		tenantID, key).Scan(&exists)
	return exists, err
}

// RoleNameExists indica si otro rol del tenant ya usa ese nombre.
func (r *Repository) RoleNameExists(ctx context.Context, tenantID uuid.UUID, name string, except *uuid.UUID) (bool, error) {
	var exists bool
	err := r.q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM shared.roles
		               WHERE tenant_id = $1 AND lower(name) = lower($2) AND ($3::uuid IS NULL OR id <> $3))`,
		tenantID, name, except).Scan(&exists)
	return exists, err
}

// AddRole inserta un rol personalizado y devuelve su id.
func (r *Repository) AddRole(ctx context.Context, tenantID uuid.UUID, key RoleKey, name string, description *string, scoped bool) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.q.QueryRow(ctx, `
		INSERT INTO shared.roles (tenant_id, key, name, description, is_system, scoped)
		VALUES ($1, $2, $3, $4, false, $5) RETURNING id`, tenantID, key, name, description, scoped).Scan(&id)
	return id, err
}

// UpdateRole cambia nombre, descripcion y alcance.
func (r *Repository) UpdateRole(ctx context.Context, id uuid.UUID, name string, description *string, scoped bool) error {
	_, err := r.q.Exec(ctx, `
		UPDATE shared.roles SET name = $2, description = $3, scoped = $4, updated_at = now() WHERE id = $1`,
		id, name, description, scoped)
	return err
}

// DeleteRole borra el rol.
func (r *Repository) DeleteRole(ctx context.Context, id uuid.UUID) error {
	_, err := r.q.Exec(ctx, `DELETE FROM shared.roles WHERE id = $1`, id)
	return err
}

// PendingInvitationsWithRole cuenta las invitaciones pendientes con el rol.
func (r *Repository) PendingInvitationsWithRole(ctx context.Context, roleID uuid.UUID) (int, error) {
	var n int
	err := r.q.QueryRow(ctx, `SELECT count(*) FROM shared.invitations WHERE role_id = $1 AND status = 'pending'`,
		roleID).Scan(&n)
	return n, err
}

// Members devuelve los miembros del tenant por nombre.
func (r *Repository) Members(ctx context.Context, tenantID uuid.UUID) ([]MemberRow, error) {
	rows, err := r.q.Query(ctx, selectMember+` WHERE m.tenant_id = $1 AND r.key <> 'guest'
		ORDER BY m.status, u.full_name`, tenantID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[MemberRow])
}

// MemberByID devuelve el miembro del tenant (bloqueado) o nil.
func (r *Repository) MemberByID(ctx context.Context, tenantID, id uuid.UUID) (*MemberRow, error) {
	rows, err := r.q.Query(ctx, selectMember+` WHERE m.tenant_id = $1 AND m.id = $2 FOR UPDATE OF m`, tenantID, id)
	if err != nil {
		return nil, err
	}
	return collectOne[MemberRow](rows)
}

// SetMemberRole cambia el rol de la membresia.
func (r *Repository) SetMemberRole(ctx context.Context, id, roleID uuid.UUID) error {
	_, err := r.q.Exec(ctx, `UPDATE shared.memberships SET role_id = $2, updated_at = now() WHERE id = $1`, id, roleID)
	return err
}

// SetMemberStatus cambia el estado de la membresia.
func (r *Repository) SetMemberStatus(ctx context.Context, id uuid.UUID, status MembershipStatus) error {
	_, err := r.q.Exec(ctx, `UPDATE shared.memberships SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	return err
}

// ActiveAdmins cuenta los administradores activos del tenant (bloquea sus
// membresias para que dos cambios simultaneos no dejen la empresa sin
// administrador).
func (r *Repository) ActiveAdmins(ctx context.Context, tenantID uuid.UUID) (int, error) {
	rows, err := r.q.Query(ctx, `
		SELECT m.id FROM shared.memberships m
		JOIN shared.roles r ON r.id = m.role_id
		JOIN shared.users u ON u.id = m.user_id
		WHERE m.tenant_id = $1 AND r.key = 'admin' AND m.status = 'active' AND u.is_active
		FOR UPDATE OF m`, tenantID)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	return len(ids), err
}

// RevokeTenantSessions revoca las sesiones del usuario abiertas en el tenant.
func (r *Repository) RevokeTenantSessions(ctx context.Context, userID, tenantID uuid.UUID) error {
	_, err := r.q.Exec(ctx, `
		UPDATE shared.refresh_tokens SET revoked_at = now(), updated_at = now()
		WHERE user_id = $1 AND tenant_id = $2 AND revoked_at IS NULL`, userID, tenantID)
	return err
}

func collectOne[T any](rows pgx.Rows) (*T, error) {
	row, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[T])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}
