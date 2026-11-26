package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/inventory"
)

// Inventario: articulos, bodegas, movimientos, custodia, diferencias y
// reposicion (fase02.md §4, Persona B).

type inventoryOverviewOutput struct {
	Body inventory.InventorySnapshot
}

type inventoryMineOutput struct {
	Body inventory.MyInventory
}

type inventoryItemsInput struct {
	Active string `query:"active" enum:"true,false"`
}

type inventoryItemsOutput struct {
	Body []inventory.InventoryItem
}

type inventoryItemOutput struct {
	Body inventory.InventoryItem
}

type inventoryItemCreateInput struct {
	Body inventory.InventoryItemCreate
}

type inventoryItemUpdateInput struct {
	ItemID uuid.UUID `path:"item_id" format:"uuid"`
	Body   inventory.InventoryItemUpdate
}

type warehousesOutput struct {
	Body []inventory.Warehouse
}

type warehouseOutput struct {
	Body inventory.Warehouse
}

type warehouseCreateInput struct {
	Body inventory.WarehouseCreate
}

type warehouseUpdateInput struct {
	WarehouseID uuid.UUID `path:"warehouse_id" format:"uuid"`
	Body        inventory.WarehouseUpdate
}

type movementInput struct {
	Body inventory.StockChange
}

type custodyReturnInput struct {
	PersonID uuid.UUID `path:"person_id" format:"uuid"`
	Body     inventory.CustodyReturn
}

type discrepancyInput struct {
	ID   uuid.UUID `path:"discrepancy_id" format:"uuid"`
	Body inventory.InventoryDecision
}

type restockDecisionInput struct {
	ID   uuid.UUID `path:"request_id" format:"uuid"`
	Body inventory.InventoryDecision
}

type restockCreateInput struct {
	Body inventory.RestockRequestCreate
}

type restockOutput struct {
	Body inventory.RestockRequest
}

func registerInventory(v1 huma.API, svc *inventory.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"inventario"}
		op.Security = api.Secured
	})
	read := guard.RequirePermission(authz.InventoryRead)
	manage := guard.RequirePermission(authz.InventoryManage)
	checkout := guard.RequireAnyPermission(authz.InventoryCheckout, authz.InventoryManage)
	reconcile := guard.RequireAnyPermission(authz.InventoryReconcile, authz.InventoryManage)

	huma.Register(grp, huma.Operation{
		OperationID: "inventory-overview", Method: http.MethodGet, Path: "/inventory/overview", Summary: "Inventario completo",
		Description: "Articulos, bodegas, existencias, movimientos recientes, custodias, diferencias y solicitudes (web). Sin inventory.reconcile, custodia y diferencias son solo las propias.",
		Middlewares: read,
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*inventoryOverviewOutput, error) {
		out, err := svc.Overview(ctx, p.Actor())
		return &inventoryOverviewOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "inventory-mine", Method: http.MethodGet, Path: "/inventory/me", Summary: "Mi inventario",
		Description: "Custodia propia, articulos y existencias para sacar, y mis solicitudes (app).",
		Middlewares: read,
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*inventoryMineOutput, error) {
		out, err := svc.Mine(ctx, p.Actor())
		return &inventoryMineOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "list-inventory-items", Method: http.MethodGet, Path: "/inventory/items", Summary: "Articulos",
		Middlewares: read,
	}, authed(func(ctx context.Context, _ auth.Principal, in *inventoryItemsInput) (*inventoryItemsOutput, error) {
		out, err := svc.Items(ctx, optionalBool(in.Active))
		return &inventoryItemsOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-inventory-item", Method: http.MethodPost, Path: "/inventory/items", Summary: "Dar de alta un articulo",
		DefaultStatus: http.StatusCreated, Middlewares: manage, Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *inventoryItemCreateInput) (*inventoryItemOutput, error) {
		out, err := svc.CreateItem(ctx, p.Actor(), in.Body)
		return &inventoryItemOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-inventory-item", Method: http.MethodPatch, Path: "/inventory/items/{item_id}", Summary: "Editar el articulo",
		Middlewares: manage, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *inventoryItemUpdateInput) (*inventoryItemOutput, error) {
		out, err := svc.UpdateItem(ctx, p.Actor(), in.ItemID, in.Body)
		return &inventoryItemOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "list-warehouses", Method: http.MethodGet, Path: "/inventory/warehouses", Summary: "Bodegas",
		Middlewares: read,
	}, authed(func(ctx context.Context, _ auth.Principal, _ *struct{}) (*warehousesOutput, error) {
		out, err := svc.Warehouses(ctx)
		return &warehousesOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-warehouse", Method: http.MethodPost, Path: "/inventory/warehouses", Summary: "Crear una bodega",
		Description:   "Sin property_id es central; con property_id es la bodega de esa propiedad (una por propiedad).",
		DefaultStatus: http.StatusCreated, Middlewares: manage, Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *warehouseCreateInput) (*warehouseOutput, error) {
		out, err := svc.CreateWarehouse(ctx, p.Actor(), in.Body)
		return &warehouseOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-warehouse", Method: http.MethodPatch, Path: "/inventory/warehouses/{warehouse_id}", Summary: "Editar la bodega",
		Middlewares: manage, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *warehouseUpdateInput) (*warehouseOutput, error) {
		out, err := svc.UpdateWarehouse(ctx, p.Actor(), in.WarehouseID, in.Body)
		return &warehouseOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-inventory-movement", Method: http.MethodPost, Path: "/inventory/movements", Summary: "Registrar un movimiento",
		Description:   "entry, adjustment (con nota) y transfer requieren inventory.manage; checkout y return mueven a la custodia propia con inventory.checkout. Nunca deja existencia negativa.",
		DefaultStatus: http.StatusNoContent, Middlewares: checkout,
		Errors: []int{http.StatusForbidden, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *movementInput) (*struct{}, error) {
		return noContent(svc.Move(ctx, p.Actor(), in.Body))
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "return-custody", Method: http.MethodPost, Path: "/inventory/custody/{person_id}/return",
		Summary: "Recibir la devolucion de una custodia", Description: "Sin qty devuelve todo; sin warehouse_id va a la bodega central.",
		DefaultStatus: http.StatusNoContent, Middlewares: reconcile,
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *custodyReturnInput) (*struct{}, error) {
		return noContent(svc.ReturnCustody(ctx, p.Actor(), in.PersonID, in.Body))
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-inventory-discrepancy", Method: http.MethodPatch, Path: "/inventory/discrepancies/{discrepancy_id}",
		Summary: "Justificar o cerrar una diferencia", Description: "status: open, justified o closed.",
		DefaultStatus: http.StatusNoContent, Middlewares: reconcile,
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *discrepancyInput) (*struct{}, error) {
		return noContent(svc.SetDiscrepancy(ctx, p.Actor(), in.ID, in.Body))
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-restock-request", Method: http.MethodPost, Path: "/inventory/restock-requests",
		Summary: "Pedir reposicion", DefaultStatus: http.StatusCreated, Middlewares: checkout,
		Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *restockCreateInput) (*restockOutput, error) {
		out, err := svc.CreateRequest(ctx, p.Actor(), in.Body)
		return &restockOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-restock-request", Method: http.MethodPatch, Path: "/inventory/restock-requests/{request_id}",
		Summary: "Resolver una solicitud", Description: "status: approved, rejected o fulfilled.",
		DefaultStatus: http.StatusNoContent, Middlewares: manage,
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *restockDecisionInput) (*struct{}, error) {
		return noContent(svc.SetRequest(ctx, p.Actor(), in.ID, in.Body))
	}))
}
