package inventory

import (
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/optional"
)

// InventoryItem es un articulo con su existencia total.
type InventoryItem struct {
	ID         uuid.UUID `db:"id" json:"id" format:"uuid"`
	SKU        string    `db:"sku" json:"sku"`
	Name       string    `db:"name" json:"name"`
	Category   string    `db:"category" json:"category" enum:"linens,amenities,cleaning,kitchen,keys"`
	Unit       string    `db:"unit" json:"unit"`
	Min        float64   `db:"min" json:"min" doc:"Existencia minima por bodega (alerta low_stock)."`
	Total      float64   `db:"total" json:"total" doc:"Existencia sumada de todas las bodegas."`
	Returnable bool      `db:"returnable" json:"returnable" doc:"Se devuelve completo al cerrar una tarea (blancos, llaves)."`
	IsActive   bool      `db:"is_active" json:"is_active"`
}

// Warehouse es una bodega.
type Warehouse struct {
	ID           uuid.UUID  `db:"id" json:"id" format:"uuid"`
	Name         string     `db:"name" json:"name"`
	PropertyID   *uuid.UUID `db:"property_id" json:"property_id" format:"uuid" nullable:"true" doc:"Vacio = bodega central."`
	PropertyCode *string    `db:"property_code" json:"property_code" nullable:"true"`
	IsActive     bool       `db:"is_active" json:"is_active"`
}

// StockRow es la existencia de un articulo en una bodega.
type StockRow struct {
	ItemID      uuid.UUID `db:"item_id" json:"item_id" format:"uuid"`
	WarehouseID uuid.UUID `db:"warehouse_id" json:"warehouse_id" format:"uuid"`
	Qty         float64   `db:"qty" json:"qty"`
}

// InventoryMovement es un movimiento (inmutable).
type InventoryMovement struct {
	ID         uuid.UUID `db:"id" json:"id" format:"uuid"`
	At         time.Time `db:"at" json:"at"`
	Type       string    `db:"type" json:"type" enum:"entry,checkout,return,transfer,adjustment,consumption"`
	ItemID     uuid.UUID `db:"item_id" json:"item_id" format:"uuid"`
	ItemName   string    `db:"item_name" json:"item_name"`
	Qty        float64   `db:"qty" json:"qty"`
	Warehouse  string    `db:"warehouse" json:"warehouse" doc:"La bodega (en traslados, origen → destino)."`
	Person     *string   `db:"person" json:"person" nullable:"true" doc:"Colaborador de la custodia o quien lo registro."`
	TaskNumber *int      `db:"task_number" json:"task_number" nullable:"true"`
	Note       *string   `db:"note" json:"note" nullable:"true"`
}

// CustodyItem es un articulo en custodia de una persona.
type CustodyItem struct {
	PersonID   uuid.UUID `db:"person_id" json:"person_id" format:"uuid"`
	PersonName string    `db:"person_name" json:"person_name"`
	ItemID     uuid.UUID `db:"item_id" json:"item_id" format:"uuid"`
	ItemName   string    `db:"item_name" json:"item_name"`
	Category   string    `db:"category" json:"category"`
	Returnable bool      `db:"returnable" json:"returnable"`
	Unit       string    `db:"unit" json:"unit"`
	Qty        float64   `db:"qty" json:"qty"`
	Since      time.Time `db:"since" json:"since"`
}

// InventoryDiscrepancy es una diferencia de custodia al cerrar una tarea.
type InventoryDiscrepancy struct {
	ID           uuid.UUID  `db:"id" json:"id" format:"uuid"`
	Status       string     `db:"status" json:"status" enum:"open,justified,closed"`
	PersonID     uuid.UUID  `db:"person_id" json:"person_id" format:"uuid"`
	PersonName   string     `db:"person_name" json:"person_name"`
	ItemID       uuid.UUID  `db:"item_id" json:"item_id" format:"uuid"`
	ItemName     string     `db:"item_name" json:"item_name"`
	Expected     float64    `db:"expected" json:"expected"`
	Returned     float64    `db:"returned" json:"returned"`
	TaskID       *uuid.UUID `db:"task_id" json:"task_id" format:"uuid" nullable:"true"`
	TaskNumber   int        `db:"task_number" json:"task_number" doc:"0 si no viene de una tarea."`
	PropertyCode string     `db:"property_code" json:"property_code"`
	At           time.Time  `db:"at" json:"at"`
	Note         *string    `db:"note" json:"note" nullable:"true"`
}

// RestockRequestItem es un articulo pedido.
type RestockRequestItem struct {
	ItemID   uuid.UUID `json:"item_id" format:"uuid"`
	ItemName string    `json:"item_name"`
	Qty      float64   `json:"qty"`
}

// RestockRequest es una solicitud de reposicion.
type RestockRequest struct {
	ID           uuid.UUID            `db:"id" json:"id" format:"uuid"`
	Status       string               `db:"status" json:"status" enum:"pending,approved,rejected,fulfilled"`
	PersonID     uuid.UUID            `db:"person_id" json:"person_id" format:"uuid"`
	PersonName   string               `db:"person_name" json:"person_name"`
	PropertyID   *uuid.UUID           `db:"property_id" json:"property_id" format:"uuid" nullable:"true"`
	PropertyCode string               `db:"property_code" json:"property_code"`
	Items        []RestockRequestItem `db:"items" json:"items"`
	At           time.Time            `db:"at" json:"at"`
	Note         *string              `db:"note" json:"note" nullable:"true"`
}

// InventorySnapshot es GET /inventory/overview (web W-E1).
type InventorySnapshot struct {
	Items         []InventoryItem        `json:"items"`
	Warehouses    []Warehouse            `json:"warehouses"`
	Stock         []StockRow             `json:"stock"`
	Movements     []InventoryMovement    `json:"movements" doc:"Los 200 mas recientes."`
	Custody       []CustodyItem          `json:"custody"`
	Discrepancies []InventoryDiscrepancy `json:"discrepancies" doc:"Abiertas y las 100 mas recientes."`
	Requests      []RestockRequest       `json:"requests" doc:"Pendientes y las 100 mas recientes."`
}

// MyInventory es GET /inventory/me (app B10).
type MyInventory struct {
	Custody    []CustodyItem    `json:"custody"`
	Items      []InventoryItem  `json:"items" doc:"Articulos activos para sacar o pedir."`
	Warehouses []Warehouse      `json:"warehouses" doc:"Bodegas activas de donde sacar."`
	Stock      []StockRow       `json:"stock"`
	Requests   []RestockRequest `json:"requests" doc:"Sus 30 solicitudes mas recientes."`
}

// InventoryItemCreate es el cuerpo de POST /inventory/items (web: ItemDraft).
type InventoryItemCreate struct {
	SKU        string  `json:"sku" maxLength:"32"`
	Name       string  `json:"name" minLength:"1" maxLength:"128"`
	Category   string  `json:"category" enum:"linens,amenities,cleaning,kitchen,keys"`
	Unit       string  `json:"unit" minLength:"1" maxLength:"16"`
	Min        float64 `json:"min" minimum:"0"`
	Returnable *bool   `json:"returnable,omitempty" doc:"Por defecto: blancos y llaves."`
}

// InventoryItemUpdate es el cuerpo de PATCH /inventory/items/{id}.
type InventoryItemUpdate struct {
	SKU        optional.Value[string] `json:"sku,omitzero" maxLength:"32"`
	Name       *string                `json:"name,omitempty" minLength:"1" maxLength:"128"`
	Category   *string                `json:"category,omitempty" enum:"linens,amenities,cleaning,kitchen,keys"`
	Unit       *string                `json:"unit,omitempty" minLength:"1" maxLength:"16"`
	Min        *float64               `json:"min,omitempty" minimum:"0"`
	Returnable *bool                  `json:"returnable,omitempty"`
	IsActive   *bool                  `json:"is_active,omitempty"`
}

// WarehouseCreate es el cuerpo de POST /inventory/warehouses.
type WarehouseCreate struct {
	Name       string     `json:"name" minLength:"1" maxLength:"128"`
	PropertyID *uuid.UUID `json:"property_id,omitempty" format:"uuid" nullable:"true"`
}

// WarehouseUpdate es el cuerpo de PATCH /inventory/warehouses/{id}.
type WarehouseUpdate struct {
	Name     *string `json:"name,omitempty" minLength:"1" maxLength:"128"`
	IsActive *bool   `json:"is_active,omitempty"`
}

// StockChange es el cuerpo de POST /inventory/movements.
type StockChange struct {
	Kind          string     `json:"kind" enum:"entry,adjustment,transfer,checkout,return"`
	ItemID        uuid.UUID  `json:"item_id" format:"uuid"`
	WarehouseID   uuid.UUID  `json:"warehouse_id" format:"uuid" doc:"Origen (o destino en entradas, ajustes y devoluciones)."`
	ToWarehouseID *uuid.UUID `json:"to_warehouse_id,omitempty" format:"uuid" nullable:"true" doc:"Destino de un traslado."`
	Qty           float64    `json:"qty" doc:"Positiva; en ajustes puede ser negativa."`
	Note          *string    `json:"note,omitempty" maxLength:"500" nullable:"true"`
	UserID        *uuid.UUID `json:"user_id,omitempty" format:"uuid" nullable:"true" doc:"checkout/return de otra persona (requiere inventory.manage). Vacio = uno mismo."`
}

// InventoryDecision es el cuerpo de PATCH de diferencias y solicitudes.
type InventoryDecision struct {
	Status string  `json:"status"`
	Note   *string `json:"note,omitempty" maxLength:"1000" nullable:"true"`
}

// RestockLine es un articulo de una solicitud nueva.
type RestockLine struct {
	ItemID   uuid.UUID `json:"item_id" format:"uuid"`
	Quantity float64   `json:"quantity" exclusiveMinimum:"0"`
}

// RestockRequestCreate es el cuerpo de POST /inventory/restock-requests (app).
type RestockRequestCreate struct {
	PropertyID *uuid.UUID    `json:"property_id,omitempty" format:"uuid" nullable:"true"`
	Items      []RestockLine `json:"items" minItems:"1" maxItems:"50"`
	Note       *string       `json:"note,omitempty" maxLength:"1000" nullable:"true"`
}

// CustodyReturn es el cuerpo de POST /inventory/custody/{person_id}/return.
type CustodyReturn struct {
	ItemID      uuid.UUID  `json:"item_id" format:"uuid"`
	Qty         *float64   `json:"qty,omitempty" exclusiveMinimum:"0" doc:"Vacio = todo."`
	WarehouseID *uuid.UUID `json:"warehouse_id,omitempty" format:"uuid" nullable:"true" doc:"Vacio = la central."`
}
