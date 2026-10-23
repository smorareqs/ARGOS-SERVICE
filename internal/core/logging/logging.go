// Package logging configura el logger de la aplicacion (log/slog).
package logging

import (
	"log/slog"
	"os"

	"github.com/smorareqs/argos-service/internal/core/config"
)

// New crea el logger raiz y lo fija como logger por defecto de slog.
//
// En staging y produccion emite JSON (apto para CloudWatch); en local y
// testing emite texto legible. El nivel es DEBUG si ARGOS_DEBUG esta activo.
func New(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	if cfg.Debug {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch cfg.Environment {
	case config.Staging, config.Production:
		handler = slog.NewJSONHandler(os.Stdout, opts)
	default:
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}
