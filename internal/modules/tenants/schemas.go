package tenants

import (
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/modules/authz"
)

const (
	slugMinLength = 2
	// PostgreSQL limita los identificadores a 63 bytes y el schema se llama
	// SchemaPrefix + slug.
	slugMaxLength = 63 - len(SchemaPrefix)
)

// El slug se usa para construir un identificador de schema en SQL; se
// restringe a [a-z0-9_] para impedir inyeccion y nombres invalidos.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_]*[a-z0-9]$`)

// TenantCreate son los datos para crear un tenant (W-P1).
type TenantCreate struct {
	Name     string          `json:"name" minLength:"1" maxLength:"255"`
	Slug     string          `json:"slug" minLength:"2" maxLength:"63" doc:"Minusculas, numeros y guiones bajos (2 a 56 caracteres). Se normaliza a minusculas y sin espacios."`
	Features map[string]bool `json:"features,omitempty" doc:"Banderas: reservations, parking, shared_areas, guest_chat, payroll, smart_locks."`
	Timezone string          `json:"timezone,omitempty" maxLength:"64" doc:"Zona horaria IANA; por defecto America/Guatemala."`
	// AdminEmail es la persona que administrara la empresa: si ya tiene cuenta
	// queda como administradora; si no, recibe una invitacion por correo.
	AdminEmail string `json:"admin_email,omitempty" maxLength:"254"`
	// Fase 6: plan y dias de la prueba con que empieza.
	PlanKey   string `json:"plan_key,omitempty" maxLength:"32" doc:"Plan de la prueba; por defecto completo."`
	TrialDays int    `json:"trial_days,omitempty" minimum:"0" maximum:"90" doc:"Días de prueba; por defecto 14."`
}

// TenantUpdate son los cambios que hace la plataforma (PATCH /platform/tenants/{id}).
type TenantUpdate struct {
	_        struct{}        `additionalProperties:"true"`
	Name     *string         `json:"name,omitempty" maxLength:"255"`
	IsActive *bool           `json:"is_active,omitempty" doc:"false suspende la empresa: nadie puede entrar y se cierran sus sesiones."`
	Features map[string]bool `json:"features,omitempty"`
}

// normalizedFeatures conserva solo las banderas conocidas, todas presentes.
func normalizedFeatures(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(authz.Features))
	for _, f := range authz.Features {
		out[string(f)] = in[string(f)]
	}
	return out
}

// NormalizeSlug pasa el slug a minusculas, quita espacios y valida su forma.
func NormalizeSlug(raw string) (string, error) {
	slug := strings.ToLower(strings.TrimSpace(raw))
	if len(slug) < slugMinLength || len(slug) > slugMaxLength || !slugPattern.MatchString(slug) {
		return "", ErrInvalidSlug
	}
	return slug, nil
}

// TenantRead es la representacion publica de un tenant.
type TenantRead struct {
	Name       string    `json:"name"`
	Slug       string    `json:"slug"`
	ID         uuid.UUID `json:"id" format:"uuid"`
	SchemaName string    `json:"schema_name"`
	IsActive   bool      `json:"is_active"`
	Timezone   string    `json:"timezone"`
	Currency   string    `json:"currency"`
	LogoURL    *string   `json:"logo_url" nullable:"true"`
	// Features son las banderas de funcionalidad del tenant.
	Features      map[string]bool `json:"features"`
	Members       int             `json:"members" doc:"Personas con membresia (activas o suspendidas)."`
	ActiveMembers int             `json:"active_members"`
	LastActivity  *time.Time      `json:"last_activity" nullable:"true" doc:"Ultimo uso de una sesion en la empresa."`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// NewTenantRead construye la representacion publica de t.
func NewTenantRead(t Tenant) TenantRead {
	return TenantRead{
		Name:       t.Name,
		Slug:       t.Slug,
		ID:         t.ID,
		SchemaName: t.SchemaName,
		IsActive:   t.IsActive,
		Timezone:   t.Timezone,
		Currency:   strings.TrimSpace(t.Currency),
		Features:   normalizedFeatures(t.Features),
		CreatedAt:  t.CreatedAt.UTC(),
		UpdatedAt:  t.UpdatedAt.UTC(),
	}
}

// SettingsRead es la configuracion de la empresa (W-O1).
type SettingsRead struct {
	Name                 string          `json:"name"`
	LegalName            string          `json:"legal_name"`
	NIT                  string          `json:"nit"`
	LogoURL              *string         `json:"logo_url" nullable:"true"`
	Timezone             string          `json:"timezone"`
	Currency             string          `json:"currency"`
	CheckinTime          string          `json:"checkin_time" doc:"HH:MM"`
	CheckoutTime         string          `json:"checkout_time" doc:"HH:MM"`
	CheckoutAlarmMinutes int             `json:"checkout_alarm_minutes"`
	GuestInstructions    string          `json:"guest_instructions"`
	Features             map[string]bool `json:"features" doc:"Solo lectura: las define el plan contratado."`
}

// SettingsUpdate son los cambios que hace la empresa (campos omitidos no
// cambian).
type SettingsUpdate struct {
	_                    struct{}   `additionalProperties:"true"`
	Name                 *string    `json:"name,omitempty" maxLength:"255"`
	LegalName            *string    `json:"legal_name,omitempty" maxLength:"255"`
	NIT                  *string    `json:"nit,omitempty" maxLength:"32"`
	Timezone             *string    `json:"timezone,omitempty" maxLength:"64"`
	Currency             *string    `json:"currency,omitempty" maxLength:"3"`
	CheckinTime          *string    `json:"checkin_time,omitempty" maxLength:"5"`
	CheckoutTime         *string    `json:"checkout_time,omitempty" maxLength:"5"`
	CheckoutAlarmMinutes *int       `json:"checkout_alarm_minutes,omitempty"`
	GuestInstructions    *string    `json:"guest_instructions,omitempty" maxLength:"2000"`
	LogoFileID           *uuid.UUID `json:"logo_file_id,omitempty" format:"uuid" doc:"Archivo subido con /files/presign-upload (module tenant, kind logo)."`
	RemoveLogo           bool       `json:"remove_logo,omitempty"`
}
