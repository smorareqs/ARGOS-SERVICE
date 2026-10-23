package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/auth"
)

// loginForm es el formulario del flujo OAuth2 password. `username`
// representa el correo del usuario.
type loginForm struct {
	_            struct{} `additionalProperties:"true"`
	GrantType    string   `json:"grant_type,omitempty" pattern:"^password$"`
	Username     string   `json:"username" minLength:"1" doc:"Correo del usuario."`
	Password     string   `json:"password" minLength:"1"`
	Scope        string   `json:"scope,omitempty"`
	ClientID     string   `json:"client_id,omitempty"`
	ClientSecret string   `json:"client_secret,omitempty"`
}

type loginInput struct {
	Body loginForm `contentType:"application/x-www-form-urlencoded"`
}

type tokenOutput struct {
	Body auth.Token
}

type registerInput struct {
	Body auth.UserCreate
}

type userOutput struct {
	Body auth.UserRead
}

func registerAuth(v1 huma.API, svc *auth.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1, "/auth")

	huma.Register(grp, huma.Operation{
		OperationID: "login",
		Method:      http.MethodPost,
		Path:        "/login",
		Summary:     "Iniciar sesion",
		Tags:        []string{"auth"},
	}, func(ctx context.Context, in *loginInput) (*tokenOutput, error) {
		token, err := svc.Login(ctx, in.Body.Username, in.Body.Password)
		if err != nil {
			return nil, api.HTTPError(ctx, err)
		}
		return &tokenOutput{Body: token}, nil
	})

	huma.Register(grp, huma.Operation{
		OperationID:   "register",
		Method:        http.MethodPost,
		Path:          "/register",
		DefaultStatus: http.StatusCreated,
		Summary:       "Registrar usuario",
		Description:   "Solo platform_admin. Crea un usuario con el rol y el tenant indicados.",
		Tags:          []string{"auth"},
		Security:      api.Secured,
		Middlewares:   guard.RequireRole(auth.RolePlatformAdmin),
	}, func(ctx context.Context, in *registerInput) (*userOutput, error) {
		user, err := svc.Register(ctx, in.Body)
		if err != nil {
			return nil, api.HTTPError(ctx, err)
		}
		return &userOutput{Body: auth.NewUserRead(user)}, nil
	})

	huma.Register(grp, huma.Operation{
		OperationID: "me",
		Method:      http.MethodGet,
		Path:        "/me",
		Summary:     "Perfil del usuario actual",
		Tags:        []string{"auth"},
		Security:    api.Secured,
		Middlewares: guard.Authenticated(),
	}, func(ctx context.Context, _ *struct{}) (*userOutput, error) {
		user, ok := api.CurrentUser(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("No autenticado.")
		}
		return &userOutput{Body: auth.NewUserRead(user)}, nil
	})
}
