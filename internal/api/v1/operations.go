package v1

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/appsync"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/checklists"
	"github.com/smorareqs/argos-service/internal/modules/employees"
)

// Plantillas de checklist, perfil laboral, bitacora y sync (fase02.md §4).

type templatesOutput struct {
	Body page[checklists.TemplateRead]
}

type templateOutput struct {
	Body checklists.TemplateRead
}

type templatePath struct {
	TemplateID uuid.UUID `path:"template_id" format:"uuid"`
}

type templateCreateInput struct {
	Body checklists.TemplateCreate
}

type templateUpdateInput struct {
	TemplateID uuid.UUID `path:"template_id" format:"uuid"`
	Body       checklists.TemplateUpdate
}

func registerChecklists(v1 huma.API, svc *checklists.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1, "/checklist-templates")
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"checklists"}
		op.Security = api.Secured
	})
	read, manage := guard.RequirePermission(authz.PropertiesRead), guard.RequirePermission(authz.PropertiesManage)
	huma.Register(grp, huma.Operation{
		OperationID: "list-checklist-templates", Method: http.MethodGet, Path: "", Summary: "Plantillas de checklist",
		Description: "Con sus puntos en orden; la de por defecto primero.", Middlewares: read,
	}, authed(func(ctx context.Context, _ auth.Principal, _ *struct{}) (*templatesOutput, error) {
		list, err := svc.List(ctx)
		return &templatesOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "get-checklist-template", Method: http.MethodGet, Path: "/{template_id}", Summary: "Plantilla",
		Middlewares: read, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, _ auth.Principal, in *templatePath) (*templateOutput, error) {
		out, err := svc.Get(ctx, in.TemplateID)
		return &templateOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-checklist-template", Method: http.MethodPost, Path: "", Summary: "Crear una plantilla",
		DefaultStatus: http.StatusCreated, Middlewares: manage, Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *templateCreateInput) (*templateOutput, error) {
		out, err := svc.Create(ctx, p.Actor(), in.Body)
		return &templateOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-checklist-template", Method: http.MethodPatch, Path: "/{template_id}", Summary: "Editar la plantilla",
		Description: "items, si viene, reemplaza todos los puntos. Las tareas ya iniciadas conservan su copia.",
		Middlewares: manage, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *templateUpdateInput) (*templateOutput, error) {
		out, err := svc.Update(ctx, p.Actor(), in.TemplateID, in.Body)
		return &templateOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "delete-checklist-template", Method: http.MethodDelete, Path: "/{template_id}", Summary: "Borrar la plantilla",
		Description: "Las propiedades que la usaban quedan sin plantilla.", Middlewares: manage, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *templatePath) (*struct{}, error) {
		return noContent(svc.Delete(ctx, p.Actor(), in.TemplateID))
	}))
}

type employeesInput struct {
	UserID uuid.UUID `query:"user_id" format:"uuid" doc:"Solo la ficha de esta persona."`
}

type employeesOutput struct {
	Body page[employees.EmployeeRead]
}

type employeeOutput struct {
	Body employees.EmployeeRead
}

type employeePath struct {
	EmployeeID uuid.UUID `path:"employee_id" format:"uuid"`
}

type employeeCreateInput struct {
	Body employees.EmployeeCreate
}

type employeeUpdateInput struct {
	EmployeeID uuid.UUID `path:"employee_id" format:"uuid"`
	Body       employees.EmployeeUpdate
}

type scheduleInput struct {
	EmployeeID uuid.UUID `path:"employee_id" format:"uuid"`
	Body       employees.ScheduleSet
}

func registerEmployees(v1 huma.API, svc *employees.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1, "/employees")
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"colaboradores"}
		op.Security = api.Secured
	})
	mw := guard.RequirePermission(authz.UsersManage)
	huma.Register(grp, huma.Operation{
		OperationID: "list-employees", Method: http.MethodGet, Path: "", Summary: "Fichas laborales",
		Description: "Con users.manage o con permisos de RRHH (nomina, disciplina, asistencia), que eligen colaboradores.",
		Middlewares: guard.RequireAnyPermission(authz.UsersManage, authz.HRPayrollManage, authz.HRDisciplinaryManage, authz.HRAttendanceManage),
	}, authed(func(ctx context.Context, _ auth.Principal, in *employeesInput) (*employeesOutput, error) {
		list, err := svc.List(ctx, optionalID(in.UserID))
		return &employeesOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "get-employee", Method: http.MethodGet, Path: "/{employee_id}", Summary: "Ficha laboral",
		Middlewares: mw, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, _ auth.Principal, in *employeePath) (*employeeOutput, error) {
		out, err := svc.Get(ctx, in.EmployeeID)
		return &employeeOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-employee", Method: http.MethodPost, Path: "", Summary: "Crear la ficha laboral",
		DefaultStatus: http.StatusCreated, Middlewares: mw, Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *employeeCreateInput) (*employeeOutput, error) {
		out, err := svc.Create(ctx, p.Actor(), in.Body)
		return &employeeOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-employee", Method: http.MethodPatch, Path: "/{employee_id}", Summary: "Editar la ficha laboral",
		Middlewares: mw, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *employeeUpdateInput) (*employeeOutput, error) {
		out, err := svc.Update(ctx, p.Actor(), in.EmployeeID, in.Body)
		return &employeeOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "set-employee-schedule", Method: http.MethodPut, Path: "/{employee_id}/schedule", Summary: "Horario semanal",
		Description: "Reemplaza todos los bloques.", Middlewares: mw, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *scheduleInput) (*employeeOutput, error) {
		out, err := svc.SetSchedule(ctx, p.Actor(), in.EmployeeID, in.Body.Blocks)
		return &employeeOutput{Body: out}, err
	}))
}

type auditInput struct {
	Entity     string    `query:"entity" maxLength:"64" doc:"property, building, employee, membership, role..."`
	EntityID   uuid.UUID `query:"entity_id" format:"uuid"`
	PropertyID uuid.UUID `query:"property_id" format:"uuid"`
	ActorID    uuid.UUID `query:"actor_id" format:"uuid"`
	Action     string    `query:"action" maxLength:"64" doc:"Prefijo de la accion (p. ej. property.)."`
	From       time.Time `query:"from" doc:"Desde (incluido), RFC 3339."`
	To         time.Time `query:"to" doc:"Hasta (excluido), RFC 3339."`
	Cursor     string    `query:"cursor" maxLength:"512"`
	Limit      int       `query:"limit" minimum:"0" maximum:"500" doc:"Por defecto 50."`
}

type auditOutput struct {
	Body page[audit.EntryRead]
}

func registerAudit(v1 huma.API, svc *audit.Service, guard *api.Guard) {
	huma.Register(v1, huma.Operation{
		OperationID: "list-audit", Method: http.MethodGet, Path: "/audit", Summary: "Bitacora de la empresa",
		Description: "Lo mas reciente primero, con antes y despues de cada cambio.", Tags: []string{"bitacora"},
		Security: api.Secured, Middlewares: guard.RequirePermission(authz.AuditRead),
	}, authed(func(ctx context.Context, p auth.Principal, in *auditInput) (*auditOutput, error) {
		items, next, err := svc.List(ctx, p.Membership.TenantID, audit.Filter{
			Entity: in.Entity, EntityID: optionalID(in.EntityID), PropertyID: optionalID(in.PropertyID),
			ActorID: optionalID(in.ActorID), Action: in.Action, From: optionalTime(in.From), To: optionalTime(in.To),
			Cursor: in.Cursor, Limit: in.Limit,
		})
		if err != nil {
			return nil, err
		}
		return &auditOutput{Body: page[audit.EntryRead]{Items: items, NextCursor: next}}, nil
	}))
}

type bootstrapOutput struct {
	Body appsync.Bootstrap
}

func registerSync(v1 huma.API, svc *appsync.Service, guard *api.Guard) {
	huma.Register(v1, huma.Operation{
		OperationID: "sync-bootstrap", Method: http.MethodGet, Path: "/sync/bootstrap", Summary: "Datos para trabajar sin conexion",
		Description: "Propiedades del alcance (sin acceso), edificios, plantillas de checklist que usan y horario propio.",
		Tags:        []string{"sync"}, Security: api.Secured, Middlewares: guard.RequireTenant(),
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*bootstrapOutput, error) {
		out, err := svc.Bootstrap(ctx, p.Actor())
		return &bootstrapOutput{Body: out}, err
	}))
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
