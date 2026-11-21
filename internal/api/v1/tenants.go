package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
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

type tenantOutput struct {
	Body tenants.TenantRead
}

type tenantListOutput struct {
	Body []tenants.TenantRead
}

func registerTenants(v1 huma.API, svc *tenants.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1, "/platform/tenants")
	// La administracion de tenants es competencia del proveedor (ARGOS).
	grp.UseMiddleware(guard.RequirePlatformAdmin()...)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"tenants"}
		op.Security = api.Secured
	})

	huma.Register(grp, huma.Operation{
		OperationID:   "create-tenant",
		Method:        http.MethodPost,
		Path:          "",
		DefaultStatus: http.StatusCreated,
		Summary:       "Crear tenant",
	}, func(ctx context.Context, in *createTenantInput) (*tenantOutput, error) {
		tenant, err := svc.CreateTenant(ctx, in.Body)
		if err != nil {
			return nil, api.HTTPError(ctx, err)
		}
		return &tenantOutput{Body: tenants.NewTenantRead(tenant)}, nil
	})

	huma.Register(grp, huma.Operation{
		OperationID: "list-tenants",
		Method:      http.MethodGet,
		Path:        "",
		Summary:     "Listar tenants",
	}, func(ctx context.Context, in *listTenantsInput) (*tenantListOutput, error) {
		list, err := svc.ListTenants(ctx, in.Limit, in.Offset)
		if err != nil {
			return nil, api.HTTPError(ctx, err)
		}
		out := make([]tenants.TenantRead, 0, len(list))
		for _, t := range list {
			out = append(out, tenants.NewTenantRead(t))
		}
		return &tenantListOutput{Body: out}, nil
	})

	huma.Register(grp, huma.Operation{
		OperationID: "get-tenant",
		Method:      http.MethodGet,
		Path:        "/{tenant_id}",
		Summary:     "Obtener tenant",
	}, func(ctx context.Context, in *getTenantInput) (*tenantOutput, error) {
		tenant, err := svc.GetTenant(ctx, in.TenantID)
		if err != nil {
			return nil, api.HTTPError(ctx, err)
		}
		return &tenantOutput{Body: tenants.NewTenantRead(tenant)}, nil
	})
}
