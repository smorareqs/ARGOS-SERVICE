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

// TenantCreate son los datos para crear un tenant.
type TenantCreate struct {
	Name     string          `json:"name" minLength:"1" maxLength:"255"`
	Slug     string          `json:"slug" minLength:"2" maxLength:"63" doc:"Minusculas, numeros y guiones bajos (2 a 56 caracteres). Se normaliza a minusculas y sin espacios."`
	Features map[string]bool `json:"features,omitempty" doc:"Banderas: reservations, parking, shared_areas, guest_chat, payroll, smart_locks."`
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
	// Features son las banderas de funcionalidad del tenant.
	Features  map[string]bool `json:"features"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
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
