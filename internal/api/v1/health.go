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
}
