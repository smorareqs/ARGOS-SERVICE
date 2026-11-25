package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/app"
)

// MapsConfig dice a la web que mapa dibujar.
type MapsConfig struct {
	Provider string `json:"provider" enum:"google,none" doc:"google si hay clave configurada; none = mapa ilustrado."`
	APIKey   string `json:"api_key,omitempty" doc:"Clave de navegador de Google Maps (restringida por dominio)."`
	MapID    string `json:"map_id,omitempty" doc:"Estilo de mapa de Google (opcional)."`
}

// ClientConfigRead es la configuracion del cliente web.
type ClientConfigRead struct {
	Maps MapsConfig `json:"maps"`
}

type clientConfigOutput struct {
	Body ClientConfigRead
}

func registerClientConfig(v1 huma.API, cfg app.ClientConfig, guard *api.Guard) {
	huma.Register(v1, huma.Operation{
		OperationID: "client-config", Method: http.MethodGet, Path: "/client-config", Summary: "Configuración del cliente web",
		Description: "Lo que la web necesita al iniciar sesión. Hoy: el proveedor del mapa del Inicio y su clave de navegador " +
			"(ARGOS_GOOGLE_MAPS_KEY). Sin clave, provider = none y la web dibuja el mapa ilustrado.",
		Tags: []string{"sistema"}, Security: api.Secured, Middlewares: guard.Authenticated(),
	}, func(_ context.Context, _ *struct{}) (*clientConfigOutput, error) {
		maps := MapsConfig{Provider: "none"}
		if cfg.MapsKey != "" {
			maps = MapsConfig{Provider: "google", APIKey: cfg.MapsKey, MapID: cfg.MapsMapID}
		}
		return &clientConfigOutput{Body: ClientConfigRead{Maps: maps}}, nil
	})
}
