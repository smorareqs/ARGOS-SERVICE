package main

import (
	"context"

	"github.com/smorareqs/argos-service/internal/app"
)

// worker ejecuta la cola de trabajos (correos, limpieza de archivos y los
// trabajos de cada modulo) hasta recibir SIGINT o SIGTERM.
func worker(ctx context.Context) error {
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
	w, err := app.NewWorker(ctx, cfg, db, log, services)
	if err != nil {
		return err
	}
	return w.Run(ctx)
}
