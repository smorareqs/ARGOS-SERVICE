package api

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// SecurityScheme es el nombre del esquema Bearer (JWT) en OpenAPI.
const SecurityScheme = "bearerAuth"

// Secured es el requisito de seguridad OpenAPI de los endpoints autenticados.
var Secured = []map[string][]string{{SecurityScheme: {}}}

var errPermissionDenied = apperr.New(apperr.KindPermissionDenied, "")

type principalKey struct{}

// CurrentPrincipal devuelve quien hace la peticion (lo deja Guard).
func CurrentPrincipal(ctx context.Context) (auth.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(auth.Principal)
	return p, ok
}

type clientIPKey struct{}

// WithClientIP guarda en el contexto la IP de origen de la peticion. Se usa
// para el bloqueo por intentos fallidos y para registrar sesiones.
func WithClientIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey{}, host)))
	})
}

// ClientIP devuelve la IP de origen (vacia si no se conoce).
func ClientIP(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}

// Guard produce los middlewares de autenticacion y autorizacion.
type Guard struct {
	api  huma.API
	auth *auth.Service
}

// NewGuard crea el Guard.
func NewGuard(api huma.API, authSvc *auth.Service) *Guard {
	return &Guard{api: api, auth: authSvc}
}

// Authenticated exige un access token valido y deja el principal en el
// contexto. Si la sesion tiene empresa, marca el contexto con su schema: toda
// transaccion posterior de la peticion opera aislada en ese schema.
func (g *Guard) Authenticated() huma.Middlewares {
	return huma.Middlewares{g.authenticate}
}

// RequirePermission exige un access token valido y todos los permisos
// indicados en la empresa activa (403 si falta alguno).
func (g *Guard) RequirePermission(perms ...authz.Permission) huma.Middlewares {
	return huma.Middlewares{g.authenticate, g.require(func(p auth.Principal) bool { return p.Can(perms...) })}
}

// RequirePlatformAdmin exige un usuario administrador de la plataforma ARGOS.
func (g *Guard) RequirePlatformAdmin() huma.Middlewares {
	return huma.Middlewares{g.authenticate, g.require(func(p auth.Principal) bool { return p.User.IsPlatformAdmin })}
}

func (g *Guard) authenticate(hctx huma.Context, next func(huma.Context)) {
	ctx := hctx.Context()
	token, ok := bearerToken(hctx.Header("Authorization"))
	if !ok {
		g.unauthorized(hctx, apperr.New(apperr.KindAuthentication, "").Message)
		return
	}
	principal, err := g.auth.ResolvePrincipal(ctx, token)
	if err != nil {
		g.writeError(hctx, err)
		return
	}
	ctx = context.WithValue(ctx, principalKey{}, principal)
	if principal.Membership != nil {
		// El schema proviene del slug validado al crear el tenant.
		ctx = database.WithTenantSchema(ctx, principal.Membership.TenantSchema)
	}
	next(huma.WithContext(hctx, ctx))
}

func (g *Guard) require(allowed func(auth.Principal) bool) func(huma.Context, func(huma.Context)) {
	return func(hctx huma.Context, next func(huma.Context)) {
		p, ok := CurrentPrincipal(hctx.Context())
		if !ok || !allowed(p) {
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
