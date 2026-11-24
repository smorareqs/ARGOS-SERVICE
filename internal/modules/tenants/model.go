// Package tenants gestiona los clientes (tenants) y el aprovisionamiento de
// su schema dedicado (schema-per-tenant), su configuracion (W-O1) y la
// administracion de la plataforma (W-P1).
package tenants

import (
	"time"

	"github.com/google/uuid"
)

// Tenant es un cliente u organizacion (tabla shared.tenants). Cada tenant
// posee un schema dedicado donde residen sus datos.
//
// Vive siempre en el schema compartido: es el catalogo que permite resolver
// el schema de cada cliente.
type Tenant struct {
	ID         uuid.UUID `db:"id"`
	Slug       string    `db:"slug"` // unico; construye el nombre del schema
	Name       string    `db:"name"`
	SchemaName string    `db:"schema_name"` // schema real en PostgreSQL
	IsActive   bool      `db:"is_active"`
	Timezone   string    `db:"timezone"`
	Currency   string    `db:"currency"`
	LogoKey    *string   `db:"logo_key"` // clave S3 del logo
	// Features son las banderas de funcionalidad (authz.Features).
	Features map[string]bool `db:"features"`
	// Settings son el resto de ajustes (datos fiscales, horarios...).
	Settings  map[string]any `db:"settings"`
	CreatedAt time.Time      `db:"created_at"`
	UpdatedAt time.Time      `db:"updated_at"`
}

// TenantStats son los numeros de una empresa para la lista de plataforma.
type TenantStats struct {
	Tenant
	Members       int        `db:"members"`
	ActiveMembers int        `db:"active_members"`
	LastActivity  *time.Time `db:"last_activity"`
}
