package auth

// Role es el rol de un usuario (PRD seccion 3).
//
// Jerarquia: Proveedor (ARGOS) > Cliente/Tenant > Administradores >
// Supervisores > Empleados. El nivel numerico permite comparaciones de
// autorizacion sencillas (mayor nivel = mas privilegios).
type Role string

// Roles del sistema.
const (
	// RolePlatformAdmin es el rol de plataforma (proveedor ARGOS); opera por
	// encima de los tenants.
	RolePlatformAdmin Role = "platform_admin"
	// RoleAdmin gestiona toda la operacion del tenant (plataforma web).
	RoleAdmin Role = "admin"
	// RoleSupervisor supervisa equipos y valida tareas (app movil).
	RoleSupervisor Role = "supervisor"
	// RoleEmployee ejecuta limpiezas y reporta incidencias (app movil).
	RoleEmployee Role = "employee"
)

// Nivel de privilegio por rol (para checks del tipo "requiere al menos X").
var roleLevel = map[Role]int{
	RoleEmployee:      10,
	RoleSupervisor:    20,
	RoleAdmin:         30,
	RolePlatformAdmin: 100,
}

// Valid indica si r es un rol conocido.
func (r Role) Valid() bool {
	_, ok := roleLevel[r]
	return ok
}

// HasAtLeast indica si role tiene un nivel de privilegio >= required. Un rol
// desconocido nunca cumple.
func HasAtLeast(role, required Role) bool {
	level, ok := roleLevel[role]
	return ok && level >= roleLevel[required]
}
