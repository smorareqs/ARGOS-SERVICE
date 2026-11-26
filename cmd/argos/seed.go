package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/smorareqs/argos-service/internal/app"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
)

// seedPasswordEnv es la contrasena de las cuentas de demo-tenant. Se pasa por
// entorno para no escribirla en la linea de comandos.
const seedPasswordEnv = "ARGOS_SEED_PASSWORD" //nolint:gosec // nombre de la variable de entorno, no una credencial

// Cuenta personal opcional de desarrollo (administradora en todas las empresas
// de demostracion). Se define en el .env local, que no se versiona.
const (
	devEmailEnv    = "ARGOS_SEED_DEV_EMAIL"
	devNameEnv     = "ARGOS_SEED_DEV_NAME"
	devPasswordEnv = "ARGOS_SEED_DEV_PASSWORD" //nolint:gosec // nombre de la variable de entorno, no una credencial
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
	case "demo-tenant":
		if len(args) != 1 {
			return fmt.Errorf("%w: seed demo-tenant (contrasena en %s)", errUsage, seedPasswordEnv)
		}
		return seedDemoTenant(ctx)
	default:
		return fmt.Errorf("%w: semilla desconocida %q", errUsage, args[0])
	}
}

// seedPlatformAdmin crea el administrador de la plataforma ARGOS (sin
// empresa). Es idempotente: si el correo ya existe, no hace nada.
func seedPlatformAdmin(ctx context.Context, email, password, fullName string) error {
	services, closeDB, err := newServices(ctx)
	if err != nil {
		return err
	}
	defer closeDB()

	_, err = services.Auth.CreateUser(ctx, auth.UserCreate{
		Email: email, Password: password, FullName: fullName, IsPlatformAdmin: true,
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

type demoTenant struct {
	slug, name string
	features   map[string]bool
}

type demoMember struct {
	email, name string
	roles       map[string]authz.RoleKey // slug del tenant -> rol
}

// Empresas y personas de demostracion: las mismas que usa el mock de la web
// (WEB/src/app/core/mocks/demo-seed.ts), ahora como datos reales.
var (
	demoTenants = []demoTenant{
		{slug: "garrent", name: "Garrent", features: map[string]bool{"reservations": true, "shared_areas": true, "payroll": true, "guest_chat": true}},
		{slug: "costa_limpia", name: "Costa Limpia", features: map[string]bool{"reservations": true, "parking": true}},
	}
	demoMembers = []demoMember{
		{"ana.lopez@garrent.gt", "Ana López", map[string]authz.RoleKey{"garrent": authz.RoleAdmin}},
		{"jorge.pena@garrent.gt", "Jorge Peña", map[string]authz.RoleKey{"garrent": authz.RoleSupervisor}},
		{"maria.ramos@garrent.gt", "María Ramos", map[string]authz.RoleKey{"garrent": authz.RoleCleaner}},
		{"luis.torres@garrent.gt", "Luis Torres", map[string]authz.RoleKey{"garrent": authz.RoleMaintenance}},
		{"sofia.mendez@garrent.gt", "Sofía Méndez", map[string]authz.RoleKey{"garrent": authz.RoleSecurity}},
		{"ricardo.castillo@garrent.gt", "Ricardo Castillo", map[string]authz.RoleKey{"garrent": authz.RoleOwner}},
		// Fase 2: para que el alcance por propiedades se vea con mas de una
		// persona por rol.
		{"diego.castillo@garrent.gt", "Diego Castillo", map[string]authz.RoleKey{"garrent": authz.RoleMaintenance}},
		{"isabel.monzon@garrent.gt", "Isabel Monzón", map[string]authz.RoleKey{"garrent": authz.RoleOwner}},
		{"pedro.aju@costalimpia.gt", "Pedro Ajú", map[string]authz.RoleKey{"costa_limpia": authz.RoleCleaner}},
		{"rosa.xol@costalimpia.gt", "Rosa Xol", map[string]authz.RoleKey{"costa_limpia": authz.RoleCleaner}},
		{"carlos.ruiz@argos.gt", "Carlos Ruiz", map[string]authz.RoleKey{
			"garrent": authz.RoleSupervisor, "costa_limpia": authz.RoleAdmin,
		}},
	}
)

// seedDemoTenant crea las empresas de demostracion con una persona por rol
// (fase01.md §3.11). Es idempotente: reutiliza lo que ya existe y deja la
// contrasena de cada cuenta en el valor de ARGOS_SEED_PASSWORD. Nunca corre en
// produccion.
func seedDemoTenant(ctx context.Context) error {
	password := os.Getenv(seedPasswordEnv)
	if password == "" {
		return fmt.Errorf("defina %s con la contrasena de las cuentas de demostracion", seedPasswordEnv)
	}
	cfg, log, db, err := bootstrap(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	if cfg.IsProduction() {
		return errors.New("seed demo-tenant no se ejecuta en produccion")
	}
	services, err := app.NewServices(cfg, db, log)
	if err != nil {
		return err
	}

	ids := map[string]tenants.Tenant{}
	for _, t := range demoTenants {
		tenant, err := ensureTenant(ctx, services, t)
		if err != nil {
			return err
		}
		ids[t.slug] = tenant
	}
	for _, m := range demoMembers {
		if err := seedMember(ctx, services, db, ids, m, password); err != nil {
			return err
		}
	}
	if err := seedPhases(ctx, services, db, ids); err != nil {
		return err
	}
	fmt.Printf("Empresas de demostracion listas: %d empresas, %d personas.\n", len(demoTenants), len(demoMembers))

	return seedDevAccount(ctx, services, db, ids)
}

// seedPhases siembra los datos de cada fase, en orden (cada una usa lo de la
// anterior: propiedades -> activos -> reservas).
func seedPhases(ctx context.Context, services app.Services, db *database.DB, ids map[string]tenants.Tenant) error {
	steps := []func() error{
		func() error { return seedOperations(ctx, services, ids) },
		func() error { return seedMaintenance(ctx, services, db, ids) },
		func() error { return seedReservations(ctx, services, db, ids) },
		func() error { return seedTasks(ctx, services, ids) },
		func() error { return seedGuestOps(ctx, services, ids) },
		func() error { return seedAttendance(ctx, services, ids) },
		func() error { return seedHR(ctx, services, ids) },
		func() error { return seedBilling(ctx, db, ids) },
		func() error { return seedInvitations(ctx, services, db, ids) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// seedDevAccount crea la cuenta personal de desarrollo (si esta en el .env):
// administradora en todas las empresas y de la plataforma, para probar el
// alta de empresas.
func seedDevAccount(ctx context.Context, services app.Services, db *database.DB, ids map[string]tenants.Tenant) error {
	dev, devPassword, ok := devMember()
	if !ok {
		return nil
	}
	if err := seedMember(ctx, services, db, ids, dev, devPassword); err != nil {
		return err
	}
	user, err := services.Auth.UserByEmail(ctx, dev.email)
	if err != nil {
		return err
	}
	if err := services.Auth.SetPlatformAdmin(ctx, user.ID, true); err != nil {
		return err
	}
	fmt.Printf("Cuenta de desarrollo lista: %s (rol de administracion en todas las empresas y en la plataforma).\n", dev.email)
	return nil
}

// seedMember crea (o actualiza) la persona y sus membresias.
func seedMember(ctx context.Context, services app.Services, db *database.DB, ids map[string]tenants.Tenant, m demoMember, password string) error {
	user, err := ensureUser(ctx, services, m, password)
	if err != nil {
		return fmt.Errorf("cuenta %s: %w", m.email, err)
	}
	for slug, role := range m.roles {
		tenantID := ids[slug].ID
		if err := db.InTx(ctx, func(q database.Querier) error {
			return authz.AddMember(ctx, q, user.ID, tenantID, role)
		}); err != nil {
			return fmt.Errorf("membresia de %s en %s: %w", m.email, slug, err)
		}
	}
	return nil
}

// devMember lee la cuenta personal de desarrollo del entorno. Sin correo o sin
// contrasena no hay cuenta.
func devMember() (demoMember, string, bool) {
	email, password := strings.TrimSpace(os.Getenv(devEmailEnv)), os.Getenv(devPasswordEnv)
	if email == "" || password == "" {
		return demoMember{}, "", false
	}
	name := strings.TrimSpace(os.Getenv(devNameEnv))
	if name == "" {
		name, _, _ = strings.Cut(email, "@")
	}
	roles := make(map[string]authz.RoleKey, len(demoTenants))
	for _, t := range demoTenants {
		roles[t.slug] = authz.RoleAdmin
	}
	return demoMember{email: email, name: name, roles: roles}, password, true
}

func ensureTenant(ctx context.Context, services app.Services, t demoTenant) (tenants.Tenant, error) {
	tenant, err := services.Tenants.GetTenantBySlug(ctx, t.slug)
	if errors.Is(err, tenants.ErrTenantNotFound) {
		return services.Tenants.CreateTenant(ctx, tenants.TenantCreate{Name: t.name, Slug: t.slug, Features: t.features})
	}
	if err != nil {
		return tenants.Tenant{}, err
	}
	return tenant, services.Tenants.SetFeatures(ctx, tenant.ID, t.features)
}

func ensureUser(ctx context.Context, services app.Services, m demoMember, password string) (auth.User, error) {
	user, err := services.Auth.UserByEmail(ctx, m.email)
	if errors.Is(err, auth.ErrUserNotFound) {
		return services.Auth.CreateUser(ctx, auth.UserCreate{Email: m.email, FullName: m.name, Password: password})
	}
	if err != nil {
		return auth.User{}, err
	}
	return user, services.Auth.SetPassword(ctx, user.ID, password)
}

// newServices conecta y construye los servicios para un comando.
func newServices(ctx context.Context) (app.Services, func(), error) {
	cfg, log, db, err := bootstrap(ctx)
	if err != nil {
		return app.Services{}, nil, err
	}
	services, err := app.NewServices(cfg, db, log)
	if err != nil {
		db.Close()
		return app.Services{}, nil, err
	}
	return services, db.Close, nil
}

// demoInvitations son invitaciones pendientes para ver el flujo completo (el
// correo queda en tmp/mail con el enlace para aceptarla).
var demoInvitations = []struct {
	tenant, inviter, email string
	role                   authz.RoleKey
	message                string
}{
	{
		"garrent", "ana.lopez@garrent.gt", "andrea.soto@garrent.gt", authz.RoleCleaner,
		"Bienvenida al equipo de limpieza. Empiezas el lunes en Torre Vista.",
	},
}

func seedInvitations(ctx context.Context, services app.Services, db *database.DB, ids map[string]tenants.Tenant) error {
	for _, inv := range demoInvitations {
		inviter, err := services.Auth.UserByEmail(ctx, inv.inviter)
		if err != nil {
			return err
		}
		err = db.InTx(ctx, func(q database.Querier) error {
			_, err := services.Auth.InviteTx(ctx, q, auth.InviteInput{
				TenantID: ids[inv.tenant].ID, Inviter: &inviter.ID, Email: inv.email, RoleKey: inv.role, Message: inv.message,
			})
			return err
		})
		// Idempotente: si ya esta invitada o ya acepto, no se toca.
		if errors.Is(err, auth.ErrAlreadyInvited) || errors.Is(err, auth.ErrAlreadyMember) {
			continue
		}
		if err != nil {
			return fmt.Errorf("invitacion de %s: %w", inv.email, err)
		}
	}
	return nil
}
