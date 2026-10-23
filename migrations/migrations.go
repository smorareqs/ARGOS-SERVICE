// Package migrations embebe en el binario las migraciones SQL de ARGOS.
//
//   - shared/: tablas globales del schema compartido.
//   - tenant/: tablas de negocio; se aplican a cada schema tenant_<slug>.
//
// El formato y las reglas estan en README.md de esta carpeta.
package migrations

import "embed"

// FS contiene los directorios shared/ y tenant/.
//
//go:embed all:shared all:tenant
var FS embed.FS
