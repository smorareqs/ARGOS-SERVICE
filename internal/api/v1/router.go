// Package v1 contiene los endpoints de la API v1: reciben la peticion HTTP ya
// validada, llaman al servicio y devuelven la respuesta con su codigo HTTP.
package v1

import (
	"github.com/danielgtaylor/huma/v2"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/app"
)

// Prefix es el prefijo de todas las rutas de la API v1.
const Prefix = "/api/v1"

// Register centraliza las rutas de la API v1.
//
// Archivo compartido: cada modulo nuevo agrega una sola linea register<Modulo>;
// nunca se reordena.
func Register(humaAPI huma.API, svc app.Services) {
	guard := api.NewGuard(humaAPI, svc.Auth)
	v1 := huma.NewGroup(humaAPI, Prefix)

	registerHealth(v1, svc.Health)
	registerAuth(v1, svc.Auth, guard)
	registerTenants(v1, svc.Tenants, guard)
}
