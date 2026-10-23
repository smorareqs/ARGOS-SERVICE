package auth

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
)

const selectUser = `SELECT id, email, hashed_password, full_name, role, is_active, is_owner,
       tenant_id, created_at, updated_at
FROM shared.users`

// Repository persiste y recupera usuarios. Es pasivo: no contiene reglas de
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
	return r.one(ctx, selectUser+" WHERE id = $1", id)
}

// GetByEmail devuelve el usuario o nil si no existe.
func (r *Repository) GetByEmail(ctx context.Context, email string) (*User, error) {
	return r.one(ctx, selectUser+" WHERE email = $1", email)
}

// Add inserta el usuario y devuelve la fila resultante.
func (r *Repository) Add(ctx context.Context, u User) (User, error) {
	rows, err := r.q.Query(ctx, `
		INSERT INTO shared.users (email, hashed_password, full_name, role, is_active, is_owner, tenant_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, email, hashed_password, full_name, role, is_active, is_owner,
		          tenant_id, created_at, updated_at`,
		u.Email, u.HashedPassword, u.FullName, u.Role, u.IsActive, u.IsOwner, u.TenantID,
	)
	if err != nil {
		return User{}, err
	}
	user, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[User])
	switch {
	case database.IsUniqueViolation(err):
		return User{}, ErrEmailAlreadyExists
	case database.IsForeignKeyViolation(err):
		return User{}, ErrUnknownTenant
	case err != nil:
		return User{}, err
	}
	return user, nil
}

func (r *Repository) one(ctx context.Context, sql string, args ...any) (*User, error) {
	rows, err := r.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	user, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[User])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &user, nil
}
