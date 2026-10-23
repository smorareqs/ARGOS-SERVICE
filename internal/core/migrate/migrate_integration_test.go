//go:build integration

package migrate_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"testing/fstest"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/migrate"
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

func loadSet(t *testing.T, files map[string]string) migrate.Set {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, content := range files {
		fsys["set/"+name] = &fstest.MapFile{Data: []byte(content)}
	}
	set, err := migrate.Load(fsys, "set")
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func freshSchema(t *testing.T, schema string) {
	t.Helper()
	ctx := context.Background()
	_, err := testDB.Pool().Exec(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %[1]s CASCADE; CREATE SCHEMA %[1]s`, schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = testDB.Pool().Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })
}

func inTx[T any](t *testing.T, fn func(q database.Querier) (T, error)) (T, error) {
	t.Helper()
	return database.InTxValue(context.Background(), testDB, fn)
}

func TestUpStatusDownOnTenantSchema(t *testing.T) {
	ctx := context.Background()
	freshSchema(t, "tenant_mig")
	set := loadSet(t, map[string]string{
		"0001_a.sql": "-- migrate:up\nCREATE TABLE a (id int);\n-- migrate:down\nDROP TABLE a;",
		"0002_b.sql": "-- migrate:up\nCREATE TABLE b (id int);\n-- migrate:down\nDROP TABLE b;",
	})

	// Status no crea nada en un schema sin migrar.
	states, err := inTx(t, func(q database.Querier) ([]migrate.State, error) { return set.Status(ctx, q, "tenant_mig") })
	if err != nil || migrate.Pending(states) != 2 {
		t.Fatalf("Status inicial = %+v, %v", states, err)
	}
	var versionTable *string
	if err := testDB.Pool().QueryRow(ctx, `SELECT to_regclass('tenant_mig.schema_migrations')::text`).Scan(&versionTable); err != nil || versionTable != nil {
		t.Errorf("Status no deberia crear la tabla de versiones: %v, %v", versionTable, err)
	}

	applied, err := inTx(t, func(q database.Querier) ([]migrate.Migration, error) { return set.Up(ctx, q, "tenant_mig") })
	if err != nil || len(applied) != 2 {
		t.Fatalf("Up = %v, %v", applied, err)
	}
	// Las tablas sin calificar caen en el schema migrado, no en shared.
	var count int
	if err := testDB.Pool().QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'tenant_mig' AND table_name IN ('a', 'b')`).Scan(&count); err != nil || count != 2 {
		t.Errorf("tablas en tenant_mig = %d, %v", count, err)
	}

	again, err := inTx(t, func(q database.Querier) ([]migrate.Migration, error) { return set.Up(ctx, q, "tenant_mig") })
	if err != nil || len(again) != 0 {
		t.Errorf("Up repetido deberia ser un no-op: %v, %v", again, err)
	}

	reverted, err := inTx(t, func(q database.Querier) (*migrate.Migration, error) { return set.Down(ctx, q, "tenant_mig") })
	if err != nil || reverted == nil || reverted.ID() != "0002_b" {
		t.Fatalf("Down = %v, %v", reverted, err)
	}
	states, err = inTx(t, func(q database.Querier) ([]migrate.State, error) { return set.Status(ctx, q, "tenant_mig") })
	if err != nil || migrate.Pending(states) != 1 || states[0].AppliedAt == nil {
		t.Errorf("Status tras Down = %+v, %v", states, err)
	}
}

func TestUpRejectsOutOfOrderMigration(t *testing.T) {
	ctx := context.Background()
	freshSchema(t, "tenant_order")
	first := loadSet(t, map[string]string{"0002_b.sql": "-- migrate:up\nCREATE TABLE b (id int);"})
	if _, err := inTx(t, func(q database.Querier) ([]migrate.Migration, error) { return first.Up(ctx, q, "tenant_order") }); err != nil {
		t.Fatal(err)
	}
	later := loadSet(t, map[string]string{
		"0001_a.sql": "-- migrate:up\nCREATE TABLE a (id int);",
		"0002_b.sql": "-- migrate:up\nCREATE TABLE b (id int);",
	})
	if _, err := inTx(t, func(q database.Querier) ([]migrate.Migration, error) { return later.Up(ctx, q, "tenant_order") }); err == nil {
		t.Error("una migracion anterior a la ultima aplicada deberia rechazarse")
	}
}

func TestUpRestoresSearchPath(t *testing.T) {
	ctx := context.Background()
	freshSchema(t, "tenant_path")
	set := loadSet(t, map[string]string{"0001_a.sql": "-- migrate:up\nCREATE TABLE a (id int);"})
	path, err := inTx(t, func(q database.Querier) (string, error) {
		if _, err := set.Up(ctx, q, "tenant_path"); err != nil {
			return "", err
		}
		var path string
		err := q.QueryRow(ctx, "SELECT current_setting('search_path')").Scan(&path)
		return path, err
	})
	if err != nil || path != `"shared"` {
		t.Errorf("search_path tras Up = %q, %v; se esperaba el previo", path, err)
	}
}

func TestEmbeddedSharedMigrationsAreApplied(t *testing.T) {
	runner, err := migrate.NewRunner(testDB, testutil.Logger)
	if err != nil {
		t.Fatal(err)
	}
	status, err := runner.Status(context.Background(), migrate.EnvShared, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 1 || migrate.Pending(status[0].States) != 0 {
		t.Errorf("shared deberia estar al dia: %+v", status)
	}
	if _, err := runner.Status(context.Background(), migrate.EnvTenant, "no_existe"); err == nil {
		t.Error("un tenant inexistente deberia dar error")
	}
}
