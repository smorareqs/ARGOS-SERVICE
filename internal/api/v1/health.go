package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/smorareqs/argos-service/internal/modules/health"
)

type healthOutput struct {
	Body health.Status
}

func registerHealth(v1 huma.API, svc *health.Service) {
	huma.Register(v1, huma.Operation{
		OperationID: "healthcheck",
		Method:      http.MethodGet,
		Path:        "/health",
		Summary:     "Estado del servicio",
		Tags:        []string{"health"},
	}, func(ctx context.Context, _ *struct{}) (*healthOutput, error) {
		return &healthOutput{Body: svc.Check(ctx)}, nil
	})
	// Fase 6: lista para recibir trafico (BD, S3, migraciones y cola). 503 si no.
	huma.Register(v1, huma.Operation{
		OperationID: "readiness",
		Method:      http.MethodGet,
		Path:        "/health/ready",
		Summary:     "Lista para recibir tráfico",
		Description: "200 con status ready si la BD responde, S3 responde y no hay migraciones pendientes; 503 si no. " +
			"Incluye el estado de la cola de trabajos.",
		Tags: []string{"health"},
	}, func(ctx context.Context, _ *struct{}) (*readyOutput, error) {
		r := svc.Ready(ctx)
		status := http.StatusOK
		if r.Status != "ready" {
			status = http.StatusServiceUnavailable
		}
		return &readyOutput{Status: status, Body: r}, nil
	})
}

type readyOutput struct {
	Status int
	Body   health.Ready
}
