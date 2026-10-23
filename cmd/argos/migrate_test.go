package main

import (
	"errors"
	"testing"

	"github.com/smorareqs/argos-service/internal/core/migrate"
)

func TestParseMigrateArgs(t *testing.T) {
	valid := []struct {
		args []string
		want migrateArgs
	}{
		{[]string{"up"}, migrateArgs{everything: true, action: "up"}},
		{[]string{"shared", "status"}, migrateArgs{env: migrate.EnvShared, action: "status"}},
		{[]string{"tenant", "up"}, migrateArgs{env: migrate.EnvTenant, action: "up", tenant: migrate.AllTenants}},
		{[]string{"tenant", "down", "-tenant", "acme"}, migrateArgs{env: migrate.EnvTenant, action: "down", tenant: "acme"}},
		{[]string{"tenant", "down", "-tenant", "all"}, migrateArgs{env: migrate.EnvTenant, action: "down", tenant: "all"}},
	}
	for _, c := range valid {
		got, err := parseMigrateArgs(c.args)
		if err != nil || got != c.want {
			t.Errorf("parseMigrateArgs(%v) = %+v, %v; se esperaba %+v", c.args, got, err, c.want)
		}
	}

	invalid := [][]string{
		{},
		{"down"},
		{"shared"},
		{"otro", "up"},
		{"shared", "borrar"},
		{"shared", "up", "-tenant", "acme"},
		{"tenant", "down"},
		{"tenant", "up", "-desconocido"},
	}
	for _, args := range invalid {
		if _, err := parseMigrateArgs(args); !errors.Is(err, errUsage) {
			t.Errorf("parseMigrateArgs(%v) deberia fallar con errUsage, se obtuvo %v", args, err)
		}
	}
}
