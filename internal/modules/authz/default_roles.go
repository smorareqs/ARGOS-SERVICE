package authz

import "slices"

// RoleKey identifica un rol dentro de un tenant. Los siete de sistema tienen
// clave fija; los personalizados usan la que defina el tenant.
type RoleKey string

// Roles de sistema (PRD seccion 3.2).
const (
	RoleAdmin       RoleKey = "admin"
	RoleSupervisor  RoleKey = "supervisor"
	RoleCleaner     RoleKey = "cleaner"
	RoleMaintenance RoleKey = "maintenance"
	RoleSecurity    RoleKey = "security"
	RoleOwner       RoleKey = "owner"
	RoleGuest       RoleKey = "guest"
)

// SystemRole es un rol sembrado en cada tenant.
type SystemRole struct {
	Key         RoleKey
	Name        string
	Permissions []Permission
	// Scoped: solo ve sus propiedades asignadas o propias (ScopeResolver).
	Scoped bool
}

// SystemRoles es la matriz de roles por defecto del PRD seccion 3.4, unica
// fuente de verdad: la migracion 0002 solo inserta clave y nombre, y
// SyncSystemRoles escribe estos permisos.
//
// Los alcances ("asignadas", "propias", "hoy"...) no son permisos distintos:
// los roles con Scoped solo ven sus propiedades (PropertyScope, scope.go).
var SystemRoles = []SystemRole{
	{Key: RoleAdmin, Name: "Administrador", Permissions: without(All, HRDisciplinaryOwn)},
	{Key: RoleSupervisor, Name: "Supervisor", Permissions: without(All,
		TenantSettings, TenantBilling,
		UsersManage, RolesManage,
		PropertiesManage,
		GuestsManage, GuestsDocuments,
		DepositsManage,
		SharedAreasManage,
		HRAttendanceManage, HRPayrollManage, HRPayrollExport, HRDisciplinaryManage,
		MetricsAll,
	)},
	{Key: RoleCleaner, Name: "Colaborador de limpieza", Scoped: true, Permissions: []Permission{
		PropertiesRead, PropertiesAccessInfo,
		TasksRead, TasksExecute,
		InventoryRead, InventoryCheckout,
		MaintenanceRead, MaintenanceReport,
		HRAttendanceOwn, HRDisciplinaryOwn,
		MetricsOwn,
		ChatInternal,
	}},
	{Key: RoleMaintenance, Name: "Mantenimiento", Scoped: true, Permissions: []Permission{
		PropertiesRead, PropertiesAccessInfo,
		TasksRead, TasksExecute,
		InventoryRead, InventoryCheckout,
		MaintenanceRead, MaintenanceReport, MaintenanceExecute,
		HRAttendanceOwn, HRDisciplinaryOwn,
		MetricsOwn,
		AlertsRead,
		ChatInternal,
	}},
	{Key: RoleSecurity, Name: "Seguridad", Permissions: []Permission{
		PropertiesRead, PropertiesAccessInfo,
		MaintenanceReport,
		ReservationsRead,
		GuestsRead,
		ParkingRead,
		SharedAreasRead,
		SecurityDashboard,
		HRAttendanceOwn, HRDisciplinaryOwn,
		MetricsOwn,
		AlertsRead,
		ChatInternal,
	}},
	{Key: RoleOwner, Name: "Propietario", Scoped: true, Permissions: []Permission{
		PropertiesRead,
		TasksRead,
		MaintenanceRead, MaintenanceReport,
		ReservationsRead,
		DepositsRead,
		MetricsAll,
		ReportsExport,
	}},
	{Key: RoleGuest, Name: "Huésped", Scoped: true, Permissions: []Permission{
		PropertiesRead,
		MaintenanceReport,
		ReservationsRead,
		ParkingRead,
		SharedAreasRead, SharedAreasBook,
		ChatGuest,
	}},
}

// SystemRoleByKey devuelve el rol de sistema con esa clave.
func SystemRoleByKey(key RoleKey) (SystemRole, bool) {
	i := slices.IndexFunc(SystemRoles, func(r SystemRole) bool { return r.Key == key })
	if i < 0 {
		return SystemRole{}, false
	}
	return SystemRoles[i], true
}

func without(all []Permission, excluded ...Permission) []Permission {
	out := make([]Permission, 0, len(all))
	for _, p := range all {
		if !slices.Contains(excluded, p) {
			out = append(out, p)
		}
	}
	return out
}
