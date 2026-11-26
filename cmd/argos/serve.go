package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/smorareqs/argos-service/internal/app"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/migrate"
	"github.com/smorareqs/argos-service/internal/server"
)

const shutdownTimeout = 10 * time.Second

func serve(ctx context.Context) error {
	cfg, log, db, err := bootstrap(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := ensureSharedMigrated(ctx, db, log); err != nil {
		return err
	}
	services, err := app.NewServices(cfg, db, log)
	if err != nil {
		return err
	}
	app.EnsureLocalBucket(ctx, cfg, log)
	go services.Realtime.Run(ctx)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.HTTPPort),
		Handler:           server.New(cfg, log, services),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.InfoContext(ctx, "Iniciando ARGOS", "environment", cfg.Environment, "addr", srv.Addr)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}

	log.Info("Deteniendo ARGOS")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// ensureSharedMigrated impide arrancar la API con el schema compartido
// desfasado respecto a las migraciones embebidas en el binario.
func ensureSharedMigrated(ctx context.Context, db *database.DB, log *slog.Logger) error {
	runner, err := migrate.NewRunner(db, log)
	if err != nil {
		return err
	}
	status, err := runner.Status(ctx, migrate.EnvShared, "")
	if err != nil {
		return fmt.Errorf("verificando las migraciones de shared: %w", err)
	}
	if pending := migrate.Pending(status[0].States); pending > 0 {
		return fmt.Errorf("el schema shared tiene %d migraciones pendientes; ejecute `argos migrate up`", pending)
	}
	return nil
}
