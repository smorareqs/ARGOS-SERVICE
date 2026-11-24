package tenants

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/migrate"
	"github.com/smorareqs/argos-service/internal/core/storage"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Ajustes por defecto de una empresa (avance.md: horas de check-in/check-out
// y anticipacion de la alarma, pendientes de confirmar con el cliente).
const (
	defaultCheckin  = "15:00"
	defaultCheckout = "11:00"
	defaultAlarm    = 30
)

var hhmm = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// Service contiene los casos de uso de tenants. Orquesta el alta: valida
// unicidad, persiste el registro en el schema compartido y aprovisiona su
// schema dedicado.
type Service struct {
	db         *database.DB
	migrations migrate.Set
	log        *slog.Logger
	auth       *auth.Service
	storage    *storage.Service
	billing    Billing
}

// Billing es lo que el alta y las banderas necesitan de facturacion (fase 6):
// la suscripcion de prueba de una empresa nueva y que las banderas quepan en
// su plan. Lo implementa billing.Service.
type Billing interface {
	EnsureSubscription(ctx context.Context, q database.Querier, tenantID uuid.UUID, planKey string, trialDays int) error
	CheckFeatures(ctx context.Context, q database.Querier, tenantID uuid.UUID, features map[string]bool) error
}

// SetBilling engancha la facturacion.
func (s *Service) SetBilling(b Billing) { s.billing = b }

// Option completa las dependencias del servicio.
type Option func(*Service)

// WithAuth permite invitar al administrador en el alta de una empresa.
func WithAuth(a *auth.Service) Option { return func(s *Service) { s.auth = a } }

// WithStorage permite firmar las URLs de los logos.
func WithStorage(st *storage.Service) Option { return func(s *Service) { s.storage = st } }

// NewService crea el servicio. migrations es el conjunto de migraciones de
// tenant que se aplica a cada schema nuevo.
func NewService(db *database.DB, migrations migrate.Set, log *slog.Logger, opts ...Option) *Service {
	s := &Service{db: db, migrations: migrations, log: log}
	for _, o := range opts {
		o(s)
	}
	return s
}

// CreateTenant crea un tenant sin administrador (seeders y pruebas).
func (s *Service) CreateTenant(ctx context.Context, in TenantCreate) (Tenant, error) {
	return s.CreateTenantBy(ctx, nil, in)
}

// CreateTenantBy crea un tenant y aprovisiona su schema en una sola
// transaccion: registro, schema, migraciones, siete roles, administrador
// (membresia o invitacion) y bitacora de plataforma. Si cualquier paso falla
// no queda nada (fase01.md §3.2).
func (s *Service) CreateTenantBy(ctx context.Context, actor *uuid.UUID, in TenantCreate) (Tenant, error) {
	in, err := validCreate(in)
	if err != nil {
		return Tenant{}, err
	}
	tenant, err := database.InTxValue(ctx, s.db, func(q database.Querier) (Tenant, error) {
		tenant, err := s.provision(ctx, q, in)
		if err != nil {
			return Tenant{}, err
		}
		admin, err := s.addAdmin(ctx, q, tenant.ID, actor, in.AdminEmail)
		if err != nil {
			return Tenant{}, err
		}
		if s.billing != nil {
			if err := s.billing.EnsureSubscription(ctx, q, tenant.ID, in.PlanKey, in.TrialDays); err != nil {
				return Tenant{}, err
			}
			if err := s.billing.CheckFeatures(ctx, q, tenant.ID, tenant.Features); err != nil {
				return Tenant{}, err
			}
		}
		meta := map[string]any{"slug": tenant.Slug, "name": tenant.Name, "features": tenant.Features}
		if in.AdminEmail != "" {
			meta["admin_email"], meta["admin"] = in.AdminEmail, admin
		}
		if err := audit.RecordPlatform(ctx, q, audit.PlatformEntry{
			Actor: actor, TenantID: &tenant.ID, Action: "tenant.created", Entity: "tenant", EntityID: &tenant.ID,
			Metadata: meta,
		}); err != nil {
			return Tenant{}, err
		}
		return tenant, nil
	})
	if err != nil {
		return Tenant{}, err
	}
	s.log.InfoContext(ctx, "Tenant creado", "tenant_id", tenant.ID, "slug", tenant.Slug)
	return tenant, nil
}

// validCreate normaliza y valida el alta (slug, nombre, zona y correo).
func validCreate(in TenantCreate) (TenantCreate, error) {
	slug, err := NormalizeSlug(in.Slug)
	if err != nil {
		return in, err
	}
	in.Slug, in.Name = slug, strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(in.Name); n < 1 || n > 255 {
		return in, ErrInvalidName.WithField("body.name")
	}
	in.Timezone = strings.TrimSpace(in.Timezone)
	if in.Timezone != "" {
		if _, err := time.LoadLocation(in.Timezone); err != nil {
			return in, ErrInvalidTimezone.WithField("body.timezone")
		}
	}
	in.AdminEmail = strings.TrimSpace(in.AdminEmail)
	if in.AdminEmail != "" {
		if in.AdminEmail, err = auth.NormalizeEmail(in.AdminEmail); err != nil {
			return in, ErrInvalidAdminMail.WithField("body.admin_email")
		}
	}
	return in, nil
}

// provision inserta el tenant, crea su schema con las migraciones de tenant y
// siembra los siete roles de sistema con la matriz del PRD.
func (s *Service) provision(ctx context.Context, q database.Querier, in TenantCreate) (Tenant, error) {
	repo := NewRepository(q)
	existing, err := repo.GetBySlug(ctx, in.Slug)
	if err != nil {
		return Tenant{}, err
	}
	if existing != nil {
		return Tenant{}, ErrTenantSlugAlreadyExists
	}
	schemaName := BuildSchemaName(in.Slug)
	tenant, err := repo.Add(ctx, Tenant{
		Name: in.Name, Slug: in.Slug, SchemaName: schemaName, IsActive: true, Timezone: in.Timezone,
		Features: normalizedFeatures(in.Features),
	})
	if err != nil {
		return Tenant{}, err
	}
	if err := repo.CreateSchema(ctx, schemaName); err != nil {
		return Tenant{}, err
	}
	if _, err := s.migrations.Up(ctx, q, schemaName); err != nil {
		return Tenant{}, err
	}
	if _, err := authz.SeedSystemRoles(ctx, q, tenant.ID); err != nil {
		return Tenant{}, err
	}
	return tenant, nil
}

// addAdmin deja a la persona como administradora: con cuenta, membresia
// activa; sin cuenta, invitacion por correo. Devuelve "member" o "invited".
func (s *Service) addAdmin(ctx context.Context, q database.Querier, tenantID uuid.UUID, actor *uuid.UUID, email string) (string, error) {
	if email == "" {
		return "", nil
	}
	user, err := auth.NewRepository(q).GetByEmail(ctx, email)
	if err != nil {
		return "", err
	}
	if user != nil && user.HashedPassword != nil {
		return "member", authz.AddMember(ctx, q, user.ID, tenantID, authz.RoleAdmin)
	}
	if s.auth == nil {
		return "", ErrInvalidAdminMail
	}
	_, err = s.auth.InviteTx(ctx, q, auth.InviteInput{
		TenantID: tenantID, Inviter: actor, Email: email, RoleKey: authz.RoleAdmin,
	})
	return "invited", err
}

// GetTenant devuelve el tenant o ErrTenantNotFound.
func (s *Service) GetTenant(ctx context.Context, id uuid.UUID) (Tenant, error) {
	tenant, err := database.InTxValue(ctx, s.db, func(q database.Querier) (*Tenant, error) {
		return NewRepository(q).GetByID(ctx, id)
	})
	if err != nil {
		return Tenant{}, err
	}
	if tenant == nil {
		return Tenant{}, ErrTenantNotFound
	}
	return *tenant, nil
}

// GetTenantBySlug devuelve el tenant o ErrTenantNotFound.
func (s *Service) GetTenantBySlug(ctx context.Context, slug string) (Tenant, error) {
	tenant, err := database.InTxValue(ctx, s.db, func(q database.Querier) (*Tenant, error) {
		return NewRepository(q).GetBySlug(ctx, slug)
	})
	if err != nil {
		return Tenant{}, err
	}
	if tenant == nil {
		return Tenant{}, ErrTenantNotFound
	}
	return *tenant, nil
}

// SetFeatures reemplaza las banderas de funcionalidad del tenant.
func (s *Service) SetFeatures(ctx context.Context, id uuid.UUID, features map[string]bool) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		return NewRepository(q).SetFeatures(ctx, id, normalizedFeatures(features))
	})
}

// ListTenants devuelve una pagina de tenants.
func (s *Service) ListTenants(ctx context.Context, limit, offset int) ([]Tenant, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]Tenant, error) {
		return NewRepository(q).ListAll(ctx, limit, offset)
	})
}

// PlatformTenants es la lista de la plataforma (W-P1), con sus numeros.
func (s *Service) PlatformTenants(ctx context.Context, limit, offset int) ([]TenantRead, error) {
	list, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]TenantStats, error) {
		return NewRepository(q).ListWithStats(ctx, limit, offset)
	})
	if err != nil {
		return nil, err
	}
	out := make([]TenantRead, 0, len(list))
	for _, t := range list {
		r := s.Read(ctx, t.Tenant)
		r.Members, r.ActiveMembers, r.LastActivity = t.Members, t.ActiveMembers, t.LastActivity
		out = append(out, r)
	}
	return out, nil
}

// Read es NewTenantRead con la URL firmada del logo.
func (s *Service) Read(ctx context.Context, t Tenant) TenantRead {
	r := NewTenantRead(t)
	r.LogoURL = s.storage.LogoURL(ctx, t.ID, t.LogoKey)
	return r
}

// UpdateTenant cambia nombre, estado o banderas desde la plataforma.
// Suspender una empresa cierra todas sus sesiones.
func (s *Service) UpdateTenant(ctx context.Context, actor uuid.UUID, id uuid.UUID, in TenantUpdate) (Tenant, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Tenant, error) {
		repo := NewRepository(q)
		current, err := repo.GetForUpdate(ctx, id)
		if err != nil {
			return Tenant{}, err
		}
		if current == nil {
			return Tenant{}, ErrTenantNotFound
		}
		next, err := applyTenantUpdate(*current, in)
		if err != nil {
			return Tenant{}, err
		}
		if in.Features != nil && s.billing != nil {
			if err := s.billing.CheckFeatures(ctx, q, id, next.Features); err != nil {
				return Tenant{}, err
			}
		}
		if err := repo.Update(ctx, next); err != nil {
			return Tenant{}, err
		}
		action := tenantAction(*current, next)
		if action == "tenant.suspended" {
			if err := repo.RevokeSessions(ctx, id); err != nil {
				return Tenant{}, err
			}
		}
		if err := audit.RecordPlatform(ctx, q, audit.PlatformEntry{
			Actor: &actor, TenantID: &id, Action: action, Entity: "tenant", EntityID: &id,
			Metadata: map[string]any{
				"before": map[string]any{"name": current.Name, "is_active": current.IsActive, "features": normalizedFeatures(current.Features)},
				"after":  map[string]any{"name": next.Name, "is_active": next.IsActive, "features": next.Features},
			},
		}); err != nil {
			return Tenant{}, err
		}
		updated, err := repo.GetByID(ctx, id)
		if err != nil {
			return Tenant{}, err
		}
		return *updated, nil
	})
}

func applyTenantUpdate(t Tenant, in TenantUpdate) (Tenant, error) {
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if n := utf8.RuneCountInString(name); n < 1 || n > 255 {
			return Tenant{}, ErrInvalidName.WithField("body.name")
		}
		t.Name = name
	}
	if in.IsActive != nil {
		t.IsActive = *in.IsActive
	}
	if in.Features != nil {
		t.Features = normalizedFeatures(in.Features)
	}
	return t, nil
}

func tenantAction(before, after Tenant) string {
	switch {
	case before.IsActive && !after.IsActive:
		return "tenant.suspended"
	case !before.IsActive && after.IsActive:
		return "tenant.reactivated"
	default:
		return "tenant.updated"
	}
}

// PlatformAudit devuelve la bitacora de la plataforma (W-P4).
func (s *Service) PlatformAudit(ctx context.Context, f audit.PlatformFilter) ([]audit.PlatformRecord, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]audit.PlatformRecord, error) {
		return audit.ListPlatform(ctx, q, f)
	})
}

// --- Configuracion de la empresa (W-O1) -------------------------------------

// Settings devuelve la configuracion de la empresa.
func (s *Service) Settings(ctx context.Context, id uuid.UUID) (SettingsRead, error) {
	t, err := s.GetTenant(ctx, id)
	if err != nil {
		return SettingsRead{}, err
	}
	return s.settingsRead(ctx, t), nil
}

// UpdateSettings guarda los cambios de la empresa. El logo llega como archivo
// ya subido (module tenant, kind logo); el anterior queda para borrarse.
func (s *Service) UpdateSettings(ctx context.Context, actor uuid.UUID, id uuid.UUID, in SettingsUpdate) (SettingsRead, error) {
	t, err := database.InTxValue(ctx, s.db, func(q database.Querier) (Tenant, error) {
		repo := NewRepository(q)
		current, err := repo.GetForUpdate(ctx, id)
		if err != nil {
			return Tenant{}, err
		}
		if current == nil {
			return Tenant{}, ErrTenantNotFound
		}
		before := s.settingsRead(ctx, *current)
		next, err := applySettings(*current, in)
		if err != nil {
			return Tenant{}, err
		}
		if err := s.applyLogo(ctx, repo, current, &next, in); err != nil {
			return Tenant{}, err
		}
		if err := repo.SaveSettings(ctx, next); err != nil {
			return Tenant{}, err
		}
		after := s.settingsRead(ctx, next)
		before.LogoURL, after.LogoURL = nil, nil
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor, Action: "tenant.settings_updated", Entity: "tenant", EntityID: &id,
			Before: before, After: after,
		}); err != nil {
			return Tenant{}, err
		}
		return next, nil
	})
	if err != nil {
		return SettingsRead{}, err
	}
	return s.settingsRead(ctx, t), nil
}

func (s *Service) applyLogo(ctx context.Context, repo *Repository, current, next *Tenant, in SettingsUpdate) error {
	switch {
	case in.LogoFileID != nil:
		f, err := readyLogo(ctx, repo, current.ID, *in.LogoFileID)
		if err != nil {
			return err
		}
		next.LogoKey = &f.S3Key
	case in.RemoveLogo:
		next.LogoKey = nil
	default:
		return nil
	}
	if current.LogoKey != nil && (next.LogoKey == nil || *next.LogoKey != *current.LogoKey) {
		return repo.DiscardFileByKey(ctx, *current.LogoKey)
	}
	return nil
}

// readyLogo valida que el archivo sea un logo subido de esta empresa y lo
// asocia a ella.
func readyLogo(ctx context.Context, repo *Repository, tenantID, fileID uuid.UUID) (*LogoFile, error) {
	f, err := repo.FileByID(ctx, fileID)
	if err != nil {
		return nil, err
	}
	if f == nil || f.Status != "ready" || f.Module != "tenant" || f.Kind != "logo" ||
		storage.AssertTenantKey(f.S3Key, tenantID) != nil {
		return nil, ErrInvalidLogoFile.WithField("body.logo_file_id")
	}
	return f, repo.AttachFile(ctx, f.ID, "tenant", tenantID)
}

func applySettings(t Tenant, in SettingsUpdate) (Tenant, error) {
	settings := make(map[string]any, len(t.Settings)+6)
	for k, v := range t.Settings {
		settings[k] = v
	}
	t, err := applyIdentity(t, in)
	if err != nil {
		return Tenant{}, err
	}
	if err := applyHours(settings, in); err != nil {
		return Tenant{}, err
	}
	if err := applyTexts(settings, in); err != nil {
		return Tenant{}, err
	}
	t.Settings = settings
	return t, nil
}

// applyIdentity aplica nombre, zona horaria y moneda.
func applyIdentity(t Tenant, in SettingsUpdate) (Tenant, error) {
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if n := utf8.RuneCountInString(name); n < 1 || n > 255 {
			return Tenant{}, ErrInvalidName.WithField("body.name")
		}
		t.Name = name
	}
	if in.Timezone != nil {
		if _, err := time.LoadLocation(*in.Timezone); err != nil || *in.Timezone == "" {
			return Tenant{}, ErrInvalidTimezone.WithField("body.timezone")
		}
		t.Timezone = *in.Timezone
	}
	if in.Currency != nil {
		c := strings.ToUpper(strings.TrimSpace(*in.Currency))
		if c != "GTQ" && c != "USD" {
			return Tenant{}, ErrInvalidCurrency.WithField("body.currency")
		}
		t.Currency = c
	}
	return t, nil
}

// applyHours aplica las horas de entrada y salida y la alarma de salida.
func applyHours(settings map[string]any, in SettingsUpdate) error {
	for _, f := range []struct {
		v          *string
		key, field string
	}{{in.CheckinTime, "checkin_time", "body.checkin_time"}, {in.CheckoutTime, "checkout_time", "body.checkout_time"}} {
		if f.v == nil {
			continue
		}
		if !hhmm.MatchString(*f.v) {
			return ErrInvalidTime.WithField(f.field)
		}
		settings[f.key] = *f.v
	}
	if in.CheckoutAlarmMinutes != nil {
		if m := *in.CheckoutAlarmMinutes; m < 0 || m > 240 {
			return ErrInvalidAlarm.WithField("body.checkout_alarm_minutes")
		}
		settings["checkout_alarm_minutes"] = *in.CheckoutAlarmMinutes
	}
	return nil
}

// applyTexts aplica los datos fiscales y las instrucciones para huespedes.
func applyTexts(settings map[string]any, in SettingsUpdate) error {
	for _, f := range []struct {
		v          *string
		key, field string
		max        int
	}{
		{in.LegalName, "legal_name", "body.legal_name", 255},
		{in.NIT, "nit", "body.nit", 32},
		{in.GuestInstructions, "guest_instructions", "body.guest_instructions", 2000},
	} {
		if f.v == nil {
			continue
		}
		v := strings.TrimSpace(*f.v)
		if utf8.RuneCountInString(v) > f.max {
			return ErrInvalidText.WithField(f.field)
		}
		settings[f.key] = v
	}
	return nil
}

func (s *Service) settingsRead(ctx context.Context, t Tenant) SettingsRead {
	str := func(key, fallback string) string {
		if v, ok := t.Settings[key].(string); ok && v != "" {
			return v
		}
		return fallback
	}
	alarm := defaultAlarm
	if v, ok := t.Settings["checkout_alarm_minutes"].(float64); ok {
		alarm = int(v)
	} else if v, ok := t.Settings["checkout_alarm_minutes"].(int); ok {
		alarm = v
	}
	return SettingsRead{
		Name: t.Name, LegalName: str("legal_name", ""), NIT: str("nit", ""),
		LogoURL:  s.storage.LogoURL(ctx, t.ID, t.LogoKey),
		Timezone: t.Timezone, Currency: strings.TrimSpace(t.Currency),
		CheckinTime: str("checkin_time", defaultCheckin), CheckoutTime: str("checkout_time", defaultCheckout),
		CheckoutAlarmMinutes: alarm, GuestInstructions: str("guest_instructions", ""),
		Features: normalizedFeatures(t.Features),
	}
}
