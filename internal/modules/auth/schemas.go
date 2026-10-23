package auth

import (
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/security"
)

// Los tags de validacion (minLength, format, enum...) los aplica la capa API
// antes de llegar al servicio y alimentan la documentacion OpenAPI. El
// servicio vuelve a validar con normalized() para que ningun camino (CLI,
// seeders) se salte las reglas.

// UserCreate son los datos para registrar un usuario.
type UserCreate struct {
	Email    string     `json:"email" format:"email" maxLength:"254"`
	FullName string     `json:"full_name" minLength:"1" maxLength:"255"`
	Password string     `json:"password" minLength:"8" maxLength:"128"`
	Role     Role       `json:"role,omitempty" enum:"platform_admin,admin,supervisor,employee" default:"employee"`
	IsOwner  bool       `json:"is_owner,omitempty" default:"false"`
	TenantID *uuid.UUID `json:"tenant_id,omitempty" format:"uuid" nullable:"true"`
}

// normalized valida y devuelve una copia con el correo normalizado (dominio
// en minusculas) y el rol por defecto aplicado.
func (u UserCreate) normalized() (UserCreate, error) {
	email, err := normalizeEmail(u.Email)
	if err != nil {
		return UserCreate{}, err
	}
	u.Email = email
	if n := utf8.RuneCountInString(u.FullName); n < 1 || n > 255 {
		return UserCreate{}, ErrInvalidFullName
	}
	if n := utf8.RuneCountInString(u.Password); n < 8 || n > 128 {
		return UserCreate{}, ErrInvalidPassword
	}
	if u.Role == "" {
		u.Role = RoleEmployee
	}
	if !u.Role.Valid() {
		return UserCreate{}, ErrInvalidRole
	}
	return u, nil
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

// UserRead es la representacion publica de un usuario (nunca expone el hash).
type UserRead struct {
	Email     string     `json:"email"`
	FullName  string     `json:"full_name"`
	ID        uuid.UUID  `json:"id" format:"uuid"`
	Role      Role       `json:"role" enum:"platform_admin,admin,supervisor,employee"`
	IsActive  bool       `json:"is_active"`
	IsOwner   bool       `json:"is_owner"`
	TenantID  *uuid.UUID `json:"tenant_id" format:"uuid" nullable:"true"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// NewUserRead construye la representacion publica de u.
func NewUserRead(u User) UserRead {
	return UserRead{
		Email:     u.Email,
		FullName:  u.FullName,
		ID:        u.ID,
		Role:      u.Role,
		IsActive:  u.IsActive,
		IsOwner:   u.IsOwner,
		TenantID:  u.TenantID,
		CreatedAt: u.CreatedAt.UTC(),
		UpdatedAt: u.UpdatedAt.UTC(),
	}
}

// Token es el par de tokens emitido en el login.
type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type" example:"bearer"`
}

// TokenPayload son los claims de un JWT validado.
type TokenPayload struct {
	Sub      uuid.UUID
	Role     Role
	TenantID *uuid.UUID
	Type     security.TokenType
}
