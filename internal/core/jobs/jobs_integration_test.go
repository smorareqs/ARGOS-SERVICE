//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/jobs"
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

func reset(t *testing.T) {
	t.Helper()
	if _, err := testDB.Pool().Exec(context.Background(), "DELETE FROM shared.jobs"); err != nil {
		t.Fatal(err)
	}
}

func enqueue(t *testing.T, j jobs.Job) {
	t.Helper()
	if err := testDB.InTx(context.Background(), func(q database.Querier) error {
		return jobs.Enqueue(context.Background(), q, j)
	}); err != nil {
		t.Fatal(err)
	}
}

func status(t *testing.T, jobType string) (st string, attempts int, payload string) {
	t.Helper()
	err := testDB.Pool().QueryRow(context.Background(),
		`SELECT status, attempts, payload::text FROM shared.jobs WHERE type = $1`, jobType).Scan(&st, &attempts, &payload)
	if err != nil {
		t.Fatal(err)
	}
	return st, attempts, payload
}

func TestRunRetryAndScrub(t *testing.T) {
	reset(t)
	ctx := context.Background()
	ran := map[string]int{}
	reg := jobs.NewRegistry()
	reg.Register("ok", func(context.Context, jobs.Running) error { ran["ok"]++; return nil }, jobs.ScrubPayload())
	reg.Register("falla", func(context.Context, jobs.Running) error { ran["falla"]++; return errors.New("SES caido") })
	w := jobs.NewWorker(testDB, reg, testutil.Logger, 1)

	enqueue(t, jobs.Job{Type: "ok", Payload: map[string]string{"link": "secreto"}})
	if ok, err := w.RunOne(ctx); !ok || err != nil {
		t.Fatalf("RunOne = %v, %v", ok, err)
	}
	if st, attempts, payload := status(t, "ok"); st != "done" || attempts != 1 || payload != "{}" {
		t.Errorf("ok: %s %d %s (el payload debe borrarse)", st, attempts, payload)
	}

	enqueue(t, jobs.Job{Type: "falla", MaxAttempts: 2})
	if _, err := w.RunOne(ctx); err != nil {
		t.Fatal(err)
	}
	if st, attempts, _ := status(t, "falla"); st != "pending" || attempts != 1 {
		t.Errorf("tras el primer fallo: %s %d", st, attempts)
	}
	// El reintento espera un minuto: no se toma antes.
	if ran, _ := w.RunOne(ctx); ran {
		t.Error("no debe reintentar antes de la espera")
	}
	if _, err := testDB.Pool().Exec(ctx, `UPDATE shared.jobs SET scheduled_at = now() WHERE type = 'falla'`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.RunOne(ctx); err != nil {
		t.Fatal(err)
	}
	if st, attempts, _ := status(t, "falla"); st != "failed" || attempts != 2 || ran["falla"] != 2 {
		t.Errorf("tras agotar intentos: %s %d (%d ejecuciones)", st, attempts, ran["falla"])
	}
	if ran, _ := w.RunOne(ctx); ran {
		t.Error("la cola debe quedar vacia")
	}
}

func TestDedupeAndUnknownType(t *testing.T) {
	reset(t)
	ctx := context.Background()
	enqueue(t, jobs.Job{Type: "recordatorio", DedupeKey: "alarma:1"})
	enqueue(t, jobs.Job{Type: "recordatorio", DedupeKey: "alarma:1"})
	var n int
	if err := testDB.Pool().QueryRow(ctx, `SELECT count(*) FROM shared.jobs WHERE type = 'recordatorio'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("la misma clave no se encola dos veces: %d %v", n, err)
	}
	w := jobs.NewWorker(testDB, jobs.NewRegistry(), testutil.Logger, 1)
	if _, err := w.RunOne(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := status(t, "recordatorio"); st != "pending" {
		t.Errorf("sin handler se reintenta: %s", st)
	}
}

func TestTenantJobRunsInTenantSchema(t *testing.T) {
	reset(t)
	ctx := context.Background()
	if err := testutil.Reset(ctx, testDB); err != nil {
		t.Fatal(err)
	}
	var tenantID uuid.UUID
	if err := testDB.Pool().QueryRow(ctx, `
		INSERT INTO shared.tenants (slug, name, schema_name, is_active) VALUES ('acme', 'Acme', 'tenant_acme', true) RETURNING id`).
		Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	var seen string
	reg := jobs.NewRegistry()
	reg.Register("tenant", func(ctx context.Context, _ jobs.Running) error {
		seen, _ = database.TenantSchema(ctx)
		return nil
	})
	enqueue(t, jobs.Job{Type: "tenant", TenantID: &tenantID})
	if _, err := jobs.NewWorker(testDB, reg, testutil.Logger, 1).RunOne(ctx); err != nil {
		t.Fatal(err)
	}
	if seen != "tenant_acme" {
		t.Errorf("el handler debe correr con el schema de la empresa: %q", seen)
	}
}
