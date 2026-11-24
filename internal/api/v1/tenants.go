package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
)

type createTenantInput struct {
	Body tenants.TenantCreate
}

type listTenantsInput struct {
	Limit  int `query:"limit" default:"100" minimum:"1" maximum:"1000"`
	Offset int `query:"offset" default:"0" minimum:"0"`
}

type getTenantInput struct {
	TenantID uuid.UUID `path:"tenant_id" format:"uuid"`
}

type updateTenantInput struct {
	TenantID uuid.UUID `path:"tenant_id" format:"uuid"`
	Body     tenants.TenantUpdate
}

type tenantOutput struct {
	Body tenants.TenantRead
}

type tenantListOutput struct {
	Body []tenants.TenantRead
}

type platformAuditInput struct {
	TenantID string `query:"tenant_id" doc:"Filtra por empresa (uuid)."`
	Action   string `query:"action" maxLength:"64" doc:"Prefijo de la accion, p. ej. tenant."`
	Limit    int    `query:"limit" default:"100" minimum:"1" maximum:"500"`
	Offset   int    `query:"offset" default:"0" minimum:"0"`
}

type platformAuditOutput struct {
	Body page[audit.PlatformRecord]
}

func registerTenants(v1 huma.API, svc *tenants.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1, "/platform")
	// La administracion de tenants es competencia del proveedor (ARGOS).
	grp.UseMiddleware(guard.RequirePlatformAdmin()...)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"plataforma"}
		op.Security = api.Secured
	})

	huma.Register(grp, huma.Operation{
		OperationID:   "create-tenant",
		Method:        http.MethodPost,
		Path:          "/tenants",
		DefaultStatus: http.StatusCreated,
		Summary:       "Crear empresa",
		Description: "Crea el schema con sus migraciones y los siete roles. Con `admin_email`, esa persona queda " +
			"con el rol de administracion (si ya tiene cuenta) o recibe una invitacion por correo.",
		Errors: []int{http.StatusConflict, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *createTenantInput) (*tenantOutput, error) {
		tenant, err := svc.CreateTenantBy(ctx, &p.User.ID, in.Body)
		return &tenantOutput{Body: svc.Read(ctx, tenant)}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "list-tenants",
		Method:      http.MethodGet,
		Path:        "/tenants",
		Summary:     "Empresas con sus numeros",
	}, public(func(ctx context.Context, in *listTenantsInput) (*tenantListOutput, error) {
		list, err := svc.PlatformTenants(ctx, in.Limit, in.Offset)
		return &tenantListOutput{Body: list}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "get-tenant",
		Method:      http.MethodGet,
		Path:        "/tenants/{tenant_id}",
		Summary:     "Obtener empresa",
	}, public(func(ctx context.Context, in *getTenantInput) (*tenantOutput, error) {
		tenant, err := svc.GetTenant(ctx, in.TenantID)
		return &tenantOutput{Body: svc.Read(ctx, tenant)}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "update-tenant",
		Method:      http.MethodPatch,
		Path:        "/tenants/{tenant_id}",
		Summary:     "Cambiar nombre, estado o banderas",
		Description: "`is_active: false` suspende la empresa y cierra todas sus sesiones.",
		Errors:      []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *updateTenantInput) (*tenantOutput, error) {
		tenant, err := svc.UpdateTenant(ctx, p.User.ID, in.TenantID, in.Body)
		return &tenantOutput{Body: svc.Read(ctx, tenant)}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "platform-audit",
		Method:      http.MethodGet,
		Path:        "/audit",
		Summary:     "Bitacora de la plataforma",
	}, public(func(ctx context.Context, in *platformAuditInput) (*platformAuditOutput, error) {
		f := audit.PlatformFilter{Action: in.Action, Limit: in.Limit, Offset: in.Offset}
		if in.TenantID != "" {
			id, err := uuid.Parse(in.TenantID)
			if err != nil {
				return nil, huma.Error422UnprocessableEntity("tenant_id no es un uuid.")
			}
			f.TenantID = &id
		}
		list, err := svc.PlatformAudit(ctx, f)
		return &platformAuditOutput{Body: pageOf(list)}, err
	}))
}
