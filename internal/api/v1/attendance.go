package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/attendance"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/metrics"
)

// Asistencia, metricas, metas e incentivos (fase05.md §4, Persona B).

type punchInput struct{ Body attendance.Punch }
type attendanceRecordOutput struct{ Body attendance.RecordRead }
type attendanceMineInput struct {
	Month string `query:"month" pattern:"^[0-9]{4}-[0-9]{2}$" doc:"AAAA-MM; vacio = este mes."`
}
type attendanceMineOutput struct{ Body attendance.Month }
type attendanceListInput struct {
	From     string    `query:"from" format:"date" required:"true"`
	To       string    `query:"to" format:"date" required:"true" doc:"Inclusive."`
	Employee uuid.UUID `query:"employee" format:"uuid" doc:"Solo ese colaborador."`
	Limit    int       `query:"limit" minimum:"1" maximum:"2000" default:"500"`
}
type attendanceListOutput struct{ Body attendance.RecordPage }
type attendanceManualInput struct{ Body attendance.ManualCreate }
type attendanceDayInput struct {
	Date string `query:"date" format:"date" required:"true"`
}
type attendanceDayOutput struct{ Body []attendance.RecordRead }
type attendanceUpdateInput struct {
	ID   uuid.UUID `path:"record_id" format:"uuid"`
	Body attendance.RecordUpdate
}

func registerAttendance(v1 huma.API, svc *attendance.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"asistencia"}
		op.Security = api.Secured
	})
	own := guard.RequirePermission(authz.HRAttendanceOwn)
	manage := guard.RequirePermission(authz.HRAttendanceManage)
	huma.Register(grp, huma.Operation{
		OperationID: "clock-in", Method: http.MethodPost, Path: "/attendance/clock-in", Summary: "Marcar entrada",
		Description:   "Con GPS (obligatorio) y foto opcional. 409 si ya hay una entrada abierta.",
		DefaultStatus: http.StatusCreated, Middlewares: own, Errors: []int{http.StatusConflict, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *punchInput) (*attendanceRecordOutput, error) {
		out, err := svc.ClockIn(ctx, p.Actor(), in.Body)
		return &attendanceRecordOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "clock-out", Method: http.MethodPost, Path: "/attendance/clock-out", Summary: "Marcar salida",
		Description: "Cierra la entrada abierta. 409 si no hay.", Middlewares: own,
		Errors: []int{http.StatusConflict, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *punchInput) (*attendanceRecordOutput, error) {
		out, err := svc.ClockOut(ctx, p.Actor(), in.Body)
		return &attendanceRecordOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "my-attendance", Method: http.MethodGet, Path: "/attendance/me", Summary: "Mi asistencia del mes",
		Middlewares: own, Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *attendanceMineInput) (*attendanceMineOutput, error) {
		out, err := svc.Me(ctx, p.Actor(), in.Month)
		return &attendanceMineOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "list-attendance", Method: http.MethodGet, Path: "/attendance", Summary: "Marcajes del equipo",
		Description: "Los que empezaron entre esos dias (hora de la empresa).", Middlewares: manage,
	}, authed(func(ctx context.Context, _ auth.Principal, in *attendanceListInput) (*attendanceListOutput, error) {
		var emp *uuid.UUID
		if in.Employee != uuid.Nil {
			emp = &in.Employee
		}
		out, err := svc.List(ctx, in.From, in.To, emp, in.Limit)
		return &attendanceListOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "attendance-day", Method: http.MethodGet, Path: "/attendance/day", Summary: "Quién marcó ese día",
		Description: "Para el tablero del equipo (app C1): tambien con tasks.assign.",
		Middlewares: guard.RequireAnyPermission(authz.HRAttendanceManage, authz.TasksAssign),
	}, authed(func(ctx context.Context, _ auth.Principal, in *attendanceDayInput) (*attendanceDayOutput, error) {
		out, err := svc.Day(ctx, in.Date)
		return &attendanceDayOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-attendance", Method: http.MethodPost, Path: "/attendance", Summary: "Registrar un marcaje a mano",
		Description:   "Con motivo. Si el colaborador tiene una entrada abierta ese dia, la cierra con estas horas.",
		DefaultStatus: http.StatusCreated, Middlewares: manage,
		Errors: []int{http.StatusConflict, http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *attendanceManualInput) (*attendanceRecordOutput, error) {
		out, err := svc.Create(ctx, p.Actor(), in.Body)
		return &attendanceRecordOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-attendance", Method: http.MethodPatch, Path: "/attendance/{record_id}", Summary: "Corregir un marcaje",
		Middlewares: manage, Errors: []int{http.StatusConflict, http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *attendanceUpdateInput) (*attendanceRecordOutput, error) {
		out, err := svc.Update(ctx, p.Actor(), in.ID, in.Body)
		return &attendanceRecordOutput{Body: out}, err
	}))
}

// --- Metricas --------------------------------------------------------------------

type metricsRangeInput struct {
	Range string `query:"range" enum:"7d,30d,week,month" default:"7d"`
}
type metricsBoardOutput struct{ Body metrics.Board }
type metricsMineOutput struct{ Body metrics.MyMetrics }
type metricsTeamOutput struct{ Body metrics.Team }
type metricsReportInput struct {
	Month string `query:"month" pattern:"^[0-9]{4}-[0-9]{2}$" doc:"AAAA-MM; vacio = este mes."`
}
type metricsReportOutput struct{ Body metrics.MonthReport }
type goalsInput struct {
	All bool `query:"all" doc:"Incluye las desactivadas."`
}
type goalsOutput struct{ Body []metrics.GoalRead }
type goalOutput struct{ Body metrics.GoalRead }
type goalCreateInput struct{ Body metrics.GoalWrite }
type goalUpdateInput struct {
	ID   uuid.UUID `path:"goal_id" format:"uuid"`
	Body metrics.GoalUpdate
}
type incentivesOutput struct{ Body []metrics.IncentiveRead }
type incentivesDecideInput struct{ Body metrics.Decide }

func registerMetrics(v1 huma.API, svc *metrics.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"metricas"}
		op.Security = api.Secured
	})
	huma.Register(grp, huma.Operation{
		OperationID: "metrics-board", Method: http.MethodGet, Path: "/metrics/board", Summary: "Tablero de desempeño",
		Description: "Limpiezas por dia, por persona y por propiedad. El propietario ve solo sus propiedades.",
		Middlewares: guard.RequirePermission(authz.MetricsAll),
	}, authed(func(ctx context.Context, p auth.Principal, in *metricsRangeInput) (*metricsBoardOutput, error) {
		out, err := svc.Board(ctx, p.Actor(), in.Range)
		return &metricsBoardOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "metrics-team", Method: http.MethodGet, Path: "/metrics/team", Summary: "Ranking del equipo",
		Description: "Colaboradores con ficha; el puesto es dentro de su rol.",
		Middlewares: guard.RequireAnyPermission(authz.MetricsTeam, authz.MetricsAll),
	}, authed(func(ctx context.Context, p auth.Principal, in *metricsRangeInput) (*metricsTeamOutput, error) {
		out, err := svc.Team(ctx, p.Actor(), in.Range)
		return &metricsTeamOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "metrics-me", Method: http.MethodGet, Path: "/metrics/me", Summary: "Mis métricas",
		Description: "Con el rango anterior (tendencia), mi puesto en mi rol y mis metas.",
		Middlewares: guard.RequireAnyPermission(authz.MetricsOwn, authz.MetricsTeam, authz.MetricsAll),
	}, authed(func(ctx context.Context, p auth.Principal, in *metricsRangeInput) (*metricsMineOutput, error) {
		out, err := svc.Me(ctx, p.Actor(), in.Range)
		return &metricsMineOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "metrics-report", Method: http.MethodGet, Path: "/metrics/report", Summary: "Reporte del mes por propiedad",
		Description: "Ocupacion, limpiezas e incidencias (app E3). El propietario ve solo sus propiedades.",
		Middlewares: guard.RequirePermission(authz.MetricsAll), Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *metricsReportInput) (*metricsReportOutput, error) {
		out, err := svc.Report(ctx, p.Actor(), in.Month)
		return &metricsReportOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "list-goals", Method: http.MethodGet, Path: "/metrics/goals", Summary: "Metas",
		Middlewares: guard.RequireAnyPermission(authz.MetricsAll, authz.HRPayrollManage),
	}, authed(func(ctx context.Context, _ auth.Principal, in *goalsInput) (*goalsOutput, error) {
		out, err := svc.Goals(ctx, in.All)
		return &goalsOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-goal", Method: http.MethodPost, Path: "/metrics/goals", Summary: "Nueva meta",
		DefaultStatus: http.StatusCreated, Middlewares: guard.RequirePermission(authz.HRPayrollManage),
		Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *goalCreateInput) (*goalOutput, error) {
		out, err := svc.CreateGoal(ctx, p.Actor(), in.Body)
		return &goalOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-goal", Method: http.MethodPatch, Path: "/metrics/goals/{goal_id}", Summary: "Corregir o desactivar una meta",
		Middlewares: guard.RequirePermission(authz.HRPayrollManage), Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *goalUpdateInput) (*goalOutput, error) {
		out, err := svc.UpdateGoal(ctx, p.Actor(), in.ID, in.Body)
		return &goalOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "list-incentives", Method: http.MethodGet, Path: "/metrics/incentives", Summary: "Incentivos",
		Description: "Los pendientes y los de los ultimos 120 dias.", Middlewares: guard.RequirePermission(authz.HRPayrollManage),
	}, authed(func(ctx context.Context, _ auth.Principal, _ *struct{}) (*incentivesOutput, error) {
		out, err := svc.Incentives(ctx)
		return &incentivesOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "decide-incentives", Method: http.MethodPost, Path: "/metrics/incentives/decide", Summary: "Aprobar o rechazar incentivos",
		Description: "Solo cambian los pendientes. Devuelve la lista completa.",
		Middlewares: guard.RequirePermission(authz.HRPayrollManage), Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *incentivesDecideInput) (*incentivesOutput, error) {
		out, err := svc.DecideIncentives(ctx, p.Actor(), in.Body)
		return &incentivesOutput{Body: out}, err
	}))
}
