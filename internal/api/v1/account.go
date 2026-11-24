package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/auth"
)

// page es la forma de las listas paginadas por cursor (fase02.md §4). En la
// fase 1 las listas caben en una pagina: next_cursor siempre es null.
type page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor" nullable:"true"`
}

func pageOf[T any](items []T) page[T] {
	if items == nil {
		items = []T{}
	}
	return page[T]{Items: items}
}

type sessionsOutput struct {
	Body []auth.SessionRead
}

type sessionIDInput struct {
	SessionID uuid.UUID `path:"session_id" format:"uuid"`
}

type resetPasswordInput struct {
	Body auth.ResetPasswordRequest
}

type changePasswordInput struct {
	Body auth.ChangePasswordRequest
}

type totpSetupOutput struct {
	Body auth.TOTPSetupResponse
}

type totpCodeInput struct {
	Body auth.TOTPCodeRequest
}

type totpDisableInput struct {
	Body auth.TOTPDisableRequest
}

type profileInput struct {
	Body auth.ProfileUpdate
}

type profileOutput struct {
	Body auth.MeUser
}

type invitationTokenInput struct {
	Token string `path:"token" maxLength:"128"`
}

type invitationPreviewOutput struct {
	Body auth.InvitationPreview
}

type acceptInvitationInput struct {
	Body auth.AcceptInvitationRequest
}

type none struct{}

// Cuenta propia: sesiones, contrasena, 2FA, perfil e invitaciones
// (fase01.md §3.7 y §3.7.1).
func registerAccount(v1 huma.API, svc *auth.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1, "/auth")
	grp.UseSimpleModifier(func(op *huma.Operation) { op.Tags = []string{"auth"} })
	secured := func(op huma.Operation) huma.Operation {
		op.Security = api.Secured
		op.Middlewares = guard.Authenticated()
		return op
	}
	registerSessions(grp, svc, secured)
	registerPasswords(grp, svc, secured)
	registerTOTP(grp, svc, secured)
	registerInvitationLinks(grp, svc)
}

type securer func(huma.Operation) huma.Operation

func registerSessions(grp huma.API, svc *auth.Service, secured securer) {
	huma.Register(grp, secured(huma.Operation{
		OperationID: "list-sessions", Method: http.MethodGet, Path: "/sessions",
		Summary: "Dispositivos con sesion abierta",
	}), authed(func(ctx context.Context, p auth.Principal, _ *none) (*sessionsOutput, error) {
		list, err := svc.Sessions(ctx, p)
		return &sessionsOutput{Body: list}, err
	}))

	huma.Register(grp, secured(huma.Operation{
		OperationID: "revoke-session", Method: http.MethodDelete, Path: "/sessions/{session_id}",
		DefaultStatus: http.StatusNoContent, Summary: "Cerrar una sesion propia",
		Errors: []int{http.StatusNotFound},
	}), authed(func(ctx context.Context, p auth.Principal, in *sessionIDInput) (*struct{}, error) {
		return noContent(svc.RevokeSession(ctx, p, in.SessionID))
	}))

	huma.Register(grp, secured(huma.Operation{
		OperationID: "revoke-other-sessions", Method: http.MethodDelete, Path: "/sessions",
		DefaultStatus: http.StatusNoContent, Summary: "Cerrar todas las sesiones menos la actual",
	}), authed(func(ctx context.Context, p auth.Principal, _ *none) (*struct{}, error) {
		return noContent(svc.RevokeOtherSessions(ctx, p))
	}))

	huma.Register(grp, secured(huma.Operation{
		OperationID: "update-me", Method: http.MethodPatch, Path: "/me",
		Summary: "Cambiar nombre o telefono", Errors: []int{http.StatusUnprocessableEntity},
	}), authed(func(ctx context.Context, p auth.Principal, in *profileInput) (*profileOutput, error) {
		out, err := svc.UpdateProfile(ctx, p, in.Body)
		return &profileOutput{Body: out}, err
	}))
}

func registerPasswords(grp huma.API, svc *auth.Service, secured securer) {
	huma.Register(grp, huma.Operation{
		OperationID: "reset-password", Method: http.MethodPost, Path: "/password/reset",
		DefaultStatus: http.StatusNoContent, Summary: "Restablecer contrasena con el enlace del correo",
		Description: "Cierra todas las sesiones de la persona. 404 si el enlace vencio o ya se uso.",
		Errors:      []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, public(func(ctx context.Context, in *resetPasswordInput) (*struct{}, error) {
		return noContent(svc.ResetPassword(ctx, in.Body))
	}))

	huma.Register(grp, secured(huma.Operation{
		OperationID: "change-password", Method: http.MethodPost, Path: "/password/change",
		DefaultStatus: http.StatusNoContent, Summary: "Cambiar contrasena",
		Description: "Pide la actual y cierra las demas sesiones.",
		Errors:      []int{http.StatusUnprocessableEntity},
	}), authed(func(ctx context.Context, p auth.Principal, in *changePasswordInput) (*struct{}, error) {
		return noContent(svc.ChangePassword(ctx, p, in.Body))
	}))
}

func registerTOTP(grp huma.API, svc *auth.Service, secured securer) {
	huma.Register(grp, secured(huma.Operation{
		OperationID: "totp-setup", Method: http.MethodPost, Path: "/totp/setup",
		Summary:     "Preparar la verificacion en dos pasos",
		Description: "Devuelve la clave y el enlace otpauth:// para el QR. Queda inactiva hasta /totp/confirm.",
		Errors:      []int{http.StatusUnprocessableEntity},
	}), authed(func(ctx context.Context, p auth.Principal, _ *none) (*totpSetupOutput, error) {
		out, err := svc.SetupTOTP(ctx, p)
		return &totpSetupOutput{Body: out}, err
	}))

	huma.Register(grp, secured(huma.Operation{
		OperationID: "totp-confirm", Method: http.MethodPost, Path: "/totp/confirm",
		DefaultStatus: http.StatusNoContent, Summary: "Activar la verificacion en dos pasos",
		Errors: []int{http.StatusUnprocessableEntity},
	}), authed(func(ctx context.Context, p auth.Principal, in *totpCodeInput) (*struct{}, error) {
		return noContent(svc.ConfirmTOTP(ctx, p, in.Body.Code))
	}))

	huma.Register(grp, secured(huma.Operation{
		OperationID: "totp-disable", Method: http.MethodPost, Path: "/totp/disable",
		DefaultStatus: http.StatusNoContent, Summary: "Desactivar la verificacion en dos pasos",
		Errors: []int{http.StatusUnprocessableEntity},
	}), authed(func(ctx context.Context, p auth.Principal, in *totpDisableInput) (*struct{}, error) {
		return noContent(svc.DisableTOTP(ctx, p, in.Body))
	}))
}

func registerInvitationLinks(grp huma.API, svc *auth.Service) {
	huma.Register(grp, huma.Operation{
		OperationID: "preview-invitation", Method: http.MethodGet, Path: "/invitations/{token}",
		Summary: "Ver una invitacion", Description: "404 si vencio, ya se uso o fue cancelada.",
		Errors: []int{http.StatusNotFound},
	}, public(func(ctx context.Context, in *invitationTokenInput) (*invitationPreviewOutput, error) {
		out, err := svc.PreviewInvitation(ctx, in.Token)
		return &invitationPreviewOutput{Body: out}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "accept-invitation", Method: http.MethodPost, Path: "/invitations/accept",
		Summary: "Aceptar una invitacion",
		Description: "Crea la cuenta (o valida la existente), activa la membresia y abre la sesion en esa empresa. " +
			"Responde como el login.",
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusPreconditionRequired},
	}, public(func(ctx context.Context, in *acceptInvitationInput) (*sessionOutput, error) {
		out, err := svc.AcceptInvitation(ctx, in.Body, api.ClientIP(ctx))
		return &sessionOutput{Body: out}, err
	}))
}
