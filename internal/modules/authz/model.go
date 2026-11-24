package authz

import (
	"time"

	"github.com/google/uuid"
)

// Role es un rol de un tenant (tabla shared.roles). PermissionsVersion sube con
// cada cambio de permisos y viaja en el JWT (`pv`): un token con una version
// anterior deja de ser valido.
type Role struct {
	ID                 uuid.UUID `db:"id"`
	TenantID           uuid.UUID `db:"tenant_id"`
	Key                RoleKey   `db:"key"`
	Name               string    `db:"name"`
	Description        *string   `db:"description"`
	IsSystem           bool      `db:"is_system"`
	Scoped             bool      `db:"scoped"`
	PermissionsVersion int       `db:"permissions_version"`
	CreatedAt          time.Time `db:"created_at"`
	UpdatedAt          time.Time `db:"updated_at"`
}

// MembershipStatus es el estado de una membresia.
type MembershipStatus string

// Estados de una membresia.
const (
	MembershipInvited   MembershipStatus = "invited"
	MembershipActive    MembershipStatus = "active"
	MembershipSuspended MembershipStatus = "suspended"
)

// Membership une un usuario con un tenant y su rol (tabla shared.memberships).
type Membership struct {
	ID        uuid.UUID        `db:"id"`
	UserID    uuid.UUID        `db:"user_id"`
	TenantID  uuid.UUID        `db:"tenant_id"`
	RoleID    uuid.UUID        `db:"role_id"`
	Status    MembershipStatus `db:"status"`
	InvitedBy *uuid.UUID       `db:"invited_by"`
	JoinedAt  *time.Time       `db:"joined_at"`
	CreatedAt time.Time        `db:"created_at"`
	UpdatedAt time.Time        `db:"updated_at"`
}

// MembershipView es una membresia con los datos de su tenant y su rol, lo que
// necesitan el login y la resolucion del usuario autenticado.
type MembershipView struct {
	MembershipID   uuid.UUID        `db:"membership_id"`
	Status         MembershipStatus `db:"status"`
	TenantID       uuid.UUID        `db:"tenant_id"`
	TenantName     string           `db:"tenant_name"`
	TenantLogoKey  *string          `db:"tenant_logo_key"`
	TenantSchema   string           `db:"tenant_schema"`
	TenantActive   bool             `db:"tenant_active"`
	TenantTimezone string           `db:"tenant_timezone"`
	TenantCurrency string           `db:"tenant_currency"`
	TenantFeatures map[string]bool  `db:"tenant_features"`
	// TenantBillingStatus es el estado de la suscripcion (fase 6).
	TenantBillingStatus string    `db:"tenant_billing_status"`
	RoleID              uuid.UUID `db:"role_id"`
	RoleKey             RoleKey   `db:"role_key"`
	RoleName            string    `db:"role_name"`
	RoleScoped          bool      `db:"role_scoped"`
	PermissionsVersion  int       `db:"permissions_version"`
}

// ReadOnly indica si la empresa solo puede leer: suscripcion suspendida o
// cancelada (fase06.md §3.2). Toda escritura responde 402 salvo facturacion.
func (m MembershipView) ReadOnly() bool {
	return m.TenantBillingStatus == "suspended" || m.TenantBillingStatus == "cancelled"
}

// Usable indica si la membresia permite entrar: activa y con el tenant activo.
func (m MembershipView) Usable() bool {
	return m.Status == MembershipActive && m.TenantActive
}
