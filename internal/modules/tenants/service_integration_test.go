//go:build integration

package tenants_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/migrate"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
	"github.com/smorareqs/argos-service/internal/testutil"
)

var testDB *database.DB

func TestMain(m *testing.M) {
	db, cleanup, err := testutil.StartDB(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testDB = db
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func newService(t *testing.T, migrations migrate.Set) *tenants.Service {
	t.Helper()
	if err := testutil.Reset(context.Background(), testDB); err != nil {
		t.Fatal(err)
	}
	return tenants.NewService(testDB, migrations, testutil.Logger)
}

func schemaExists(t *testing.T, schema string) bool {
	t.Helper()
	exists, err := tenants.NewRepository(testDB.Pool()).SchemaExists(context.Background(), schema)
	if err != nil {
		t.Fatal(err)
	}
	return exists
}

func TestCreateTenantProvisionsSchema(t *testing.T) {
	ctx := context.Background()
	svc := newService(t, migrate.Set{})

	tenant, err := svc.CreateTenant(ctx, tenants.TenantCreate{Name: "Acme Rentals", Slug: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if tenant.ID == uuid.Nil || tenant.SchemaName != "tenant_acme" || !tenant.IsActive {
		t.Errorf("tenant inesperado: %+v", tenant)
	}
	if !schemaExists(t, "tenant_acme") {
		t.Error("no se creo el schema tenant_acme")
	}

	got, err := svc.GetTenant(ctx, tenant.ID)
	if err != nil || got.Slug != "acme" {
		t.Errorf("GetTenant = %+v, %v", got, err)
	}
}

func TestCreateTenantDuplicateSlugFails(t *testing.T) {
	ctx := context.Background()
	svc := newService(t, migrate.Set{})
	if _, err := svc.CreateTenant(ctx, tenants.TenantCreate{Name: "Beta", Slug: "beta"}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CreateTenant(ctx, tenants.TenantCreate{Name: "Beta 2", Slug: " BETA "})
	if !errors.Is(err, tenants.ErrTenantSlugAlreadyExists) {
		t.Errorf("se esperaba ErrTenantSlugAlreadyExists, se obtuvo %v", err)
	}
}

// Un schema huerfano (sin fila en shared.tenants) nunca se adopta.
func TestCreateTenantRefusesExistingSchema(t *testing.T) {
	ctx := context.Background()
	svc := newService(t, migrate.Set{})
	if _, err := testDB.Pool().Exec(ctx, `CREATE SCHEMA tenant_ghost`); err != nil {
		t.Fatal(err)
	}
	_, err := svc.CreateTenant(ctx, tenants.TenantCreate{Name: "Ghost", Slug: "ghost"})
	if !errors.Is(err, tenants.ErrTenantSlugAlreadyExists) {
		t.Errorf("se esperaba ErrTenantSlugAlreadyExists, se obtuvo %v", err)
	}
	if _, err := svc.GetTenant(ctx, uuid.New()); !errors.Is(err, tenants.ErrTenantNotFound) {
		t.Errorf("se esperaba ErrTenantNotFound, se obtuvo %v", err)
	}
}

// El alta aplica las migraciones de tenant dentro de la misma transaccion: si
// una falla, no queda ni el registro ni el schema.
func TestCreateTenantAppliesTenantMigrationsAtomically(t *testing.T) {
	ctx := context.Background()
	good, err := migrate.Load(fstest.MapFS{
		"tenant/0001_base.sql": {Data: []byte("-- migrate:up\nCREATE TABLE files (id uuid PRIMARY KEY DEFAULT gen_random_uuid());")},
	}, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	svc := newService(t, good)
	tenant, err := svc.CreateTenant(ctx, tenants.TenantCreate{Name: "Gamma", Slug: "gamma"})
	if err != nil {
		t.Fatal(err)
	}
	var tables int
	err = testDB.Pool().QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name IN ('files', 'schema_migrations')`,
		tenant.SchemaName).Scan(&tables)
	if err != nil || tables != 2 {
		t.Errorf("el schema del tenant deberia tener files y schema_migrations: %d, %v", tables, err)
	}

	broken, err := migrate.Load(fstest.MapFS{
		"tenant/0001_base.sql": {Data: []byte("-- migrate:up\nCREATE TABLE files (id uuid PRIMARY KEY);\nSELECT * FROM tabla_inexistente;")},
	}, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	svc = newService(t, broken)
	if _, err := svc.CreateTenant(ctx, tenants.TenantCreate{Name: "Delta", Slug: "delta"}); err == nil {
		t.Fatal("se esperaba un error de migracion")
	}
	list, err := svc.ListTenants(ctx, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 || schemaExists(t, "tenant_delta") {
		t.Errorf("el alta fallida dejo restos: %d tenants, schema=%v", len(list), schemaExists(t, "tenant_delta"))
	}
}
