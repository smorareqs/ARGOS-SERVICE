package auth

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
)

const selectUser = `SELECT id, email, hashed_password, full_name, phone, is_active, is_platform_admin,
       totp_enabled, password_changed_at, last_login_at, created_at, updated_at
FROM shared.users`

const refreshColumns = `id, user_id, tenant_id, token_hash, family_id, device_id, device_name, platform,
       expires_at, revoked_at, last_used_at, created_at, updated_at`

// Repository persiste usuarios y sesiones. Es pasivo: no contiene reglas de
// negocio.
type Repository struct {
	q database.Querier
}

// NewRepository crea un repositorio sobre q (normalmente una transaccion).
func NewRepository(q database.Querier) *Repository {
	return &Repository{q: q}
}

// GetByID devuelve el usuario o nil si no existe.
func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*User, error) {
	return oneOrNil[User](ctx, r.q, selectUser+" WHERE id = $1", id)
}

// GetByEmail devuelve el usuario o nil si no existe. No distingue mayusculas.
func (r *Repository) GetByEmail(ctx context.Context, email string) (*User, error) {
	return oneOrNil[User](ctx, r.q, selectUser+" WHERE lower(email) = lower($1)", email)
}

// Add inserta el usuario y devuelve la fila resultante.
func (r *Repository) Add(ctx context.Context, u User) (User, error) {
	rows, err := r.q.Query(ctx, `
		INSERT INTO shared.users (email, hashed_password, full_name, phone, is_active, is_platform_admin,
		                          password_changed_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		RETURNING id, email, hashed_password, full_name, phone, is_active, is_platform_admin,
		          totp_enabled, password_changed_at, last_login_at, created_at, updated_at`,
		u.Email, u.HashedPassword, u.FullName, u.Phone, u.IsActive, u.IsPlatformAdmin,
	)
	if err != nil {
		return User{}, err
	}
	user, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[User])
	if database.IsUniqueViolation(err) {
		return User{}, ErrEmailAlreadyExists
	}
	return user, err
}

// SetPassword reemplaza el hash de la contrasena.
func (r *Repository) SetPassword(ctx context.Context, id uuid.UUID, hashed string) error {
	_, err := r.q.Exec(ctx, `
		UPDATE shared.users SET hashed_password = $2, password_changed_at = now(), updated_at = now()
		WHERE id = $1`, id, hashed)
	return err
}

// TouchLogin registra la hora del ultimo login.
func (r *Repository) TouchLogin(ctx context.Context, id uuid.UUID) error {
	_, err := r.q.Exec(ctx, "UPDATE shared.users SET last_login_at = now() WHERE id = $1", id)
	return err
}

// AddRefreshToken guarda una sesion. ip puede ser nil.
func (r *Repository) AddRefreshToken(ctx context.Context, t RefreshToken, ip *string) error {
	_, err := r.q.Exec(ctx, `
		INSERT INTO shared.refresh_tokens (user_id, tenant_id, token_hash, family_id, device_id, device_name,
		                                   platform, expires_at, ip, last_used_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::inet, now())`,
		t.UserID, t.TenantID, t.TokenHash, t.FamilyID, t.DeviceID, t.DeviceName, t.Platform, t.ExpiresAt, ip,
	)
	return err
}

// RefreshTokenByHash devuelve la sesion con ese hash (bloqueada para esta
// transaccion, asi dos rotaciones simultaneas no emiten dos tokens) o nil.
func (r *Repository) RefreshTokenByHash(ctx context.Context, hash string) (*RefreshToken, error) {
	return oneOrNil[RefreshToken](ctx, r.q,
		"SELECT "+refreshColumns+" FROM shared.refresh_tokens WHERE token_hash = $1 FOR UPDATE", hash)
}

// RevokeRefreshToken marca la sesion como revocada (si no lo estaba).
func (r *Repository) RevokeRefreshToken(ctx context.Context, id uuid.UUID) error {
	_, err := r.q.Exec(ctx, `
		UPDATE shared.refresh_tokens SET revoked_at = now(), updated_at = now()
		WHERE id = $1 AND revoked_at IS NULL`, id)
	return err
}

// RevokeFamily revoca todos los tokens vigentes de una sesion.
func (r *Repository) RevokeFamily(ctx context.Context, familyID uuid.UUID) error {
	_, err := r.q.Exec(ctx, `
		UPDATE shared.refresh_tokens SET revoked_at = now(), updated_at = now()
		WHERE family_id = $1 AND revoked_at IS NULL`, familyID)
	return err
}

// FamilyActive indica si la sesion (familia) tiene algun token vigente.
func (r *Repository) FamilyActive(ctx context.Context, familyID uuid.UUID) (bool, error) {
	var active bool
	err := r.q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM shared.refresh_tokens
		               WHERE family_id = $1 AND revoked_at IS NULL AND expires_at > now())`,
		familyID).Scan(&active)
	return active, err
}

func oneOrNil[T any](ctx context.Context, q database.Querier, sql string, args ...any) (*T, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	row, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[T])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}
