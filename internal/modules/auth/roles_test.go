package auth

import "testing"

func TestRoleHierarchy(t *testing.T) {
	cases := []struct {
		role, required Role
		want           bool
	}{
		{RoleAdmin, RoleEmployee, true},
		{RoleEmployee, RoleAdmin, false},
		{RolePlatformAdmin, RoleAdmin, true},
		{RoleSupervisor, RoleSupervisor, true},
		{RoleAdmin, RolePlatformAdmin, false},
		{Role("desconocido"), RoleEmployee, false},
	}
	for _, c := range cases {
		if got := HasAtLeast(c.role, c.required); got != c.want {
			t.Errorf("HasAtLeast(%s, %s) = %v, se esperaba %v", c.role, c.required, got, c.want)
		}
	}
}
