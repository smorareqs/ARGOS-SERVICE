// Comando argos: binario unico del backend.
//
//	argos serve                     Arranca la API HTTP
//	argos migrate ...               Migraciones de base de datos
//	argos seed ...                  Datos iniciales
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/smorareqs/argos-service/internal/core/config"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/logging"
)

const usage = `Uso: argos <comando> [argumentos]

Comandos:
  serve                                         Arranca la API HTTP
  migrate up                                    Aplica las migraciones de shared y de todos los tenants
  migrate shared up|down|status                 Migraciones del schema compartido
  migrate tenant up|down|status [-tenant SLUG]  Migraciones de tenants (por defecto todos; down exige -tenant)
  seed platform-admin EMAIL PASSWORD NOMBRE     Crea el primer platform_admin (idempotente)
  seed demo-tenant                              Empresas y personas de demostracion (contrasena en ARGOS_SEED_PASSWORD)
`

var errUsage = errors.New("argumentos invalidos")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:])
	stop()
	if err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprint(os.Stderr, usage)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "serve":
		return serve(ctx)
	case "migrate":
		return migrateCmd(ctx, args[1:])
	case "seed":
		return seedCmd(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("%w: comando desconocido %q", errUsage, args[0])
	}
}

// bootstrap carga la configuracion, el logger y el pool de conexiones.
func bootstrap(ctx context.Context) (config.Config, *slog.Logger, *database.DB, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	log := logging.New(cfg)

	opts := database.Options{MaxConns: cfg.DBMaxConns}
	if cfg.DBEcho {
		opts.EchoLogger = log
	}
	db, err := database.Connect(ctx, cfg.DatabaseURL(), opts)
	if err != nil {
		return config.Config{}, nil, nil, err
	}
	return cfg, log, db, nil
}
