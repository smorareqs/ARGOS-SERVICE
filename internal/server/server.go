// Package server compone la aplicacion HTTP: router, CORS, logs de acceso,
// configuracion OpenAPI/documentacion y las rutas de la API.
package server

import (
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/smorareqs/argos-service/internal/api"
	v1 "github.com/smorareqs/argos-service/internal/api/v1"
	"github.com/smorareqs/argos-service/internal/app"
	"github.com/smorareqs/argos-service/internal/core/config"
	"github.com/smorareqs/argos-service/internal/core/ratelimit"
	"github.com/smorareqs/argos-service/internal/core/reqctx"
)

const (
	title   = "ARGOS"
	version = "0.1.0"
)

// New devuelve el handler HTTP de la aplicacion.
func New(cfg config.Config, log *slog.Logger, svc app.Services) http.Handler {
	router := chi.NewMux()
	router.Use(middleware.RequestID)
	router.Use(accessLog(log))
	router.Use(middleware.Recoverer)
	router.Use(corsHandler(cfg.CORSOrigins))
	router.Use(api.WithClientIP)
	// Limite por IP en acceso y rutas publicas (fase06.md §5).
	router.Use(api.RateLimitIP(ratelimit.PerMinute(cfg.RateLimitAuth), v1.Prefix+"/auth/login", v1.Prefix+"/auth/password/", v1.Prefix+"/auth/select-tenant", v1.Prefix+"/auth/refresh"))
	router.Use(api.RateLimitIP(ratelimit.PerMinute(cfg.RateLimitPublic), v1.Prefix+"/public/"))

	humaAPI := humachi.New(router, humaConfig())
	v1.Register(humaAPI, svc)
	if cfg.Environment == config.Local {
		mountDevICal(router)
	}
	return router
}

func humaConfig() huma.Config {
	cfg := huma.DefaultConfig(title, version)
	// Sin el transformador por defecto que agrega `$schema` a cada respuesta:
	// los cuerpos quedan exactamente como el contrato publicado.
	cfg.CreateHooks = nil
	cfg.SchemasPath = ""
	cfg.OpenAPIPath = v1.Prefix + "/openapi" // sirve openapi.json y openapi.yaml
	cfg.DocsPath = "/docs"
	cfg.DocsRenderer = huma.DocsRendererSwaggerUI
	cfg.Formats = map[string]huma.Format{
		"application/json": huma.DefaultJSONFormat,
		"json":             huma.DefaultJSONFormat,
	}
	// Swagger UI: pegar el access_token de POST /auth/login en "Authorize".
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		api.SecurityScheme: {Type: "http", Scheme: "bearer", BearerFormat: "JWT"},
		// Clave API de la empresa para POST /public/reservations (fase 6).
		api.APIKeyScheme: {Type: "apiKey", In: "header", Name: "X-Api-Key"},
	}
	return cfg
}

// corsHandler admite cualquier origen si la lista contiene "*". En ese caso
// devuelve el origen de la peticion (no "*") para que funcione tambien con
// credenciales.
func corsHandler(origins []string) func(http.Handler) http.Handler {
	opts := cors.Options{
		AllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"*"},
		// El cliente lee Retry-After de un 429 para mostrar la espera y
		// Content-Disposition para nombrar las descargas (Excel, boletas).
		ExposedHeaders:   []string{"Retry-After", "Content-Disposition"},
		AllowCredentials: true,
		MaxAge:           600,
	}
	if slices.Contains(origins, "*") {
		opts.AllowOriginFunc = func(*http.Request, string) bool { return true }
	} else {
		opts.AllowedOrigins = origins
	}
	return cors.Handler(opts)
}

func accessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			ctx, trace := reqctx.WithTrace(r.Context())
			next.ServeHTTP(ww, r.WithContext(ctx))

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			log.InfoContext(r.Context(), "HTTP",
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", middleware.GetReqID(r.Context()),
				"tenant_id", trace.TenantID,
				"user_id", trace.UserID,
			)
		})
	}
}
