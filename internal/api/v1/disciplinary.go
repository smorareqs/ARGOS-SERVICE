package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/disciplinary"
)

// Disciplina (fase05.md §4). No depende de la bandera payroll: nunca toca la
// nomina.

// DisciplinaryParam es el id de la accion en la ruta.
type DisciplinaryParam struct {
	ActionID uuid.UUID `path:"action_id" format:"uuid"`
}

type (
	disciplinaryOutput     struct{ Body disciplinary.ActionRead }
	disciplinaryListOutput struct {
		Body page[disciplinary.ActionRead]
	}
	disciplinaryCreateInput struct{ Body disciplinary.ActionCreate }
)

func registerDisciplinary(v1 huma.API, svc *disciplinary.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"disciplina"}
		op.Security = api.Secured
	})
	manage := guard.RequirePermission(authz.HRDisciplinaryManage)
	own := guard.RequirePermission(authz.HRDisciplinaryOwn)
	huma.Register(grp, huma.Operation{
		OperationID: "list-disciplinary", Method: http.MethodGet, Path: "/disciplinary", Summary: "Acciones disciplinarias",
		Description: "Las mas recientes primero; por colaborador o solo las que esperan acuse (W-J4).", Middlewares: manage,
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		EmployeeID uuid.UUID `query:"employee_id" format:"uuid"`
		Pending    bool      `query:"pending" doc:"Solo las que esperan acuse."`
		Cursor     string    `query:"cursor" maxLength:"512"`
		Limit      int       `query:"limit" minimum:"0" maximum:"500"`
	},
	) (*disciplinaryListOutput, error) {
		f := disciplinary.Filter{Pending: in.Pending, Cursor: in.Cursor, Limit: in.Limit}
		if in.EmployeeID != uuid.Nil {
			f.EmployeeID = &in.EmployeeID
		}
		items, next, err := svc.List(ctx, p.Actor(), f)
		return &disciplinaryListOutput{Body: page[disciplinary.ActionRead]{Items: items, NextCursor: next}}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-disciplinary", Method: http.MethodPost, Path: "/disciplinary", Summary: "Nueva accion",
		Description:   "Avisa al colaborador por correo y publica disciplinary.created. Adjuntos: modulo disciplinary, tipo attachment.",
		DefaultStatus: http.StatusCreated, Middlewares: manage, Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *disciplinaryCreateInput) (*disciplinaryOutput, error) {
		out, err := svc.Create(ctx, p.Actor(), in.Body)
		return &disciplinaryOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "my-disciplinary", Method: http.MethodGet, Path: "/disciplinary/me", Summary: "Mi historial disciplinario",
		Description: "Con URLs firmadas de los adjuntos (app).", Middlewares: own,
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*disciplinaryListOutput, error) {
		list, err := svc.Me(ctx, p.Actor())
		return &disciplinaryListOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "get-disciplinary", Method: http.MethodGet, Path: "/disciplinary/{action_id}", Summary: "Ficha de la accion",
		Description: "Quien administra disciplina ve todas; el colaborador, solo las suyas (404 en las demas).",
		Middlewares: guard.RequireAnyPermission(authz.HRDisciplinaryManage, authz.HRDisciplinaryOwn), Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *DisciplinaryParam) (*disciplinaryOutput, error) {
		out, err := svc.Get(ctx, p.Actor(), in.ActionID)
		return &disciplinaryOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "acknowledge-disciplinary", Method: http.MethodPost, Path: "/disciplinary/{action_id}/acknowledge", Summary: "Dar acuse",
		Description: "Solo el colaborador, una vez (422 si ya lo dio; 404 si no es suya).", Middlewares: own,
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		DisciplinaryParam
		Body disciplinary.Acknowledge
	},
	) (*disciplinaryOutput, error) {
		out, err := svc.Acknowledge(ctx, p.Actor(), in.ActionID, in.Body)
		return &disciplinaryOutput{Body: out}, err
	}))
}
