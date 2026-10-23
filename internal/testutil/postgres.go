//go:build integration

// Package testutil levanta un PostgreSQL efimero para las pruebas de
// integracion (ARCHITECTURE.md: Testcontainers).
package testutil

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/migrate"
)

// DatabaseURLEnv permite usar un PostgreSQL ya levantado en lugar de un
// contenedor nuevo. Las pruebas truncan sus tablas: nunca apuntarlo a una base
// con datos reales. Con varios paquetes, ejecutar con `go test -p 1`.
const DatabaseURLEnv = "ARGOS_TEST_DATABASE_URL"

// Logger descarta los logs durante las pruebas.
var Logger = slog.New(slog.DiscardHandler)

// StartDB devuelve una base con las migraciones de shared aplicadas y una
// funcion para liberarla. Pensado para TestMain.
func StartDB(ctx context.Context) (*database.DB, func(), error) {
	dsn := os.Getenv(DatabaseURLEnv)
	terminate := func() {}
	if dsn == "" {
		container, err := postgres.Run(ctx, "postgres:16-alpine",
			postgres.WithDatabase("argos_test"),
			postgres.WithUsername("argos"),
			postgres.WithPassword("argos"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(90*time.Second),
			),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("levantando PostgreSQL: %w", err)
		}
		terminate = func() { _ = testcontainers.TerminateContainer(container) }
		dsn, err = container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			terminate()
			return nil, nil, err
		}
	}

	db, err := database.Connect(ctx, dsn, database.Options{MaxConns: 10})
	if err != nil {
		terminate()
		return nil, nil, err
	}
	runner, err := migrate.NewRunner(db, Logger)
	if err == nil {
		err = runner.Up(ctx, migrate.EnvShared, "")
	}
	if err != nil {
		db.Close()
		terminate()
		return nil, nil, fmt.Errorf("migrando shared: %w", err)
	}
	return db, func() { db.Close(); terminate() }, nil
}

// Reset vacia las tablas globales y elimina los schemas de tenant.
func Reset(ctx context.Context, db *database.DB) error {
	_, err := db.Pool().Exec(ctx, `
		TRUNCATE shared.users, shared.tenants CASCADE;
		DO $$
		DECLARE s text;
		BEGIN
			FOR s IN SELECT nspname FROM pg_namespace WHERE nspname LIKE 'tenant\_%' LOOP
				EXECUTE format('DROP SCHEMA %I CASCADE', s);
			END LOOP;
		END $$;`)
	return err
}
