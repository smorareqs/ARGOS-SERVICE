package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/smorareqs/argos-service/internal/app"
	"github.com/smorareqs/argos-service/internal/modules/auth"
)

func seedCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "platform-admin":
		if len(args) != 4 {
			return fmt.Errorf("%w: seed platform-admin EMAIL PASSWORD NOMBRE", errUsage)
		}
		return seedPlatformAdmin(ctx, args[1], args[2], args[3])
	default:
		return fmt.Errorf("%w: semilla desconocida %q", errUsage, args[0])
	}
}

// seedPlatformAdmin crea el usuario platform_admin inicial (proveedor ARGOS).
// Es idempotente: si el correo ya existe, no hace nada.
func seedPlatformAdmin(ctx context.Context, email, password, fullName string) error {
	cfg, log, db, err := bootstrap(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	services, err := app.NewServices(cfg, db, log)
	if err != nil {
		return err
	}
	_, err = services.Auth.Register(ctx, auth.UserCreate{
		Email:    email,
		Password: password,
		FullName: fullName,
		Role:     auth.RolePlatformAdmin,
	})
	if errors.Is(err, auth.ErrEmailAlreadyExists) {
		fmt.Printf("El usuario '%s' ya existe. No se realizan cambios.\n", email)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("platform_admin creado: %s\n", email)
	return nil
}
