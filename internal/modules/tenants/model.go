// Package tenants gestiona los clientes (tenants) y el aprovisionamiento de
// su schema dedicado (schema-per-tenant).
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
	CreatedAt  time.Time `db:"created_at"`
	UpdatedAt  time.Time `db:"updated_at"`
}
