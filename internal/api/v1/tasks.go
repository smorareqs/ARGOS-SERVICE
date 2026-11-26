package v1

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/tasks"
)

// Itinerario y ejecucion de tareas (fase02.md §4, Persona B).

type tasksInput struct {
	Date         string    `query:"date" format:"date" doc:"Un solo dia (atajo de from=to)."`
	From         string    `query:"from" format:"date"`
	To           string    `query:"to" format:"date" doc:"Incluido. Maximo 92 dias."`
	PropertyID   uuid.UUID `query:"property_id" format:"uuid"`
	AssigneeID   uuid.UUID `query:"assignee_id" format:"uuid"`
	Status       string    `query:"status" doc:"Uno o varios separados por coma. Sin filtro no trae canceladas."`
	Mine         bool      `query:"mine" doc:"Solo las asignadas a quien consulta (app)."`
	UpdatedSince time.Time `query:"updated_since" doc:"Sincronizacion incremental de la app (incluye canceladas)."`
	Cursor       string    `query:"cursor" maxLength:"512"`
	Limit        int       `query:"limit" minimum:"0" maximum:"500" doc:"Por defecto 50."`
}

type tasksOutput struct {
	Body page[tasks.TaskRead]
}

type taskOutput struct {
	Body tasks.TaskRead
}

type taskDetailOutput struct {
	Body tasks.TaskDetail
}

type taskPath struct {
	TaskID uuid.UUID `path:"task_id" format:"uuid"`
}

type taskCreateInput struct {
	Body tasks.TaskCreate
}

type taskRescheduleInput struct {
	TaskID uuid.UUID `path:"task_id" format:"uuid"`
	Body   tasks.TaskReschedule
}

type taskDecisionInput struct {
	TaskID uuid.UUID `path:"task_id" format:"uuid"`
	Body   *tasks.TaskDecision
}

type taskStartInput struct {
	TaskID uuid.UUID `path:"task_id" format:"uuid"`
	Body   tasks.TaskStart
}

type taskItemInput struct {
	TaskID uuid.UUID `path:"task_id" format:"uuid"`
	ItemID uuid.UUID `path:"item_id" format:"uuid"`
	Body   tasks.TaskItemUpdate
}

type taskItemOutput struct {
	Body tasks.TaskChecklistItem
}

type taskConsumptionsInput struct {
	TaskID uuid.UUID `path:"task_id" format:"uuid"`
	Body   tasks.TaskConsumptions
}

type taskCompleteInput struct {
	TaskID uuid.UUID `path:"task_id" format:"uuid"`
	Body   tasks.TaskComplete
}

type generateInput struct {
	Body tasks.TaskGenerate
}

type generateOutput struct {
	Body tasks.TaskGenerated
}

type taskAssignablesOutput struct {
	Body []tasks.TaskAssignee
}

func decisionOf(b *tasks.TaskDecision) tasks.TaskDecision {
	if b == nil {
		return tasks.TaskDecision{}
	}
	return *b
}

func registerTasks(v1 huma.API, svc *tasks.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"tareas"}
		op.Security = api.Secured
	})
	read := guard.RequirePermission(authz.TasksRead)
	manage := guard.RequirePermission(authz.TasksManage)
	assign := guard.RequireAnyPermission(authz.TasksAssign, authz.TasksManage)
	validate := guard.RequirePermission(authz.TasksValidate)
	execute := guard.RequirePermission(authz.TasksExecute)

	huma.Register(grp, huma.Operation{
		OperationID: "list-tasks", Method: http.MethodGet, Path: "/tasks", Summary: "Tareas",
		Description: "Del alcance (sus propiedades o asignadas a quien consulta), por dia y hora. Cada una trae su avance de checklist, insumos y linea de tiempo.",
		Middlewares: read,
	}, authed(func(ctx context.Context, p auth.Principal, in *tasksInput) (*tasksOutput, error) {
		f := tasks.Filter{
			Date: in.Date, From: in.From, To: in.To, PropertyID: optionalID(in.PropertyID), AssigneeID: optionalID(in.AssigneeID),
			Mine: in.Mine, Cursor: in.Cursor, Limit: in.Limit,
		}
		if in.Status != "" {
			for _, st := range strings.Split(in.Status, ",") {
				if st = strings.TrimSpace(st); st != "" {
					f.Statuses = append(f.Statuses, st)
				}
			}
		}
		if !in.UpdatedSince.IsZero() {
			f.UpdatedSince = &in.UpdatedSince
		}
		items, next, err := svc.List(ctx, p.Actor(), f)
		if err != nil {
			return nil, err
		}
		return &tasksOutput{Body: page[tasks.TaskRead]{Items: items, NextCursor: next}}, nil
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "list-task-assignables", Method: http.MethodGet, Path: "/tasks/assignables",
		Summary: "Personas que atienden tareas", Description: "Con tasks.execute; property_ids son sus propiedades si su rol es de alcance restringido.",
		Middlewares: assign,
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*taskAssignablesOutput, error) {
		out, err := svc.Assignables(ctx, p.Actor())
		return &taskAssignablesOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "get-task", Method: http.MethodGet, Path: "/tasks/{task_id}", Summary: "Detalle de la tarea",
		Description: "Con su checklist (fotos por punto), evidencias y la custodia de quien consulta para el cierre.",
		Middlewares: read, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *taskPath) (*taskDetailOutput, error) {
		out, err := svc.Get(ctx, p.Actor(), in.TaskID)
		return &taskDetailOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-task", Method: http.MethodPost, Path: "/tasks", Summary: "Programar una tarea",
		Description:   "Las limpiezas y las inspecciones copian el checklist de la propiedad (o el de por defecto).",
		DefaultStatus: http.StatusCreated, Middlewares: manage, Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *taskCreateInput) (*taskOutput, error) {
		out, err := svc.Create(ctx, p.Actor(), in.Body)
		return &taskOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "reschedule-task", Method: http.MethodPost, Path: "/tasks/{task_id}/reschedule",
		Summary: "Reprogramar o reasignar", Description: "Con motivo. assignee_id null la deja sin asignar.",
		Middlewares: assign, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *taskRescheduleInput) (*taskOutput, error) {
		out, err := svc.Reschedule(ctx, p.Actor(), in.TaskID, in.Body)
		return &taskOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "validate-task", Method: http.MethodPost, Path: "/tasks/{task_id}/validate", Summary: "Validar",
		Description: "done → validated, con nota opcional.", Middlewares: validate,
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *taskDecisionInput) (*taskOutput, error) {
		out, err := svc.Validate(ctx, p.Actor(), in.TaskID, decisionOf(in.Body))
		return &taskOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "return-task", Method: http.MethodPost, Path: "/tasks/{task_id}/return", Summary: "Devolver para corregir",
		Description: "done o validated → returned; la nota (motivo) es obligatoria.", Middlewares: validate,
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *taskDecisionInput) (*taskOutput, error) {
		out, err := svc.Return(ctx, p.Actor(), in.TaskID, decisionOf(in.Body))
		return &taskOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "cancel-task", Method: http.MethodPost, Path: "/tasks/{task_id}/cancel", Summary: "Cancelar",
		Middlewares: manage, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *taskDecisionInput) (*taskOutput, error) {
		out, err := svc.Cancel(ctx, p.Actor(), in.TaskID, decisionOf(in.Body))
		return &taskOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "generate-tasks-from-reservations", Method: http.MethodPost, Path: "/tasks/generate-from-reservations",
		Summary: "Crear limpiezas de salida", Description: "Para las reservas vivas que salen en el rango y aun no tienen una.",
		Middlewares: manage, Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *generateInput) (*generateOutput, error) {
		out, err := svc.GenerateFromReservations(ctx, p.Actor(), in.Body)
		return &generateOutput{Body: out}, err
	}))

	// Ejecucion (app).
	huma.Register(grp, huma.Operation{
		OperationID: "start-task", Method: http.MethodPost, Path: "/tasks/{task_id}/start", Summary: "Iniciar",
		Description: "Requiere GPS. Solo el asignado (o quien asigna); sin asignado, la toma quien la inicia.",
		Middlewares: execute, Errors: []int{http.StatusNotFound, http.StatusForbidden, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *taskStartInput) (*taskDetailOutput, error) {
		out, err := svc.Start(ctx, p.Actor(), in.TaskID, in.Body)
		return &taskDetailOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-task-checklist-item", Method: http.MethodPatch, Path: "/tasks/{task_id}/checklist/{item_id}",
		Summary: "Marcar un punto del checklist", Description: "Las fotos se suben con module tasks, kind before/after/evidence, entity_type task_checklist_item y entity_id el punto.",
		Middlewares: execute, Errors: []int{http.StatusNotFound, http.StatusForbidden, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *taskItemInput) (*taskItemOutput, error) {
		out, err := svc.UpdateItem(ctx, p.Actor(), in.TaskID, in.ItemID, in.Body)
		return &taskItemOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "add-task-consumptions", Method: http.MethodPost, Path: "/tasks/{task_id}/consumptions",
		Summary: "Registrar insumos usados", Description: "Salen de la custodia propia (solo consumibles).",
		Middlewares: execute, Errors: []int{http.StatusNotFound, http.StatusForbidden, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *taskConsumptionsInput) (*taskOutput, error) {
		out, err := svc.AddConsumptions(ctx, p.Actor(), in.TaskID, in.Body)
		return &taskOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "complete-task", Method: http.MethodPost, Path: "/tasks/{task_id}/complete", Summary: "Cerrar (prueba de salida)",
		Description: "Exige checklist completo, fotos obligatorias, GPS y la conciliacion de toda la custodia: devuelto + usado + se queda = custodia. Cada diferencia abre una discrepancia.",
		Middlewares: execute, Errors: []int{http.StatusNotFound, http.StatusForbidden, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *taskCompleteInput) (*taskDetailOutput, error) {
		out, err := svc.Complete(ctx, p.Actor(), in.TaskID, in.Body)
		return &taskDetailOutput{Body: out}, err
	}))
}
