package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/ratelimit"
	"github.com/smorareqs/argos-service/internal/modules/apikeys"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/billing"
	"github.com/smorareqs/argos-service/internal/modules/reservations"
)

// Integraciones (fase06.md §4.1): claves API de la empresa y rutas publicas
// (reservas del sitio web del cliente y webhook de la pasarela).

var (
	errKeyScope     = apperr.New(apperr.KindPermissionDenied, "La clave no tiene permiso para crear reservas.")
	errKeyModule    = apperr.New(apperr.KindPermissionDenied, "Las reservas no están activas en esta empresa.")
	errKeyTenant    = apperr.New(apperr.KindAuthentication, "La empresa de esta clave está desactivada.")
	errKeyReadOnly  = apperr.New(apperr.KindPaymentRequired, "La empresa está en solo lectura por un pago pendiente: no recibe reservas nuevas.")
	errKeyRateLimit = apperr.New(apperr.KindTooManyRequests, "Demasiadas reservas seguidas con esta clave. Espera un momento.")
)

// APIKeyParam es el id de la clave en la ruta.
type APIKeyParam struct {
	KeyID uuid.UUID `path:"key_id" format:"uuid"`
}

type (
	apiKeysOutput struct {
		Body page[apikeys.KeyRead]
	}
	apiKeyOutput        struct{ Body apikeys.KeyRead }
	apiKeyCreatedOutput struct{ Body apikeys.KeyCreated }
	apiKeyCreateInput   struct{ Body apikeys.KeyCreate }
	publicReservationIn struct {
		APIKey string `header:"X-Api-Key" required:"true" doc:"Clave API de la empresa (argos_…)."`
		Body   reservations.PublicReservation
	}
	publicReservationOut struct {
		Body reservations.PublicReservationRead
	}
	webhookInput struct {
		Provider  string `path:"provider" maxLength:"32"`
		Signature string `header:"X-Argos-Signature" doc:"sha256=<hex> del cuerpo con el secreto del webhook."`
		RawBody   []byte
	}
)

func registerAPIKeys(v1 huma.API, keys *apikeys.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1, "/api-keys")
	grp.UseMiddleware(guard.RequirePermission(authz.TenantSettings)...)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"integraciones"}
		op.Security = api.Secured
	})
	huma.Register(grp, huma.Operation{
		OperationID: "list-api-keys", Method: http.MethodGet, Path: "", Summary: "Claves API",
		Description: "Sin el secreto: solo el prefijo, el último uso y si está revocada.",
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*apiKeysOutput, error) {
		list, err := keys.List(ctx, p.Membership.TenantID)
		return &apiKeysOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-api-key", Method: http.MethodPost, Path: "", Summary: "Nueva clave API",
		Description: "Devuelve la clave completa una sola vez (`key`).", DefaultStatus: http.StatusCreated,
		Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *apiKeyCreateInput) (*apiKeyCreatedOutput, error) {
		out, err := keys.Create(ctx, p.Actor(), in.Body)
		return &apiKeyCreatedOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "revoke-api-key", Method: http.MethodPost, Path: "/{key_id}/revoke", Summary: "Revocar una clave",
		Description: "Deja de funcionar al momento; el registro queda.", Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *APIKeyParam) (*apiKeyOutput, error) {
		out, err := keys.Revoke(ctx, p.Actor(), in.KeyID)
		return &apiKeyOutput{Body: out}, err
	}))
}

func registerPublic(v1 huma.API, keys *apikeys.Service, res *reservations.Service, bill *billing.Service, perKey *ratelimit.Limiter) {
	grp := huma.NewGroup(v1, "/public")
	grp.UseSimpleModifier(func(op *huma.Operation) { op.Tags = []string{"publico"} })
	huma.Register(grp, huma.Operation{
		OperationID: "public-create-reservation", Method: http.MethodPost, Path: "/reservations", Summary: "Reserva desde el sitio web",
		Description: "Con la cabecera X-Api-Key de la empresa (alcance reservations:write). La propiedad va por su código; " +
			"queda por confirmar (source api) en el canal Directo y avisa por correo a quien administra reservas. " +
			"401 clave inválida, 403 sin alcance o sin el módulo, 402 empresa en solo lectura, 404 propiedad, " +
			"422 fechas o cruce con otra reserva, 429 más de 60 por minuto con la misma clave.",
		Security:      api.APIKeySecured,
		DefaultStatus: http.StatusCreated,
		Errors: []int{
			http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound,
			http.StatusUnprocessableEntity, http.StatusTooManyRequests,
		},
	}, func(ctx context.Context, in *publicReservationIn) (*publicReservationOut, error) {
		out, err := publicReservation(ctx, keys, res, perKey, in)
		if err != nil {
			return nil, httpError(ctx, err)
		}
		return &publicReservationOut{Body: out}, nil
	})
	huma.Register(grp, huma.Operation{
		OperationID: "billing-webhook", Method: http.MethodPost, Path: "/billing/webhook/{provider}", Summary: "Webhook de la pasarela",
		Description:   "Verifica la firma y aplica el pago o el fallo. Idempotente por id de evento.",
		DefaultStatus: http.StatusNoContent, Errors: []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusUnprocessableEntity},
	}, func(ctx context.Context, in *webhookInput) (*struct{}, error) {
		if err := bill.Webhook(ctx, in.Provider, in.Signature, in.RawBody); err != nil {
			return nil, httpError(ctx, err)
		}
		return nil, nil
	})
}

func publicReservation(ctx context.Context, keys *apikeys.Service, res *reservations.Service, perKey *ratelimit.Limiter, in *publicReservationIn) (reservations.PublicReservationRead, error) {
	key, err := keys.Authenticate(ctx, in.APIKey)
	if err != nil {
		return reservations.PublicReservationRead{}, err
	}
	switch {
	case !key.TenantActive:
		return reservations.PublicReservationRead{}, errKeyTenant
	case !key.Can(apikeys.ScopeReservationsWrite):
		return reservations.PublicReservationRead{}, errKeyScope
	case !key.Features[string(authz.FeatureReservations)]:
		return reservations.PublicReservationRead{}, errKeyModule
	case billing.ReadOnly(key.BillingState):
		return reservations.PublicReservationRead{}, errKeyReadOnly
	}
	if ok, wait := perKey.Allow(key.ID.String()); !ok {
		return reservations.PublicReservationRead{}, errKeyRateLimit.WithRetryAfter(wait)
	}
	ctx = database.WithTenantSchema(ctx, key.TenantSchema)
	return res.CreateFromAPI(ctx, key.TenantID, key.Name, in.Body)
}
