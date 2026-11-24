package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/alerts"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/dashboard"
)

// Alertas (fase03.md §5) e Inicio (W-B1).

type alertsInput struct {
	Status     string    `query:"status" enum:"open,active,acknowledged,resolved" doc:"Por defecto open (activas y reconocidas)."`
	Type       string    `query:"type" enum:"asset_failure,maintenance_overdue,task_late,low_stock,discrepancy_open,checkout_alarm_unattended"`
	Severity   string    `query:"severity" enum:"low,medium,high"`
	PropertyID uuid.UUID `query:"property_id" format:"uuid"`
	Cursor     string    `query:"cursor" maxLength:"512"`
	Limit      int       `query:"limit" minimum:"0" maximum:"500" doc:"Por defecto 50."`
}

type alertsOutput struct {
	Body page[alerts.AlertRead]
}

type alertOutput struct {
	Body alerts.AlertRead
}

type alertPath struct {
	AlertID uuid.UUID `path:"alert_id" format:"uuid"`
}

type alertSummaryOutput struct {
	Body alerts.Summary
}

type dashboardOutput struct {
	Body dashboard.Dashboard
}

func registerAlerts(v1 huma.API, svc *alerts.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1, "/alerts")
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"alertas"}
		op.Security = api.Secured
	})
	read, manage := guard.RequirePermission(authz.AlertsRead), guard.RequirePermission(authz.AlertsManage)
	huma.Register(grp, huma.Operation{
		OperationID: "list-alerts", Method: http.MethodGet, Path: "", Summary: "Alertas",
		Description: "Del alcance (las de propiedades fuera de alcance y las de toda la empresa no las ven los roles restringidos), las mas nuevas primero.",
		Middlewares: read,
	}, authed(func(ctx context.Context, p auth.Principal, in *alertsInput) (*alertsOutput, error) {
		items, next, err := svc.List(ctx, p.Actor(), alerts.Filter{
			Status: in.Status, Type: in.Type, Severity: in.Severity, PropertyID: optionalID(in.PropertyID),
			Cursor: in.Cursor, Limit: in.Limit,
		})
		if err != nil {
			return nil, err
		}
		return &alertsOutput{Body: page[alerts.AlertRead]{Items: items, NextCursor: next}}, nil
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "alerts-summary", Method: http.MethodGet, Path: "/summary", Summary: "Contadores de alertas",
		Description: "Abiertas por tipo, altas y nuevas sin reconocer (tarjetas de W-L1, campana e Inicio).",
		Middlewares: read,
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*alertSummaryOutput, error) {
		out, err := svc.Summarize(ctx, p.Actor())
		return &alertSummaryOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "acknowledge-alert", Method: http.MethodPost, Path: "/{alert_id}/ack", Summary: "Reconocer",
		Description: "Ya se vio: deja de contar como nueva y sigue abierta. Repetirlo no cambia nada.",
		Middlewares: manage, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *alertPath) (*alertOutput, error) {
		out, err := svc.Acknowledge(ctx, p.Actor(), in.AlertID)
		return &alertOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "resolve-alert", Method: http.MethodPost, Path: "/{alert_id}/resolve", Summary: "Resolver",
		Description: "La cierra a mano. Si la causa sigue, el sistema levanta una nueva en su siguiente revision.",
		Middlewares: manage, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *alertPath) (*alertOutput, error) {
		out, err := svc.Resolve(ctx, p.Actor(), in.AlertID)
		return &alertOutput{Body: out}, err
	}))
}

func registerDashboard(v1 huma.API, svc *dashboard.Service, guard *api.Guard) {
	huma.Register(v1, huma.Operation{
		OperationID: "dashboard", Method: http.MethodGet, Path: "/dashboard", Summary: "Inicio",
		Description: "Agregado del dia (W-B1). Cada bloque respeta el alcance y viene vacio sin su permiso: alerts y top_alerts (alerts.read), incidents y upcoming (maintenance.read), activity (audit.read), pins (properties.read). tasks.available es false hasta que el modulo de tareas lo conecte.",
		Tags:        []string{"inicio"}, Security: api.Secured, Middlewares: guard.RequireTenant(),
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*dashboardOutput, error) {
		out, err := svc.Get(ctx, p.Actor())
		return &dashboardOutput{Body: out}, err
	}))
}
