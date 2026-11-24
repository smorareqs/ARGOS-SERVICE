package auth

import (
	"net/mail"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Contrato JSON de autenticacion: APP/docs/plans/fase01.md §3.7.1.
//
// Los tags de validacion (minLength, format, enum...) los aplica la capa API
// antes de llegar al servicio y alimentan la documentacion OpenAPI. El
// servicio vuelve a validar con normalized() para que ningun camino (CLI,
// seeders) se salte las reglas.

// Device identifica el dispositivo que abre la sesion. El cliente genera el id
// una vez y lo conserva.
type Device struct {
	_        struct{} `additionalProperties:"true"`
	ID       string   `json:"id" minLength:"1" maxLength:"128"`
	Name     string   `json:"name,omitempty" maxLength:"128"`
	Platform Platform `json:"platform" enum:"android,ios,web"`
}

func (d Device) normalized() (Device, error) {
	d.ID = strings.TrimSpace(d.ID)
	d.Name = strings.TrimSpace(d.Name)
	if d.ID == "" || utf8.RuneCountInString(d.ID) > 128 || utf8.RuneCountInString(d.Name) > 128 || !d.Platform.Valid() {
		return Device{}, ErrInvalidDevice
	}
	return d, nil
}

// LoginRequest es el cuerpo de POST /auth/login.
type LoginRequest struct {
	_        struct{} `additionalProperties:"true"`
	Email    string   `json:"email" minLength:"3" maxLength:"254"`
	Password string   `json:"password" minLength:"1" maxLength:"128"`
	TOTPCode string   `json:"totp_code,omitempty" maxLength:"10" doc:"Codigo de verificacion en dos pasos, si la cuenta lo exige."`
	Device   Device   `json:"device"`
}

// SessionStatus distingue una sesion abierta de una seleccion de empresa
// pendiente.
type SessionStatus string

// Estados de la respuesta de sesion.
const (
	StatusAuthenticated SessionStatus = "authenticated"
	StatusSelectTenant  SessionStatus = "select_tenant"
)

// MembershipRead es una empresa a la que el usuario puede entrar.
type MembershipRead struct {
	TenantID      uuid.UUID     `json:"tenant_id" format:"uuid"`
	TenantName    string        `json:"tenant_name"`
	TenantLogoURL *string       `json:"tenant_logo_url" nullable:"true"`
	RoleKey       authz.RoleKey `json:"role_key"`
	RoleName      string        `json:"role_name"`
}

// NewMembershipRead construye la representacion publica de m.
func NewMembershipRead(m authz.MembershipView, logoURL *string) MembershipRead {
	return MembershipRead{
		TenantID: m.TenantID, TenantName: m.TenantName, TenantLogoURL: logoURL, RoleKey: m.RoleKey, RoleName: m.RoleName,
	}
}

// SessionResponse es la respuesta de login, select-tenant y aceptar invitacion.
// Con status "authenticated" trae los tokens; con "select_tenant" trae el
// selection_token y las empresas para elegir.
type SessionResponse struct {
	Status         SessionStatus    `json:"status" enum:"authenticated,select_tenant"`
	AccessToken    string           `json:"access_token,omitempty"`
	RefreshToken   string           `json:"refresh_token,omitempty"`
	ExpiresIn      int              `json:"expires_in,omitempty" doc:"Segundos de vigencia del access token."`
	SelectionToken string           `json:"selection_token,omitempty"`
	Memberships    []MembershipRead `json:"memberships"`
	// PlatformAdmin indica que la persona tambien puede entrar a la
	// administracion de ARGOS (select-tenant con platform: true).
	PlatformAdmin bool `json:"platform_admin,omitempty"`
}

// SelectTenantRequest es el cuerpo de POST /auth/select-tenant: tras el login
// (selection_token + device) o para cambiar de empresa (refresh_token).
type SelectTenantRequest struct {
	_              struct{}  `additionalProperties:"true"`
	TenantID       uuid.UUID `json:"tenant_id,omitempty" format:"uuid" doc:"Obligatorio salvo con platform."`
	Platform       bool      `json:"platform,omitempty" doc:"true para entrar a la administracion de ARGOS (solo platform_admin)."`
	SelectionToken string    `json:"selection_token,omitempty"`
	Device         *Device   `json:"device,omitempty"`
	RefreshToken   string    `json:"refresh_token,omitempty"`
}

// RefreshRequest es el cuerpo de POST /auth/refresh.
type RefreshRequest struct {
	_            struct{} `additionalProperties:"true"`
	RefreshToken string   `json:"refresh_token" minLength:"1"`
	DeviceID     string   `json:"device_id" minLength:"1" maxLength:"128"`
}

// RefreshResponse son los tokens nuevos tras rotar.
type RefreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// LogoutRequest es el cuerpo de POST /auth/logout.
type LogoutRequest struct {
	_            struct{} `additionalProperties:"true"`
	RefreshToken string   `json:"refresh_token" minLength:"1"`
}

// ForgotPasswordRequest es el cuerpo de POST /auth/password/forgot.
type ForgotPasswordRequest struct {
	_     struct{} `additionalProperties:"true"`
	Email string   `json:"email" minLength:"3" maxLength:"254"`
}

// MeUser es el usuario en GET /auth/me.
type MeUser struct {
	ID              uuid.UUID `json:"id" format:"uuid"`
	Email           string    `json:"email"`
	FullName        string    `json:"full_name"`
	Phone           *string   `json:"phone" nullable:"true"`
	TOTPEnabled     bool      `json:"totp_enabled"`
	IsPlatformAdmin bool      `json:"is_platform_admin"`
}

// MeTenant es la empresa activa en GET /auth/me.
type MeTenant struct {
	ID       uuid.UUID       `json:"id" format:"uuid"`
	Name     string          `json:"name"`
	LogoURL  *string         `json:"logo_url" nullable:"true"`
	Timezone string          `json:"timezone"`
	Currency string          `json:"currency"`
	Features map[string]bool `json:"features"`
	// Fase 6: estado de la suscripcion y solo lectura.
	BillingStatus string `json:"billing_status" enum:"trialing,active,past_due,suspended,cancelled"`
	ReadOnly      bool   `json:"read_only" doc:"true: la empresa solo puede leer hasta ponerse al día (402 en escrituras)."`
}

// MeRole es el rol en la empresa activa.
type MeRole struct {
	Key  authz.RoleKey `json:"key"`
	Name string        `json:"name"`
}

// MeResponse es la respuesta de GET /auth/me. Tenant y Role son null en modo
// plataforma.
type MeResponse struct {
	User        MeUser             `json:"user"`
	Tenant      *MeTenant          `json:"tenant" doc:"null en modo plataforma."`
	Role        *MeRole            `json:"role" doc:"null en modo plataforma."`
	Permissions []authz.Permission `json:"permissions"`
	Memberships []MembershipRead   `json:"memberships"`
}

// UserCreate son los datos para crear un usuario con contrasena (seeders y
// alta de plataforma).
type UserCreate struct {
	Email           string `json:"email" format:"email" maxLength:"254"`
	FullName        string `json:"full_name" minLength:"1" maxLength:"255"`
	Password        string `json:"password" minLength:"8" maxLength:"128"`
	Phone           string `json:"phone,omitempty" maxLength:"32"`
	IsPlatformAdmin bool   `json:"is_platform_admin,omitempty"`
}

// normalized valida y devuelve una copia con el correo normalizado.
func (u UserCreate) normalized() (UserCreate, error) {
	email, err := normalizeEmail(u.Email)
	if err != nil {
		return UserCreate{}, err
	}
	u.Email = email
	u.FullName = strings.TrimSpace(u.FullName)
	if n := utf8.RuneCountInString(u.FullName); n < 1 || n > 255 {
		return UserCreate{}, ErrInvalidFullName
	}
	if err := validatePassword(u.Password); err != nil {
		return UserCreate{}, err
	}
	u.Phone = strings.TrimSpace(u.Phone)
	if utf8.RuneCountInString(u.Phone) > 32 {
		return UserCreate{}, ErrInvalidPhone
	}
	return u, nil
}

func validatePassword(p string) error {
	if n := utf8.RuneCountInString(p); n < 8 || n > 128 {
		return ErrInvalidPassword
	}
	return nil
}

// normalizeEmail acepta solo direcciones simples (local@dominio.tld, sin
// nombre visible) y pone el dominio en minusculas.
func normalizeEmail(raw string) (string, error) {
	addr, err := mail.ParseAddress(raw)
	if err != nil || addr.Name != "" || addr.Address != raw || len(raw) > 254 {
		return "", ErrInvalidEmail
	}
	at := strings.LastIndexByte(raw, '@')
	local, domain := raw[:at], raw[at+1:]
	if len(local) > 64 || !strings.Contains(domain, ".") ||
		strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", ErrInvalidEmail
	}
	return local + "@" + strings.ToLower(domain), nil
}

// NormalizeEmail valida un correo simple y pone su dominio en minusculas
// (alta de empresa con administrador).
func NormalizeEmail(raw string) (string, error) {
	return normalizeEmail(raw)
}
