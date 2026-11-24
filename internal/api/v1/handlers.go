package v1

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/modules/auth"
)

// Envoltorios de handlers: resuelven el principal y traducen los errores de
// dominio a HTTP en un solo lugar, asi cada endpoint queda en una linea.

// errOutOfScope es la respuesta a un recurso de otra empresa: 404, nunca 403.
var errOutOfScope = apperr.New(apperr.KindNotFound, "")

// httpError traduce un error de dominio (los de huma pasan tal cual).
func httpError(ctx context.Context, err error) error {
	var statusErr huma.StatusError
	if errors.As(err, &statusErr) {
		return err
	}
	return api.HTTPError(ctx, err)
}

// authed es un handler que necesita al usuario autenticado.
func authed[I, O any](fn func(context.Context, auth.Principal, *I) (*O, error)) func(context.Context, *I) (*O, error) {
	return func(ctx context.Context, in *I) (*O, error) {
		p, err := api.MustPrincipal(ctx)
		if err != nil {
			return nil, err
		}
		out, err := fn(ctx, p, in)
		if err != nil {
			return nil, httpError(ctx, err)
		}
		return out, nil
	}
}

// public es un handler sin sesion.
func public[I, O any](fn func(context.Context, *I) (*O, error)) func(context.Context, *I) (*O, error) {
	return func(ctx context.Context, in *I) (*O, error) {
		out, err := fn(ctx, in)
		if err != nil {
			return nil, httpError(ctx, err)
		}
		return out, nil
	}
}

// tenantScoped lo cumplen las entradas con {tenant_id} en la ruta.
type tenantScoped interface {
	tenant() uuid.UUID
}

// scoped es un handler de /tenants/{tenant_id}/...: exige que la ruta sea la
// empresa activa de la sesion (si no, 404).
func scoped[I any, PI interface {
	*I
	tenantScoped
}, O any](fn func(context.Context, auth.Principal, PI) (*O, error)) func(context.Context, PI) (*O, error) {
	return func(ctx context.Context, in PI) (*O, error) {
		p, err := api.MustPrincipal(ctx)
		if err != nil {
			return nil, err
		}
		if p.Membership == nil || p.Membership.TenantID != in.tenant() {
			return nil, api.HTTPError(ctx, errOutOfScope)
		}
		out, err := fn(ctx, p, in)
		if err != nil {
			return nil, httpError(ctx, err)
		}
		return out, nil
	}
}

// noContent es la salida de los 204.
func noContent(err error) (*struct{}, error) {
	return nil, err
}
