// Package auth gestiona la identidad: usuarios, login multiempresa, sesiones
// por dispositivo (refresh tokens rotativos) y emision/validacion de JWT
// (APP/docs/plans/fase01.md §3.7).
package auth

import (
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// User es una persona con acceso a ARGOS (tabla shared.users). El catalogo de
// usuarios es global: una persona puede pertenecer a varias empresas mediante
// membresias (authz.Membership).
type User struct {
	ID                uuid.UUID  `db:"id"`
	Email             string     `db:"email"`
	HashedPassword    *string    `db:"hashed_password"` // nil: invitado sin contrasena
	FullName          string     `db:"full_name"`
	Phone             *string    `db:"phone"`
	IsActive          bool       `db:"is_active"`
	IsPlatformAdmin   bool       `db:"is_platform_admin"`
	TOTPEnabled       bool       `db:"totp_enabled"`
	PasswordChangedAt *time.Time `db:"password_changed_at"`
	LastLoginAt       *time.Time `db:"last_login_at"`
	CreatedAt         time.Time  `db:"created_at"`
	UpdatedAt         time.Time  `db:"updated_at"`
}

// Platform es la plataforma de un dispositivo.
type Platform string

// Plataformas admitidas.
const (
	PlatformAndroid Platform = "android"
	PlatformIOS     Platform = "ios"
	PlatformWeb     Platform = "web"
)

// Valid indica si p es una plataforma conocida.
func (p Platform) Valid() bool {
	return p == PlatformAndroid || p == PlatformIOS || p == PlatformWeb
}

// RefreshToken es una sesion de un dispositivo (tabla shared.refresh_tokens).
// Solo se guarda el hash del token. Todos los tokens de una misma sesion
// comparten FamilyID: al rotar, el anterior queda revocado; si alguien
// presenta uno ya rotado, se revoca la familia entera.
type RefreshToken struct {
	ID         uuid.UUID  `db:"id"`
	UserID     uuid.UUID  `db:"user_id"`
	TenantID   *uuid.UUID `db:"tenant_id"` // nil: sesion de plataforma
	TokenHash  string     `db:"token_hash"`
	FamilyID   uuid.UUID  `db:"family_id"`
	DeviceID   string     `db:"device_id"`
	DeviceName *string    `db:"device_name"`
	Platform   Platform   `db:"platform"`
	ExpiresAt  time.Time  `db:"expires_at"`
	RevokedAt  *time.Time `db:"revoked_at"`
	LastUsedAt *time.Time `db:"last_used_at"`
	CreatedAt  time.Time  `db:"created_at"`
	UpdatedAt  time.Time  `db:"updated_at"`
}

// Principal es quien hace una peticion autenticada: el usuario, su empresa
// activa (si la hay) con su rol y sus permisos, y la sesion de origen.
type Principal struct {
	User        User
	Membership  *authz.MembershipView // nil en modo plataforma
	Permissions map[authz.Permission]bool
	SessionID   uuid.UUID
}

// Can indica si el principal tiene todos los permisos.
func (p Principal) Can(perms ...authz.Permission) bool {
	for _, perm := range perms {
		if !p.Permissions[perm] {
			return false
		}
	}
	return true
}

// Actor es el principal como autor de cambios en su empresa (authz).
func (p Principal) Actor() authz.Actor {
	a := authz.Actor{UserID: p.User.ID, Permissions: p.Permissions, Scoped: p.Scoped()}
	if p.Membership != nil {
		a.TenantID = p.Membership.TenantID
	}
	return a
}

// Scoped indica si el rol del principal solo ve sus propiedades asignadas o
// propias (fase02.md §3, authz.PropertyScope).
func (p Principal) Scoped() bool {
	return p.Membership != nil && p.Membership.RoleScoped
}

// Platform indica si la sesion opera en modo plataforma (sin empresa).
func (p Principal) Platform() bool {
	return p.Membership == nil
}

// Propositos de los tokens de un solo uso (shared.one_time_tokens).
const (
	PurposePasswordReset = "password_reset"
	PurposeInvitation    = "invitation"
)

// OneTimeToken es un enlace de un solo uso enviado por correo. Solo se guarda
// el hash del token.
type OneTimeToken struct {
	ID        uuid.UUID      `db:"id"`
	UserID    *uuid.UUID     `db:"user_id"`
	Purpose   string         `db:"purpose"`
	TokenHash string         `db:"token_hash"`
	Payload   map[string]any `db:"payload"`
	ExpiresAt time.Time      `db:"expires_at"`
	UsedAt    *time.Time     `db:"used_at"`
	CreatedAt time.Time      `db:"created_at"`
	UpdatedAt time.Time      `db:"updated_at"`
}

// Usable indica si el token sigue sirviendo en now.
func (t OneTimeToken) Usable(now time.Time) bool {
	return t.UsedAt == nil && t.ExpiresAt.After(now)
}

// Estados de una invitacion.
const (
	InvitationPending  = "pending"
	InvitationAccepted = "accepted"
	InvitationExpired  = "expired"
	InvitationRevoked  = "revoked"
)

// Invitation es una invitacion a una empresa con los datos de su rol, su
// empresa, quien invito y su token vigente.
type Invitation struct {
	ID             uuid.UUID      `db:"id"`
	TenantID       uuid.UUID      `db:"tenant_id"`
	Email          string         `db:"email"`
	RoleID         uuid.UUID      `db:"role_id"`
	RoleKey        authz.RoleKey  `db:"role_key"`
	RoleName       string         `db:"role_name"`
	InvitedBy      *uuid.UUID     `db:"invited_by"`
	InviterName    *string        `db:"inviter_name"`
	TokenID        *uuid.UUID     `db:"token_id"`
	Status         string         `db:"status"`
	AcceptedAt     *time.Time     `db:"accepted_at"`
	CreatedAt      time.Time      `db:"created_at"`
	UpdatedAt      time.Time      `db:"updated_at"`
	ExpiresAt      *time.Time     `db:"expires_at"`
	Payload        map[string]any `db:"payload"`
	TenantName     string         `db:"tenant_name"`
	TenantSchema   string         `db:"tenant_schema"`
	TenantActive   bool           `db:"tenant_active"`
	TenantLogoKey  *string        `db:"tenant_logo_key"`
	TenantTimezone string         `db:"tenant_timezone"`
}

// SessionRow es una sesion (familia de refresh tokens) vigente.
type SessionRow struct {
	ID         uuid.UUID  `db:"id"`
	DeviceName *string    `db:"device_name"`
	Platform   Platform   `db:"platform"`
	IP         *string    `db:"ip"`
	TenantName *string    `db:"tenant_name"`
	LastUsedAt *time.Time `db:"last_used_at"`
	CreatedAt  time.Time  `db:"created_at"`
}
