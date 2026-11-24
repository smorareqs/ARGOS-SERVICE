package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/payroll"
)

// Nomina (fase05.md §4). Todo exige la bandera payroll de la empresa (403 sin
// ella); las boletas del propio colaborador (GET /payroll/me) tambien.

type (
	// PayrollPeriodParam es el id del periodo en la ruta.
	PayrollPeriodParam struct {
		PeriodID uuid.UUID `path:"period_id" format:"uuid"`
	}
	// PayrollEntryParam es el id de la linea en la ruta.
	PayrollEntryParam struct {
		EntryID uuid.UUID `path:"entry_id" format:"uuid"`
	}
)

type (
	payrollSettingsOutput struct{ Body payroll.Settings }
	payrollSettingsInput  struct{ Body payroll.SettingsInput }
	payrollPeriodsOutput  struct {
		Body page[payroll.PeriodRead]
	}
	payrollPeriodOutput   struct{ Body payroll.PeriodRead }
	payrollDetailOutput   struct{ Body payroll.PeriodDetail }
	payrollPeriodInput    struct{ Body payroll.PeriodCreate }
	payrollEntryOutput    struct{ Body payroll.Line }
	payrollEmployeeOutput struct {
		Body page[payroll.EmployeeEntry]
	}
	payrollMeOutput struct {
		Body page[payroll.MyPayslip]
	}
	// downloadOutput es un archivo generado que se descarga tal cual.
	downloadOutput struct {
		ContentType        string `header:"Content-Type"`
		ContentDisposition string `header:"Content-Disposition"`
		CacheControl       string `header:"Cache-Control"`
		Body               []byte
	}
)

func download(f payroll.File) *downloadOutput {
	return &downloadOutput{
		ContentType: f.ContentType, ContentDisposition: `attachment; filename="` + f.Name + `"`,
		CacheControl: "no-store", Body: f.Body,
	}
}

func registerPayroll(v1 huma.API, svc *payroll.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"nomina"}
		op.Security = api.Secured
	})
	need := func(perms ...authz.Permission) huma.Middlewares {
		return guard.RequireFeature(authz.FeaturePayroll, guard.RequirePermission(perms...))
	}
	either := func(perms ...authz.Permission) huma.Middlewares {
		return guard.RequireFeature(authz.FeaturePayroll, guard.RequireAnyPermission(perms...))
	}
	registerPayrollSettings(grp, svc, need)
	registerPayrollPeriods(grp, svc, need, either)
	registerPayrollFlow(grp, svc, need)
	registerPayrollFiles(grp, svc, either)
}

func registerPayrollSettings(grp huma.API, svc *payroll.Service, need guardFn) {
	huma.Register(grp, huma.Operation{
		OperationID: "get-payroll-settings", Method: http.MethodGet, Path: "/payroll/settings", Summary: "Configuracion de nomina",
		Middlewares: need(authz.HRPayrollManage),
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*payrollSettingsOutput, error) {
		out, err := svc.Settings(ctx, p.Actor())
		return &payrollSettingsOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-payroll-settings", Method: http.MethodPut, Path: "/payroll/settings", Summary: "Guardar la configuracion de nomina",
		Description: "Aplica desde el siguiente calculo.", Middlewares: need(authz.HRPayrollManage),
		Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *payrollSettingsInput) (*payrollSettingsOutput, error) {
		out, err := svc.UpdateSettings(ctx, p.Actor(), in.Body)
		return &payrollSettingsOutput{Body: out}, err
	}))
}

func registerPayrollPeriods(grp huma.API, svc *payroll.Service, need, either guardFn) {
	read := either(authz.HRPayrollManage, authz.HRPayrollExport)
	huma.Register(grp, huma.Operation{
		OperationID: "list-payroll-periods", Method: http.MethodGet, Path: "/payroll/periods", Summary: "Periodos de nomina",
		Description: "Con estado y totales, el mas reciente primero (W-J2).", Middlewares: read,
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*payrollPeriodsOutput, error) {
		list, err := svc.Periods(ctx, p.Actor())
		return &payrollPeriodsOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-payroll-period", Method: http.MethodPost, Path: "/payroll/periods", Summary: "Nuevo periodo",
		Description: "Queda en borrador. 422 si se cruza con otro del mismo tipo.", DefaultStatus: http.StatusCreated,
		Middlewares: need(authz.HRPayrollManage), Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *payrollPeriodInput) (*payrollPeriodOutput, error) {
		out, err := svc.CreatePeriod(ctx, p.Actor(), in.Body)
		return &payrollPeriodOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "get-payroll-period", Method: http.MethodGet, Path: "/payroll/periods/{period_id}", Summary: "Detalle del periodo",
		Description: "Lineas por colaborador, totales y de donde salen los datos (W-J3).", Middlewares: read,
		Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *PayrollPeriodParam) (*payrollDetailOutput, error) {
		out, err := svc.Period(ctx, p.Actor(), in.PeriodID)
		return &payrollDetailOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "delete-payroll-period", Method: http.MethodDelete, Path: "/payroll/periods/{period_id}", Summary: "Borrar el periodo",
		Description: "Solo en borrador (409 si no).", DefaultStatus: http.StatusNoContent, Middlewares: need(authz.HRPayrollManage),
		Errors: []int{http.StatusNotFound, http.StatusConflict},
	}, authed(func(ctx context.Context, p auth.Principal, in *PayrollPeriodParam) (*struct{}, error) {
		return nil, svc.DeletePeriod(ctx, p.Actor(), in.PeriodID)
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "list-employee-payroll-entries", Method: http.MethodGet, Path: "/payroll/entries", Summary: "Nomina de un colaborador",
		Description: "Sus lineas en todos los periodos (ficha del colaborador, W-D2).", Middlewares: need(authz.HRPayrollManage),
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		EmployeeID uuid.UUID `query:"employee_id" format:"uuid" required:"true"`
	},
	) (*payrollEmployeeOutput, error) {
		list, err := svc.EmployeeEntries(ctx, p.Actor(), in.EmployeeID)
		return &payrollEmployeeOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "my-payslips", Method: http.MethodGet, Path: "/payroll/me", Summary: "Mis boletas",
		Description: "Lineas de periodos cerrados del colaborador con la URL firmada de su boleta (app, B20). 404 sin ficha laboral.",
		Middlewares: need(authz.HRAttendanceOwn), Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*payrollMeOutput, error) {
		list, err := svc.Me(ctx, p.Actor())
		return &payrollMeOutput{Body: pageOf(list)}, err
	}))
}

func registerPayrollFlow(grp huma.API, svc *payroll.Service, need guardFn) {
	manage := need(authz.HRPayrollManage)
	for _, op := range []struct {
		id, path, summary, doc string
		fn                     func(context.Context, authz.Actor, uuid.UUID) (payroll.PeriodDetail, error)
	}{
		{"compute-payroll-period", "compute", "Calcular", "Solo en borrador (409 en revision o cerrado). Reemplaza las lineas.", svc.Compute},
		{"review-payroll-period", "review", "Pasar a revision", "Borrador calculado → revision: se pueden corregir las lineas.", svc.Review},
		{"reopen-payroll-period", "reopen", "Volver a borrador", "Revision → borrador para recalcular (el calculo reemplaza las correcciones).", svc.Reopen},
		{"close-payroll-period", "close", "Cerrar el periodo", "Revision → cerrado: inmutable; guarda Excel y boletas en S3, avisa por correo y publica payroll.closed.", svc.Close},
	} {
		fn := op.fn
		huma.Register(grp, huma.Operation{
			OperationID: op.id, Method: http.MethodPost, Path: "/payroll/periods/{period_id}/" + op.path, Summary: op.summary,
			Description: op.doc, Middlewares: manage,
			Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity},
		}, authed(func(ctx context.Context, p auth.Principal, in *PayrollPeriodParam) (*payrollDetailOutput, error) {
			out, err := fn(ctx, p.Actor(), in.PeriodID)
			return &payrollDetailOutput{Body: out}, err
		}))
	}
	huma.Register(grp, huma.Operation{
		OperationID: "update-payroll-entry", Method: http.MethodPatch, Path: "/payroll/periods/{period_id}/entries/{entry_id}",
		Summary: "Corregir una linea", Description: "Solo en revision. Deja historial; recalcula base, IGSS, total y liquido.",
		Middlewares: manage, Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		PayrollPeriodParam
		PayrollEntryParam
		Body payroll.EntryUpdate
	},
	) (*payrollEntryOutput, error) {
		out, err := svc.UpdateEntry(ctx, p.Actor(), in.PeriodID, in.EntryID, in.Body)
		return &payrollEntryOutput{Body: out}, err
	}))
}

func registerPayrollFiles(grp huma.API, svc *payroll.Service, either guardFn) {
	export := either(authz.HRPayrollExport, authz.HRPayrollManage)
	huma.Register(grp, huma.Operation{
		OperationID: "export-payroll-period", Method: http.MethodGet, Path: "/payroll/periods/{period_id}/export.xlsx",
		Summary: "Descargar Excel", Description: "Hojas Nomina, Bono 14 o aguinaldo (si aplica) y Asistencia.",
		Middlewares: export, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *PayrollPeriodParam) (*downloadOutput, error) {
		f, err := svc.ExportXLSX(ctx, p.Actor(), in.PeriodID)
		if err != nil {
			return nil, err
		}
		return download(f), nil
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "payroll-period-payslips", Method: http.MethodGet, Path: "/payroll/periods/{period_id}/payslips.pdf",
		Summary: "Descargar las boletas", Description: "Un PDF con una boleta por pagina.",
		Middlewares: export, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *PayrollPeriodParam) (*downloadOutput, error) {
		f, err := svc.PeriodPayslips(ctx, p.Actor(), in.PeriodID)
		if err != nil {
			return nil, err
		}
		return download(f), nil
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "payroll-entry-payslip", Method: http.MethodGet, Path: "/payroll/entries/{entry_id}/payslip.pdf",
		Summary:     "Descargar una boleta",
		Description: "Con hr.payroll.export o manage, cualquiera; con hr.attendance.own, solo la propia de un periodo cerrado (404 en las demas).",
		Middlewares: either(authz.HRPayrollExport, authz.HRPayrollManage, authz.HRAttendanceOwn), Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *PayrollEntryParam) (*downloadOutput, error) {
		f, err := svc.Payslip(ctx, p.Actor(), in.EntryID)
		if err != nil {
			return nil, err
		}
		return download(f), nil
	}))
}
