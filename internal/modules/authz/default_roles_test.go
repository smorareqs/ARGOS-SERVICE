package authz

import (
	"slices"
	"testing"
)

func TestCatalogHasNoDuplicates(t *testing.T) {
	seen := map[Permission]bool{}
	for _, p := range All {
		if seen[p] {
			t.Errorf("permiso duplicado: %s", p)
		}
		seen[p] = true
	}
}

func TestSystemRolesUseCatalogPermissions(t *testing.T) {
	if len(SystemRoles) != 7 {
		t.Fatalf("se esperaban 7 roles de sistema, hay %d", len(SystemRoles))
	}
	for _, r := range SystemRoles {
		if len(r.Permissions) == 0 {
			t.Errorf("%s no tiene permisos", r.Key)
		}
		for _, p := range r.Permissions {
			if !p.Valid() {
				t.Errorf("%s usa un permiso fuera del catalogo: %s", r.Key, p)
			}
		}
	}
}

// Casos puntuales de la matriz del PRD 3.4.
func TestMatrixSpotChecks(t *testing.T) {
	has := func(key RoleKey, p Permission) bool {
		r, ok := SystemRoleByKey(key)
		if !ok {
			t.Fatalf("no existe el rol %s", key)
		}
		return slices.Contains(r.Permissions, p)
	}
	cases := []struct {
		role RoleKey
		perm Permission
		want bool
	}{
		{RoleAdmin, TenantBilling, true},
		{RoleAdmin, HRDisciplinaryOwn, false},
		{RoleSupervisor, UsersRead, true},
		{RoleSupervisor, UsersManage, false},
		{RoleSupervisor, MetricsAll, false},
		{RoleSupervisor, HRDisciplinaryOwn, true},
		{RoleCleaner, TasksExecute, true},
		{RoleCleaner, TasksValidate, false},
		{RoleMaintenance, MaintenanceExecute, true},
		{RoleSecurity, SecurityDashboard, true},
		{RoleOwner, MetricsAll, true},
		{RoleOwner, HRAttendanceOwn, false},
		{RoleGuest, ChatGuest, true},
		{RoleGuest, ChatInternal, false},
	}
	for _, c := range cases {
		if got := has(c.role, c.perm); got != c.want {
			t.Errorf("%s/%s = %v, se esperaba %v", c.role, c.perm, got, c.want)
		}
	}
}

func TestSamePermissionsIgnoresOrder(t *testing.T) {
	if !samePermissions([]Permission{UsersRead, TasksRead}, []Permission{TasksRead, UsersRead}) {
		t.Error("el orden no deberia importar")
	}
	if samePermissions([]Permission{UsersRead}, []Permission{UsersRead, TasksRead}) {
		t.Error("listas distintas no deberian ser iguales")
	}
}
