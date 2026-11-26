// Package app construye los servicios de dominio y sus dependencias
// (inyeccion de dependencias). Lo usan el servidor HTTP y los comandos CLI.
//
// Archivo compartido: cada modulo nuevo agrega su campo y su linea de
// construccion; nunca se reordena.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/smorareqs/argos-service/internal/core/config"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/migrate"
	"github.com/smorareqs/argos-service/internal/core/ratelimit"
	"github.com/smorareqs/argos-service/internal/core/security"
	"github.com/smorareqs/argos-service/internal/core/storage"
	"github.com/smorareqs/argos-service/internal/modules/alerts"
	"github.com/smorareqs/argos-service/internal/modules/apikeys"
	"github.com/smorareqs/argos-service/internal/modules/appsync"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/billing"
	"github.com/smorareqs/argos-service/internal/modules/checklists"
	"github.com/smorareqs/argos-service/internal/modules/dashboard"
	"github.com/smorareqs/argos-service/internal/modules/disciplinary"
	"github.com/smorareqs/argos-service/internal/modules/employees"
	"github.com/smorareqs/argos-service/internal/modules/files"
	"github.com/smorareqs/argos-service/internal/modules/guestaccess"
	"github.com/smorareqs/argos-service/internal/modules/health"
	"github.com/smorareqs/argos-service/internal/modules/inventory"
	"github.com/smorareqs/argos-service/internal/modules/maintenance"
	"github.com/smorareqs/argos-service/internal/modules/notifications"
	"github.com/smorareqs/argos-service/internal/modules/parking"
	"github.com/smorareqs/argos-service/internal/modules/payroll"
	"github.com/smorareqs/argos-service/internal/modules/properties"
	"github.com/smorareqs/argos-service/internal/modules/realtime"
	"github.com/smorareqs/argos-service/internal/modules/reservations"
	"github.com/smorareqs/argos-service/internal/modules/securityops"
	"github.com/smorareqs/argos-service/internal/modules/sharedareas"
	"github.com/smorareqs/argos-service/internal/modules/tasks"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
)

// Services agrupa los servicios de dominio.
type Services struct {
	Health  *health.Service
	Auth    *auth.Service
	Tenants *tenants.Service
	Authz   *authz.Service
	Storage *storage.Service
	Files   *files.Service
	// Fase 2 (Persona A).
	Properties *properties.Service
	Checklists *checklists.Service
	Employees  *employees.Service
	Audit      *audit.Service
	Sync       *appsync.Service
	// Fase 3 (Persona A). Events es el bus de eventos de dominio: notificaciones
	// (Persona B) se suscribe a ticket.*, maintenance.due y alert.raised.
	Events      *events.Bus
	Alerts      *alerts.Service
	Maintenance *maintenance.Service
	Dashboard   *dashboard.Service
	// Fase 4 (Persona A). Publica reservation.* (B: accesos, alarmas y limpiezas).
	Reservations *reservations.Service
	// Fase 5 (Persona A). Publican payroll.closed y disciplinary.* (B: push).
	Payroll      *payroll.Service
	Disciplinary *disciplinary.Service
	// Fase 6 (Persona A): facturacion (limites del plan y solo lectura), claves
	// API y su limite por clave en POST /public/reservations.
	Billing       *billing.Service
	APIKeys       *apikeys.Service
	APIKeyLimiter *ratelimit.Limiter
	// Configuracion que necesita el cliente web (mapa real).
	Client ClientConfig
	// Fase 2 (Persona B): itinerario y tareas, inventario.
	Inventory *inventory.Service
	Tasks     *tasks.Service
	// Fase 3 (Persona B): bandeja, push y correo de avisos; tiempo real.
	Notifications *notifications.Service
	Realtime      *realtime.Hub
	// Fase 4 (Persona B): huesped en la app, parqueos, areas comunes y
	// seguridad (alarmas de salida y tablero).
	GuestAccess *guestaccess.Service
	Parking     *parking.Service
	SharedAreas *sharedareas.Service
	Security    *securityops.Service
}

// ClientConfig es lo que la web pide al iniciar sesion (GET /client-config).
type ClientConfig struct {
	MapsKey   string
	MapsMapID string
}

// NewServices construye todos los servicios.
func NewServices(cfg config.Config, db *database.DB, log *slog.Logger) (Services, error) {
	tokens, err := security.NewTokenManager(
		cfg.SecretKey,
		cfg.JWTAlgorithm,
		time.Duration(cfg.AccessTokenExpireMinutes)*time.Minute,
		time.Duration(cfg.RefreshTokenExpireDays)*24*time.Hour,
	)
	if err != nil {
		return Services{}, err
	}
	tenantMigrations, err := migrate.Tenant()
	if err != nil {
		return Services{}, err
	}
	fallbackKey := ""
	if cfg.IsDevelopment() {
		fallbackKey = cfg.SecretKey
	}
	cipher, err := security.NewFieldCipher(cfg.FieldEncryptionKey, fallbackKey)
	if err != nil {
		return Services{}, err
	}
	store, err := NewStorage(context.Background(), cfg)
	if err != nil {
		return Services{}, err
	}

	authSvc := auth.NewService(db, tokens, log.With("module", "auth"), auth.Options{
		RefreshTTL: time.Duration(cfg.RefreshTokenExpireDays) * 24 * time.Hour,
		Cipher:     cipher,
		Storage:    store,
		WebBaseURL: cfg.WebBaseURL,
	})
	props := properties.NewService(db, cipher, store, log.With("module", "properties"))
	bus := events.New()
	alertsSvc := alerts.NewService(db, bus, log.With("module", "alerts"))
	maint := maintenance.NewService(db, store, alertsSvc, bus, log.With("module", "maintenance"))
	res := reservations.NewService(db, store, maint, bus, log.With("module", "reservations"), reservations.Options{
		AllowPrivateFeeds: cfg.IsDevelopment(), WebBaseURL: cfg.WebBaseURL,
	})
	sharedMigrations, err := migrate.Shared()
	if err != nil {
		return Services{}, err
	}
	bill := billing.NewService(db, newPaymentProvider(cfg, log), log.With("module", "billing"), cfg.WebBaseURL)
	props.SetLimits(bill)
	authSvc.SetLimits(bill)
	tenantSvc := tenants.NewService(db, tenantMigrations, log.With("module", "tenants"),
		tenants.WithAuth(authSvc), tenants.WithStorage(store))
	tenantSvc.SetBilling(bill)
	dash := dashboard.NewService(db, maint, log.With("module", "dashboard"))
	// Fase 2 (Persona B): tareas se engancha a propiedades (acceso del dia),
	// mantenimiento (tarea de la incidencia), el Inicio y las reservas.
	inv := inventory.NewService(db, bus, log.With("module", "inventory"))
	taskSvc := tasks.NewService(db, bus, inv, maint, log.With("module", "tasks"))
	props.SetAccessGate(taskSvc.AccessGate)
	maint.SetTaskLinker(taskSvc.LinkTicket)
	dash.SetTasksSource(taskSvc.TodayBlock)
	taskSvc.Subscribe(bus)
	// Fase 3 (Persona B): los avisos se arman con los eventos de dominio.
	var push notifications.PushSender
	if cfg.FCMCredentials != "" {
		fcm, err := notifications.NewFCM(cfg.FCMCredentials)
		if err != nil {
			return Services{}, err
		}
		push = fcm
	}
	notif := notifications.NewService(db, push, cfg.WebBaseURL, log.With("module", "notifications"))
	notif.Subscribe(bus)
	// Fase 4 (Persona B): accesos del huesped al confirmar, alarmas de salida y
	// parqueos y areas que siguen a la reserva.
	park := parking.NewService(db, log.With("module", "parking"))
	park.Subscribe(bus)
	areas := sharedareas.NewService(db, log.With("module", "shared_areas"))
	areas.Subscribe(bus)
	sec := securityops.NewService(db, notif, alertsSvc, log.With("module", "security"))
	sec.Subscribe(bus)
	guest := guestaccess.NewService(db, tokens, maint, areas, log.With("module", "guest_access"))
	res.SetGuestAccess(guest.Issue)
	return Services{
		Health: health.NewService(db, string(cfg.Environment)).WithReadiness(health.Readiness{
			DB: db, Storage: store, Shared: sharedMigrations, Tenant: tenantMigrations,
		}),
		Auth:    authSvc,
		Tenants: tenantSvc,
		Authz:   authz.NewService(db, log.With("module", "authz")),
		Storage: store,
		Files:   files.NewService(db, store, log.With("module", "files")),

		Properties: props,
		Checklists: checklists.NewService(db, log.With("module", "checklists")),
		Employees:  employees.NewService(db, log.With("module", "employees")),
		Audit:      audit.NewService(db),
		Sync:       appsync.NewService(db, props, log.With("module", "sync")),

		Events:      bus,
		Alerts:      alertsSvc,
		Maintenance: maint,
		Dashboard:   dash,

		Reservations: res,

		Payroll:      payroll.NewService(db, store, bus, log.With("module", "payroll")),
		Disciplinary: disciplinary.NewService(db, store, bus, log.With("module", "disciplinary")),

		Billing:       bill,
		APIKeys:       apikeys.NewService(db, log.With("module", "apikeys")),
		APIKeyLimiter: ratelimit.PerMinute(cfg.RateLimitAPIKey),
		Client:        ClientConfig{MapsKey: cfg.GoogleMapsKey, MapsMapID: cfg.GoogleMapsMapID},

		Inventory: inv,
		Tasks:     taskSvc,

		Notifications: notif,
		Realtime:      realtime.NewHub(db, authSvc, log.With("module", "realtime")),

		GuestAccess: guest,
		Parking:     park,
		SharedAreas: areas,
		Security:    sec,
	}, nil
}

// newPaymentProvider elige la pasarela. Mientras no se decida el proveedor
// (avance.md) solo existe la simulada; el secreto de sus webhooks sale de
// ARGOS_BILLING_WEBHOOK_SECRET o, si falta, de SECRET_KEY.
func newPaymentProvider(cfg config.Config, log *slog.Logger) billing.Provider {
	secret := cfg.BillingWebhookSecret
	if secret == "" {
		sum := sha256.Sum256([]byte("billing-webhook:" + cfg.SecretKey))
		secret = hex.EncodeToString(sum[:])
	}
	if cfg.BillingProvider != "" && cfg.BillingProvider != "fake" {
		log.Warn("Pasarela de pago desconocida; se usa la simulada", "provider", cfg.BillingProvider)
	}
	return billing.NewFakeProvider(secret)
}

// NewStorage elige el almacen: S3 (o el emulador local) si hay bucket; si no,
// memoria (solo pruebas: los archivos no sobreviven al proceso).
func NewStorage(ctx context.Context, cfg config.Config) (*storage.Service, error) {
	if cfg.S3Bucket == "" {
		return storage.NewService(storage.NewMemory()), nil
	}
	backend, err := storage.NewS3(ctx, storage.S3Options{
		Region: cfg.AWSRegion, Bucket: cfg.S3Bucket, Endpoint: cfg.S3EndpointURL,
		PublicEndpoint: cfg.S3PublicEndpointURL, AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey,
	})
	if err != nil {
		return nil, err
	}
	return storage.NewService(backend), nil
}

// EnsureLocalBucket crea el bucket del emulador S3 local y su CORS para que
// el navegador suba directo. Solo en local con ARGOS_S3_ENDPOINT_URL; si el
// emulador aun no responde, reintenta unos segundos y deja un aviso.
func EnsureLocalBucket(ctx context.Context, cfg config.Config, log *slog.Logger) {
	if cfg.S3EndpointURL == "" || cfg.S3Bucket == "" || !cfg.IsDevelopment() {
		return
	}
	backend, err := storage.NewS3(ctx, storage.S3Options{
		Region: cfg.AWSRegion, Bucket: cfg.S3Bucket, Endpoint: cfg.S3EndpointURL,
		AccessKey: cfg.S3AccessKey, SecretKey: cfg.S3SecretKey,
	})
	if err != nil {
		log.WarnContext(ctx, "S3 local no configurado", "error", err)
		return
	}
	for attempt := 1; ; attempt++ {
		err = backend.EnsureBucket(ctx, cfg.CORSOrigins)
		if err == nil || attempt == 10 || ctx.Err() != nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.WarnContext(ctx, "No se pudo preparar el bucket local", "bucket", cfg.S3Bucket, "error", err)
		return
	}
	log.InfoContext(ctx, "Bucket local listo", "bucket", cfg.S3Bucket)
}
