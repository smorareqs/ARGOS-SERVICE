package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/properties"
)

// Propiedades y edificios (fase02.md §4). La empresa es la de la sesion; una
// propiedad fuera del alcance de quien consulta responde 404.

type propertiesInput struct {
	Query      string    `query:"q" maxLength:"64" doc:"Busca en codigo, nombre, zona y edificio."`
	BuildingID uuid.UUID `query:"building_id" format:"uuid"`
	Active     string    `query:"active" enum:"true,false" doc:"Vacio = activas e inactivas."`
	AssigneeID uuid.UUID `query:"assignee" format:"uuid" doc:"Solo las asignadas a esta persona."`
	OwnerID    uuid.UUID `query:"owner" format:"uuid" doc:"Solo las de este propietario."`
	Cursor     string    `query:"cursor" maxLength:"512"`
	Limit      int       `query:"limit" minimum:"0" maximum:"500" doc:"Por defecto 50."`
}

type propertiesOutput struct {
	Body page[properties.PropertyRead]
}

type propertyOutput struct {
	Body properties.PropertyRead
}

type propertyPath struct {
	PropertyID uuid.UUID `path:"property_id" format:"uuid"`
}

type propertyCreateInput struct {
	Body properties.PropertyCreate
}

type propertyUpdateInput struct {
	PropertyID uuid.UUID `path:"property_id" format:"uuid"`
	Body       properties.PropertyUpdate
}

type accessInfoOutput struct {
	Body properties.AccessInfoRead
}

type peopleInput struct {
	PropertyID uuid.UUID `path:"property_id" format:"uuid"`
	Body       properties.PeopleSet
}

type userPropertiesInput struct {
	UserID uuid.UUID `path:"user_id" format:"uuid"`
	Body   properties.PropertySet
}

type userPropertiesOutput struct {
	Body properties.UserProperties
}

type buildingsOutput struct {
	Body page[properties.BuildingRead]
}

type buildingOutput struct {
	Body properties.BuildingRead
}

type buildingPath struct {
	BuildingID uuid.UUID `path:"building_id" format:"uuid"`
}

type buildingCreateInput struct {
	Body properties.BuildingCreate
}

type buildingUpdateInput struct {
	BuildingID uuid.UUID `path:"building_id" format:"uuid"`
	Body       properties.BuildingUpdate
}

func registerProperties(v1 huma.API, svc *properties.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"propiedades"}
		op.Security = api.Secured
	})
	registerPropertyReads(grp, svc, guard)
	registerPropertyWrites(grp, svc, guard)
	registerPropertyPeople(grp, svc, guard)
	registerBuildings(grp, svc, guard)
}

func registerPropertyReads(grp huma.API, svc *properties.Service, guard *api.Guard) {
	huma.Register(grp, huma.Operation{
		OperationID: "list-properties", Method: http.MethodGet, Path: "/properties", Summary: "Propiedades",
		Description: "Solo las del alcance de quien consulta (limpieza y mantenimiento: asignadas; propietario: propias). Por codigo.",
		Middlewares: guard.RequirePermission(authz.PropertiesRead),
	}, authed(func(ctx context.Context, p auth.Principal, in *propertiesInput) (*propertiesOutput, error) {
		items, next, err := svc.List(ctx, p.Actor(), properties.ListFilter{
			Query: in.Query, BuildingID: optionalID(in.BuildingID), Active: optionalBool(in.Active),
			AssigneeID: optionalID(in.AssigneeID), OwnerID: optionalID(in.OwnerID), Cursor: in.Cursor, Limit: in.Limit,
		})
		if err != nil {
			return nil, err
		}
		return &propertiesOutput{Body: page[properties.PropertyRead]{Items: items, NextCursor: next}}, nil
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "get-property", Method: http.MethodGet, Path: "/properties/{property_id}", Summary: "Ficha de la propiedad",
		Middlewares: guard.RequirePermission(authz.PropertiesRead), Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *propertyPath) (*propertyOutput, error) {
		out, err := svc.Get(ctx, p.Actor(), in.PropertyID)
		return &propertyOutput{Body: out}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "get-property-access-info", Method: http.MethodGet, Path: "/properties/{property_id}/access-info",
		Summary:     "Informacion de acceso",
		Description: "Llaves, codigos y wifi descifrados. Cada consulta queda en la bitacora. Limpieza y mantenimiento solo con tarea del dia (403).",
		Middlewares: guard.RequirePermission(authz.PropertiesAccessInfo), Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *propertyPath) (*accessInfoOutput, error) {
		out, err := svc.AccessInfo(ctx, p.Actor(), in.PropertyID)
		return &accessInfoOutput{Body: out}, err
	}))
}

func registerPropertyWrites(grp huma.API, svc *properties.Service, guard *api.Guard) {
	huma.Register(grp, huma.Operation{
		OperationID: "create-property", Method: http.MethodPost, Path: "/properties", Summary: "Dar de alta una propiedad",
		DefaultStatus: http.StatusCreated, Middlewares: guard.RequirePermission(authz.PropertiesManage),
		Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *propertyCreateInput) (*propertyOutput, error) {
		out, err := svc.Create(ctx, p.Actor(), in.Body)
		return &propertyOutput{Body: out}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "update-property", Method: http.MethodPatch, Path: "/properties/{property_id}", Summary: "Editar la propiedad",
		Description: "Solo cambia lo que viene; null borra un campo opcional. access_info: texto nuevo, \"\" o null la borra.",
		Middlewares: guard.RequirePermission(authz.PropertiesManage),
		Errors:      []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *propertyUpdateInput) (*propertyOutput, error) {
		out, err := svc.Update(ctx, p.Actor(), in.PropertyID, in.Body)
		return &propertyOutput{Body: out}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "delete-property", Method: http.MethodDelete, Path: "/properties/{property_id}", Summary: "Borrar la propiedad",
		Description: "Borrado logico: su historial se conserva y el codigo queda libre.",
		Middlewares: guard.RequirePermission(authz.PropertiesManage), Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *propertyPath) (*struct{}, error) {
		return noContent(svc.Delete(ctx, p.Actor(), in.PropertyID))
	}))
}

func registerPropertyPeople(grp huma.API, svc *properties.Service, guard *api.Guard) {
	manage := guard.RequirePermission(authz.PropertiesManage)
	errs := []int{http.StatusNotFound, http.StatusUnprocessableEntity}
	huma.Register(grp, huma.Operation{
		OperationID: "set-property-assignments", Method: http.MethodPut, Path: "/properties/{property_id}/assignments",
		Summary: "Colaboradores asignados", Description: "Reemplaza la lista completa.", Middlewares: manage, Errors: errs,
	}, authed(func(ctx context.Context, p auth.Principal, in *peopleInput) (*propertyOutput, error) {
		out, err := svc.SetAssignees(ctx, p.Actor(), in.PropertyID, in.Body.UserIDs)
		return &propertyOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "set-property-owners", Method: http.MethodPut, Path: "/properties/{property_id}/owners",
		Summary: "Propietarios", Description: "Reemplaza la lista completa.", Middlewares: manage, Errors: errs,
	}, authed(func(ctx context.Context, p auth.Principal, in *peopleInput) (*propertyOutput, error) {
		out, err := svc.SetOwners(ctx, p.Actor(), in.PropertyID, in.Body.UserIDs)
		return &propertyOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "set-user-assignments", Method: http.MethodPut, Path: "/property-assignments/{user_id}",
		Summary: "Propiedades asignadas a una persona", Description: "Reemplaza la lista completa (ficha del colaborador).",
		Middlewares: manage, Errors: errs,
	}, authed(func(ctx context.Context, p auth.Principal, in *userPropertiesInput) (*userPropertiesOutput, error) {
		out, err := svc.SetUserAssignments(ctx, p.Actor(), in.UserID, in.Body.PropertyIDs)
		return &userPropertiesOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "set-user-ownerships", Method: http.MethodPut, Path: "/property-owners/{user_id}",
		Summary: "Propiedades de un propietario", Description: "Reemplaza la lista completa.", Middlewares: manage, Errors: errs,
	}, authed(func(ctx context.Context, p auth.Principal, in *userPropertiesInput) (*userPropertiesOutput, error) {
		out, err := svc.SetUserOwnerships(ctx, p.Actor(), in.UserID, in.Body.PropertyIDs)
		return &userPropertiesOutput{Body: out}, err
	}))
}

func registerBuildings(grp huma.API, svc *properties.Service, guard *api.Guard) {
	read, manage := guard.RequirePermission(authz.PropertiesRead), guard.RequirePermission(authz.PropertiesManage)
	huma.Register(grp, huma.Operation{
		OperationID: "list-buildings", Method: http.MethodGet, Path: "/buildings", Summary: "Edificios", Middlewares: read,
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*buildingsOutput, error) {
		list, err := svc.Buildings(ctx, p.Actor())
		return &buildingsOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "get-building", Method: http.MethodGet, Path: "/buildings/{building_id}", Summary: "Edificio",
		Middlewares: read, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *buildingPath) (*buildingOutput, error) {
		out, err := svc.Building(ctx, p.Actor(), in.BuildingID)
		return &buildingOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-building", Method: http.MethodPost, Path: "/buildings", Summary: "Dar de alta un edificio",
		DefaultStatus: http.StatusCreated, Middlewares: manage, Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *buildingCreateInput) (*buildingOutput, error) {
		out, err := svc.CreateBuilding(ctx, p.Actor(), in.Body)
		return &buildingOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-building", Method: http.MethodPatch, Path: "/buildings/{building_id}", Summary: "Editar el edificio",
		Middlewares: manage, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *buildingUpdateInput) (*buildingOutput, error) {
		out, err := svc.UpdateBuilding(ctx, p.Actor(), in.BuildingID, in.Body)
		return &buildingOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "delete-building", Method: http.MethodDelete, Path: "/buildings/{building_id}", Summary: "Borrar el edificio",
		Description: "422 si tiene propiedades.", Middlewares: manage,
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *buildingPath) (*struct{}, error) {
		return noContent(svc.DeleteBuilding(ctx, p.Actor(), in.BuildingID))
	}))
}

// optionalID convierte el uuid vacio de un query param en nil.
func optionalID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// optionalBool lee "true"/"false" de un query param (vacio = nil).
func optionalBool(s string) *bool {
	switch s {
	case "true":
		v := true
		return &v
	case "false":
		v := false
		return &v
	default:
		return nil
	}
}
