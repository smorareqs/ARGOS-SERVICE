package auth

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Contratos de cuenta, sesiones, contrasenas, 2FA e invitaciones
// (fase01.md §3.7.1).

// SessionRead es una sesion abierta en GET /auth/sessions.
type SessionRead struct {
	ID         uuid.UUID  `json:"id" format:"uuid"`
	DeviceName *string    `json:"device_name" nullable:"true"`
	Platform   Platform   `json:"platform" enum:"android,ios,web"`
	IP         *string    `json:"ip" nullable:"true"`
	TenantName *string    `json:"tenant_name" nullable:"true" doc:"Empresa activa de la sesion; null en modo plataforma."`
	LastUsedAt *time.Time `json:"last_used_at" nullable:"true"`
	CreatedAt  time.Time  `json:"created_at"`
	Current    bool       `json:"current" doc:"true para la sesion desde la que se consulta."`
}

// ResetPasswordRequest es el cuerpo de POST /auth/password/reset.
type ResetPasswordRequest struct {
	_           struct{} `additionalProperties:"true"`
	Token       string   `json:"token" minLength:"1" maxLength:"128"`
	NewPassword string   `json:"new_password" minLength:"1" maxLength:"128"`
}

// ChangePasswordRequest es el cuerpo de POST /auth/password/change.
type ChangePasswordRequest struct {
	_               struct{} `additionalProperties:"true"`
	CurrentPassword string   `json:"current_password" minLength:"1" maxLength:"128"`
	NewPassword     string   `json:"new_password" minLength:"1" maxLength:"128"`
}

// TOTPSetupResponse es la respuesta de POST /auth/totp/setup.
type TOTPSetupResponse struct {
	Secret     string `json:"secret" doc:"Clave en base32 para escribirla a mano en la app autenticadora."`
	OTPAuthURL string `json:"otpauth_url" doc:"Enlace otpauth:// para mostrar como QR."`
}

// TOTPCodeRequest es el cuerpo de POST /auth/totp/confirm.
type TOTPCodeRequest struct {
	_    struct{} `additionalProperties:"true"`
	Code string   `json:"code" minLength:"1" maxLength:"10"`
}

// TOTPDisableRequest es el cuerpo de POST /auth/totp/disable.
type TOTPDisableRequest struct {
	_        struct{} `additionalProperties:"true"`
	Password string   `json:"password" minLength:"1" maxLength:"128"`
	Code     string   `json:"code" minLength:"1" maxLength:"10"`
}

// ProfileUpdate es el cuerpo de PATCH /auth/me.
type ProfileUpdate struct {
	_        struct{} `additionalProperties:"true"`
	FullName *string  `json:"full_name,omitempty" maxLength:"255"`
	Phone    *string  `json:"phone,omitempty" maxLength:"32" doc:"Cadena vacia para quitarlo."`
}

// InvitationCreate es el cuerpo de POST /tenants/{id}/invitations.
type InvitationCreate struct {
	_           struct{}      `additionalProperties:"true"`
	Email       string        `json:"email" minLength:"3" maxLength:"254"`
	RoleKey     authz.RoleKey `json:"role_key" minLength:"1" maxLength:"64"`
	PropertyIDs []uuid.UUID   `json:"property_ids,omitempty" maxItems:"500" doc:"Propiedades que vera la persona (se aplican al aceptar; fase 2)."`
	Message     *string       `json:"message,omitempty" maxLength:"500" nullable:"true"`
}

// InvitationRead es una invitacion en la administracion del equipo.
type InvitationRead struct {
	ID          uuid.UUID     `json:"id" format:"uuid"`
	Email       string        `json:"email"`
	RoleKey     authz.RoleKey `json:"role_key"`
	RoleName    string        `json:"role_name"`
	PropertyIDs []string      `json:"property_ids"`
	InvitedBy   string        `json:"invited_by" doc:"Nombre de quien invito."`
	Status      string        `json:"status" enum:"pending,accepted,expired,revoked"`
	Message     *string       `json:"message" nullable:"true"`
	CreatedAt   time.Time     `json:"created_at"`
	ExpiresAt   *time.Time    `json:"expires_at" nullable:"true"`
}

// InvitationPreview es lo que ve quien abre el enlace (GET /auth/invitations/{token}).
type InvitationPreview struct {
	TenantName    string    `json:"tenant_name"`
	TenantLogoURL *string   `json:"tenant_logo_url" nullable:"true"`
	RoleName      string    `json:"role_name"`
	Email         string    `json:"email"`
	InviterName   *string   `json:"inviter_name" nullable:"true"`
	UserExists    bool      `json:"user_exists" doc:"true si ya tiene cuenta: entra con su contrasena actual."`
	ExpiresAt     time.Time `json:"expires_at"`
}

// AcceptInvitationRequest es el cuerpo de POST /auth/invitations/accept.
type AcceptInvitationRequest struct {
	_        struct{} `additionalProperties:"true"`
	Token    string   `json:"token" minLength:"1" maxLength:"128"`
	Password string   `json:"password" minLength:"1" maxLength:"128" doc:"La actual si user_exists; la nueva si no."`
	FullName string   `json:"full_name,omitempty" maxLength:"255" doc:"Solo si la persona no tiene cuenta."`
	TOTPCode string   `json:"totp_code,omitempty" maxLength:"10"`
	Device   Device   `json:"device"`
}

func newInvitationRead(i Invitation) InvitationRead {
	out := InvitationRead{
		ID: i.ID, Email: i.Email, RoleKey: i.RoleKey, RoleName: i.RoleName, Status: i.Status,
		CreatedAt: i.CreatedAt.UTC(), ExpiresAt: i.ExpiresAt, PropertyIDs: []string{},
		InvitedBy: "ARGOS",
	}
	if i.InviterName != nil {
		out.InvitedBy = *i.InviterName
	}
	if i.Status == InvitationPending && i.ExpiresAt != nil && !i.ExpiresAt.After(time.Now()) {
		out.Status = InvitationExpired
	}
	if msg, ok := i.Payload["message"].(string); ok && msg != "" {
		out.Message = &msg
	}
	if ids, ok := i.Payload["property_ids"].([]any); ok {
		for _, id := range ids {
			if s, ok := id.(string); ok {
				out.PropertyIDs = append(out.PropertyIDs, s)
			}
		}
	}
	return out
}

func validateFullName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 255 {
		return "", ErrInvalidFullName.WithField("body.full_name")
	}
	return name, nil
}
