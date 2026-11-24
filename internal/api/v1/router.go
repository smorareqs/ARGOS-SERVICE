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
	registerAccount(v1, svc.Auth, guard)
	registerTeam(v1, svc.Auth, svc.Authz, svc.Tenants, guard)
	registerFiles(v1, svc.Files, guard)
	registerProperties(v1, svc.Properties, guard)
	registerChecklists(v1, svc.Checklists, guard)
	registerEmployees(v1, svc.Employees, guard)
	registerAudit(v1, svc.Audit, guard)
	registerSync(v1, svc.Sync, guard)
	registerMaintenance(v1, svc.Maintenance, guard)
	registerAlerts(v1, svc.Alerts, guard)
	registerDashboard(v1, svc.Dashboard, guard)
	registerReservations(v1, svc.Reservations, guard)
	registerPayroll(v1, svc.Payroll, guard)
	registerDisciplinary(v1, svc.Disciplinary, guard)
	registerBilling(v1, svc.Billing, guard)
	registerAPIKeys(v1, svc.APIKeys, guard)
	registerPublic(v1, svc.APIKeys, svc.Reservations, svc.Billing, svc.APIKeyLimiter)
}
