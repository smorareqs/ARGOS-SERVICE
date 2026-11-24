package api

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/smorareqs/argos-service/internal/core/ratelimit"
	"github.com/smorareqs/argos-service/internal/core/reqctx"
)

// APIKeyScheme es el esquema OpenAPI de la clave API (cabecera X-Api-Key).
const APIKeyScheme = "apiKey"

// APIKeySecured es el requisito de seguridad de las rutas publicas con clave.
var APIKeySecured = []map[string][]string{{APIKeyScheme: {}}}

// RateLimitIP limita por IP las peticiones cuyas rutas empiezan con alguno de
// los prefijos (fase06.md §5). Con l nil no limita. Responde 429 con
// Retry-After y el mismo formato de error que el resto de la API.
func RateLimitIP(l *ratelimit.Limiter, prefixes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if l == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions || !hasPrefix(r.URL.Path, prefixes) {
				next.ServeHTTP(w, r)
				return
			}
			ok, wait := l.Allow(reqctx.IP(r.Context()) + "|" + prefixes[0])
			if ok {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"title": http.StatusText(http.StatusTooManyRequests), "status": http.StatusTooManyRequests,
				"detail": "Demasiadas peticiones seguidas. Espera un momento y vuelve a intentar.",
			})
		})
	}
}

func hasPrefix(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}
