package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
)

// SecurityScheme es el nombre del esquema OAuth2 (password flow) en OpenAPI.
const SecurityScheme = "OAuth2PasswordBearer"

// Secured es el requisito de seguridad OpenAPI de los endpoints autenticados.
var Secured = []map[string][]string{{SecurityScheme: {}}}

var errPermissionDenied = apperr.New(apperr.KindPermissionDenied, "")

type currentUserKey struct{}

// CurrentUser devuelve el usuario autenticado por Guard.
func CurrentUser(ctx context.Context) (auth.User, bool) {
	user, ok := ctx.Value(currentUserKey{}).(auth.User)
	return user, ok
}

// Guard produce los middlewares de autenticacion y autorizacion.
type Guard struct {
	api     huma.API
	auth    *auth.Service
	tenants *tenants.Service
}

// NewGuard crea el Guard.
func NewGuard(api huma.API, authSvc *auth.Service, tenantsSvc *tenants.Service) *Guard {
	return &Guard{api: api, auth: authSvc, tenants: tenantsSvc}
}

// Authenticated exige un token de acceso valido.
func (g *Guard) Authenticated() huma.Middlewares {
	return huma.Middlewares{g.authenticate}
}

// RequireRole exige un token de acceso valido y un rol minimo.
func (g *Guard) RequireRole(minimum auth.Role) huma.Middlewares {
	return huma.Middlewares{g.authenticate, g.requireRole(minimum)}
}

// authenticate valida el JWT, carga el usuario y, si pertenece a un tenant,
// marca el contexto con su schema. De este modo cualquier transaccion
// posterior de la peticion opera aislada dentro del schema del tenant.
func (g *Guard) authenticate(hctx huma.Context, next func(huma.Context)) {
	ctx := hctx.Context()
	token, ok := bearerToken(hctx.Header("Authorization"))
	if !ok {
		g.unauthorized(hctx, apperr.New(apperr.KindAuthentication, "").Message)
		return
	}

	user, err := g.auth.CurrentUserFromToken(ctx, token)
	if err != nil {
		g.writeError(hctx, err)
		return
	}
	ctx = context.WithValue(ctx, currentUserKey{}, user)

	if user.TenantID != nil {
		// El schema_name proviene del slug validado almacenado por el tenant.
		tenant, err := g.tenants.GetTenant(ctx, *user.TenantID)
		if err != nil {
			g.writeError(hctx, err)
			return
		}
		ctx = database.WithTenantSchema(ctx, tenant.SchemaName)
	}

	next(huma.WithContext(hctx, ctx))
}

func (g *Guard) requireRole(minimum auth.Role) func(huma.Context, func(huma.Context)) {
	return func(hctx huma.Context, next func(huma.Context)) {
		user, ok := CurrentUser(hctx.Context())
		if !ok || !auth.HasAtLeast(user.Role, minimum) {
			_ = huma.WriteErr(g.api, hctx, http.StatusForbidden, errPermissionDenied.Message)
			return
		}
		next(hctx)
	}
}

func (g *Guard) writeError(hctx huma.Context, err error) {
	status, message := classify(hctx.Context(), err)
	if status == http.StatusUnauthorized {
		g.unauthorized(hctx, message)
		return
	}
	_ = huma.WriteErr(g.api, hctx, status, message)
}

func (g *Guard) unauthorized(hctx huma.Context, message string) {
	hctx.SetHeader("WWW-Authenticate", "Bearer")
	_ = huma.WriteErr(g.api, hctx, http.StatusUnauthorized, message)
}

func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}
