// Package auth gestiona usuarios, roles, login y emision/validacion de JWT.
package auth

import (
	"time"

	"github.com/google/uuid"
)

// User es un usuario del sistema, asociado a un tenant y a un rol
// (tabla shared.users).
//
// El usuario vive en el schema compartido y referencia su tenant: el catalogo
// de usuarios es global para permitir el login antes de resolver el schema.
// IsOwner modela la condicion de propietario, que el PRD define como un
// atributo y no como un rol.
type User struct {
	ID             uuid.UUID  `db:"id"`
	Email          string     `db:"email"`
	HashedPassword string     `db:"hashed_password"`
	FullName       string     `db:"full_name"`
	Role           Role       `db:"role"`
	IsActive       bool       `db:"is_active"`
	IsOwner        bool       `db:"is_owner"`
	TenantID       *uuid.UUID `db:"tenant_id"` // nil para platform_admin
	CreatedAt      time.Time  `db:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at"`
}
