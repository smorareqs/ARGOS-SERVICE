package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/auth"
)

type loginInput struct {
	Body auth.LoginRequest
}

type selectTenantInput struct {
	Body auth.SelectTenantRequest
}

type sessionOutput struct {
	Body auth.SessionResponse
}

type refreshInput struct {
	Body auth.RefreshRequest
}

type refreshOutput struct {
	Body auth.RefreshResponse
}

type logoutInput struct {
	Body auth.LogoutRequest
}

type forgotInput struct {
	Body auth.ForgotPasswordRequest
}

type meOutput struct {
	Body auth.MeResponse
}

// Contrato: APP/docs/plans/fase01.md §3.7.1.
func registerAuth(v1 huma.API, svc *auth.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1, "/auth")
	grp.UseSimpleModifier(func(op *huma.Operation) { op.Tags = []string{"auth"} })

	huma.Register(grp, huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        "/login",
		Summary:     "Iniciar sesion",
		Description: "Con una sola empresa activa devuelve los tokens; con varias, `select_tenant` y un " +
			"`selection_token` de 5 minutos. 428 si falta el codigo de verificacion; 429 con `Retry-After` " +
			"tras 10 intentos fallidos en 15 minutos.",
		Errors: []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusPreconditionRequired, http.StatusTooManyRequests},
	}, func(ctx context.Context, in *loginInput) (*sessionOutput, error) {
		out, err := svc.Login(ctx, in.Body, api.ClientIP(ctx))
		if err != nil {
			return nil, api.HTTPError(ctx, err)
		}
		return &sessionOutput{Body: out}, nil
	})

	huma.Register(grp, huma.Operation{
		OperationID: "select-tenant",
		Method:      http.MethodPost,
		Path:        "/select-tenant",
		Summary:     "Elegir o cambiar de empresa",
		Description: "Tras el login: `selection_token` + `device`. Con sesion abierta: `refresh_token`.",
		Errors:      []int{http.StatusUnauthorized, http.StatusNotFound},
	}, func(ctx context.Context, in *selectTenantInput) (*sessionOutput, error) {
		out, err := svc.SelectTenant(ctx, in.Body, api.ClientIP(ctx))
		if err != nil {
			return nil, api.HTTPError(ctx, err)
		}
		return &sessionOutput{Body: out}, nil
	})

	huma.Register(grp, huma.Operation{
		OperationID: "refresh",
		Method:      http.MethodPost,
		Path:        "/refresh",
		Summary:     "Renovar tokens",
		Description: "Rota el refresh token. Presentar uno ya rotado revoca toda la sesion.",
		Errors:      []int{http.StatusUnauthorized},
	}, func(ctx context.Context, in *refreshInput) (*refreshOutput, error) {
		out, err := svc.Refresh(ctx, in.Body, api.ClientIP(ctx))
		if err != nil {
			return nil, api.HTTPError(ctx, err)
		}
		return &refreshOutput{Body: out}, nil
	})

	huma.Register(grp, huma.Operation{
		OperationID:   "logout",
		Method:        http.MethodPost,
		Path:          "/logout",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Cerrar sesion",
	}, func(ctx context.Context, in *logoutInput) (*struct{}, error) {
		if err := svc.Logout(ctx, in.Body.RefreshToken); err != nil {
			return nil, api.HTTPError(ctx, err)
		}
		return nil, nil
	})

	huma.Register(grp, huma.Operation{
		OperationID:   "forgot-password",
		Method:        http.MethodPost,
		Path:          "/password/forgot",
		DefaultStatus: http.StatusAccepted,
		Summary:       "Recuperar contrasena",
		Description:   "Responde 202 siempre, exista o no el correo.",
	}, func(ctx context.Context, in *forgotInput) (*struct{}, error) {
		svc.ForgotPassword(ctx, in.Body.Email)
		return nil, nil
	})

	huma.Register(grp, huma.Operation{
		OperationID: "me",
		Method:      http.MethodGet,
		Path:        "/me",
		Summary:     "Usuario, empresa activa, rol y permisos",
		Security:    api.Secured,
		Middlewares: guard.Authenticated(),
	}, func(ctx context.Context, _ *struct{}) (*meOutput, error) {
		p, ok := api.CurrentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("No autenticado.")
		}
		out, err := svc.Me(ctx, p)
		if err != nil {
			return nil, api.HTTPError(ctx, err)
		}
		return &meOutput{Body: out}, nil
	})
}
