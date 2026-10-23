package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/smorareqs/argos-service/internal/core/migrate"
)

type migrateArgs struct {
	// everything es `argos migrate up`: shared y despues todos los tenants.
	everything bool
	env        migrate.Env
	action     string
	tenant     string
}

func parseMigrateArgs(args []string) (migrateArgs, error) {
	if len(args) == 1 && args[0] == "up" {
		return migrateArgs{everything: true, action: "up"}, nil
	}
	if len(args) < 2 {
		return migrateArgs{}, errUsage
	}
	a := migrateArgs{env: migrate.Env(args[0]), action: args[1]}
	flags := flag.NewFlagSet("migrate "+args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&a.tenant, "tenant", "", "slug del tenant o 'all'")
	if err := flags.Parse(args[2:]); err != nil {
		return migrateArgs{}, fmt.Errorf("%w: %w", errUsage, err)
	}
	return a.validated()
}

func (a migrateArgs) validated() (migrateArgs, error) {
	if a.env != migrate.EnvShared && a.env != migrate.EnvTenant {
		return migrateArgs{}, fmt.Errorf("%w: conjunto desconocido %q", errUsage, a.env)
	}
	if !slices.Contains([]string{"up", "down", "status"}, a.action) {
		return migrateArgs{}, fmt.Errorf("%w: accion desconocida %q", errUsage, a.action)
	}
	switch {
	case a.env == migrate.EnvShared && a.tenant != "":
		return migrateArgs{}, fmt.Errorf("%w: -tenant solo aplica a migrate tenant", errUsage)
	case a.env == migrate.EnvTenant && a.tenant == "" && a.action == "down":
		return migrateArgs{}, fmt.Errorf("%w: migrate tenant down exige -tenant SLUG o -tenant all", errUsage)
	case a.env == migrate.EnvTenant && a.tenant == "":
		a.tenant = migrate.AllTenants
	}
	return a, nil
}

func migrateCmd(ctx context.Context, args []string) error {
	a, err := parseMigrateArgs(args)
	if err != nil {
		return err
	}
	runner, closeDB, err := newRunner(ctx)
	if err != nil {
		return err
	}
	defer closeDB()

	if a.everything {
		if err := runner.Up(ctx, migrate.EnvShared, ""); err != nil {
			return err
		}
		return runner.Up(ctx, migrate.EnvTenant, migrate.AllTenants)
	}

	switch a.action {
	case "up":
		return runner.Up(ctx, a.env, a.tenant)
	case "down":
		return runner.Down(ctx, a.env, a.tenant)
	default:
		status, err := runner.Status(ctx, a.env, a.tenant)
		if err != nil {
			return err
		}
		printStatus(os.Stdout, status)
		return nil
	}
}

func newRunner(ctx context.Context) (*migrate.Runner, func(), error) {
	_, log, db, err := bootstrap(ctx)
	if err != nil {
		return nil, nil, err
	}
	runner, err := migrate.NewRunner(db, log)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return runner, db.Close, nil
}

func printStatus(w io.Writer, status []migrate.SchemaStatus) {
	if len(status) == 0 {
		fmt.Fprintln(w, "No hay schemas que mostrar.")
	}
	for _, s := range status {
		fmt.Fprintln(w, s.Schema)
		if len(s.States) == 0 {
			fmt.Fprintln(w, "  (sin migraciones)")
		}
		for _, st := range s.States {
			if st.AppliedAt == nil {
				fmt.Fprintf(w, "  [ ] %-40s pendiente\n", st.ID())
				continue
			}
			fmt.Fprintf(w, "  [x] %-40s %s\n", st.ID(), st.AppliedAt.Format("2006-01-02 15:04:05 MST"))
		}
	}
}
