package inventory

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// --- Lecturas --------------------------------------------------------------------

const selectItem = `
	SELECT i.id, coalesce(i.sku, '') AS sku, i.name, i.category, i.unit, i.min_stock::float8 AS min,
	       coalesce((SELECT sum(s.quantity) FROM inventory_stock s JOIN warehouses w ON w.id = s.warehouse_id
	                 WHERE s.item_id = i.id AND w.is_active), 0)::float8 AS total,
	       i.returnable, i.is_active
	FROM inventory_items i`

const selectWarehouse = `
	SELECT w.id, w.name, w.property_id, p.code AS property_code, w.is_active
	FROM warehouses w LEFT JOIN properties p ON p.id = w.property_id`

const selectMovement = `
	SELECT m.id, m.created_at AS at, m.type, m.item_id, i.name AS item_name, m.quantity::float8 AS qty,
	       CASE WHEN m.type = 'transfer' THEN coalesce(wf.name, '') || ' → ' || coalesce(wt.name, '')
	            ELSE coalesce(wt.name, wf.name, '') END AS warehouse,
	       coalesce(ue.full_name, uc.full_name) AS person, t.number AS task_number,
	       coalesce(m.notes, m.reference) AS note
	FROM inventory_movements m
	JOIN inventory_items i ON i.id = m.item_id
	LEFT JOIN warehouses wf ON wf.id = m.from_warehouse_id
	LEFT JOIN warehouses wt ON wt.id = m.to_warehouse_id
	LEFT JOIN shared.users ue ON ue.id = m.employee_user_id
	LEFT JOIN shared.users uc ON uc.id = m.created_by
	LEFT JOIN tasks t ON t.id = m.task_id`

const selectCustody = `
	SELECT c.user_id AS person_id, u.full_name AS person_name, c.item_id, i.name AS item_name, i.category,
	       i.returnable, i.unit, c.quantity::float8 AS qty, c.since
	FROM employee_custody c
	JOIN shared.users u ON u.id = c.user_id
	JOIN inventory_items i ON i.id = c.item_id
	WHERE c.quantity > 0`

const selectDiscrepancy = `
	SELECT d.id, d.status, d.user_id AS person_id, u.full_name AS person_name, d.item_id, i.name AS item_name,
	       d.expected_qty::float8 AS expected, d.returned_qty::float8 AS returned, d.task_id,
	       coalesce(t.number, 0) AS task_number, coalesce(p.code, '') AS property_code, d.created_at AS at,
	       d.justification AS note
	FROM inventory_discrepancies d
	JOIN shared.users u ON u.id = d.user_id
	JOIN inventory_items i ON i.id = d.item_id
	LEFT JOIN tasks t ON t.id = d.task_id
	LEFT JOIN properties p ON p.id = t.property_id`

const selectRequest = `
	SELECT r.id, r.status, r.requested_by AS person_id, u.full_name AS person_name, r.property_id,
	       coalesce(p.code, '') AS property_code,
	       coalesce((SELECT json_agg(json_build_object('item_id', ri.item_id, 'item_name', i.name, 'qty', ri.quantity::float8)
	                                 ORDER BY i.name)
	                 FROM restock_request_items ri JOIN inventory_items i ON i.id = ri.item_id
	                 WHERE ri.request_id = r.id), '[]'::json) AS items,
	       r.created_at AS at, r.notes AS note
	FROM restock_requests r
	JOIN shared.users u ON u.id = r.requested_by
	LEFT JOIN properties p ON p.id = r.property_id`

func collect[T any](ctx context.Context, q database.Querier, sql string, args ...any) ([]T, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[T])
	if out == nil {
		out = []T{}
	}
	return out, err
}

// Overview arma la pantalla de inventario de la web. Sin inventory.reconcile,
// la custodia y las diferencias son solo las propias.
func (s *Service) Overview(ctx context.Context, actor authz.Actor) (InventorySnapshot, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (InventorySnapshot, error) {
		var out InventorySnapshot
		var err error
		if out.Items, err = collect[InventoryItem](ctx, q, selectItem+` ORDER BY i.is_active DESC, i.name`); err != nil {
			return out, err
		}
		if out.Warehouses, err = collect[Warehouse](ctx, q, selectWarehouse+` ORDER BY (w.property_id IS NOT NULL), w.name`); err != nil {
			return out, err
		}
		if out.Stock, err = collect[StockRow](ctx, q, `
			SELECT item_id, warehouse_id, quantity::float8 AS qty FROM inventory_stock ORDER BY item_id, warehouse_id`); err != nil {
			return out, err
		}
		if out.Movements, err = collect[InventoryMovement](ctx, q, selectMovement+` ORDER BY m.created_at DESC, m.id DESC LIMIT 200`); err != nil {
			return out, err
		}
		all := actor.Permissions[authz.InventoryReconcile] || actor.Permissions[authz.InventoryManage]
		if out.Custody, err = collect[CustodyItem](ctx, q, selectCustody+`
			AND ($1::bool OR c.user_id = $2) ORDER BY u.full_name, i.name`, all, actor.UserID); err != nil {
			return out, err
		}
		if out.Discrepancies, err = collect[InventoryDiscrepancy](ctx, q, `
			SELECT * FROM (`+selectDiscrepancy+` WHERE ($1::bool OR d.user_id = $2)
			ORDER BY (d.status = 'open') DESC, d.created_at DESC LIMIT 150) x ORDER BY (status = 'open') DESC, at DESC`,
			all, actor.UserID); err != nil {
			return out, err
		}
		out.Requests, err = collect[RestockRequest](ctx, q, selectRequest+`
			WHERE ($1::bool OR r.requested_by = $2)
			ORDER BY (r.status = 'pending') DESC, r.created_at DESC LIMIT 150`, all, actor.UserID)
		return out, err
	})
}

// Mine es el inventario de la app: su custodia, lo que puede sacar y sus
// solicitudes.
func (s *Service) Mine(ctx context.Context, actor authz.Actor) (MyInventory, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (MyInventory, error) {
		var out MyInventory
		var err error
		if out.Custody, err = collect[CustodyItem](ctx, q, selectCustody+` AND c.user_id = $1 ORDER BY i.name`, actor.UserID); err != nil {
			return out, err
		}
		if out.Items, err = collect[InventoryItem](ctx, q, selectItem+` WHERE i.is_active ORDER BY i.name`); err != nil {
			return out, err
		}
		if out.Warehouses, err = collect[Warehouse](ctx, q, selectWarehouse+`
			WHERE w.is_active ORDER BY (w.property_id IS NOT NULL), w.name`); err != nil {
			return out, err
		}
		if out.Stock, err = collect[StockRow](ctx, q, `
			SELECT s.item_id, s.warehouse_id, s.quantity::float8 AS qty FROM inventory_stock s
			JOIN warehouses w ON w.id = s.warehouse_id WHERE w.is_active AND s.quantity > 0`); err != nil {
			return out, err
		}
		out.Requests, err = collect[RestockRequest](ctx, q, selectRequest+`
			WHERE r.requested_by = $1 ORDER BY r.created_at DESC LIMIT 30`, actor.UserID)
		return out, err
	})
}

// Items devuelve los articulos (activos primero).
func (s *Service) Items(ctx context.Context, active *bool) ([]InventoryItem, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]InventoryItem, error) {
		return collect[InventoryItem](ctx, q, selectItem+` WHERE ($1::bool IS NULL OR i.is_active = $1)
			ORDER BY i.is_active DESC, i.name`, active)
	})
}

// ActiveItems son los articulos activos (bootstrap de la app).
func ActiveItems(ctx context.Context, q database.Querier) ([]InventoryItem, error) {
	return collect[InventoryItem](ctx, q, selectItem+` WHERE i.is_active ORDER BY i.name`)
}

// Warehouses devuelve las bodegas.
func (s *Service) Warehouses(ctx context.Context) ([]Warehouse, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]Warehouse, error) {
		return collect[Warehouse](ctx, q, selectWarehouse+` ORDER BY w.is_active DESC, (w.property_id IS NOT NULL), w.name`)
	})
}

// --- Articulos y bodegas -----------------------------------------------------------

// CreateItem da de alta un articulo.
func (s *Service) CreateItem(ctx context.Context, actor authz.Actor, in InventoryItemCreate) (InventoryItem, error) {
	returnable := returnableCategory(in.Category)
	if in.Returnable != nil {
		returnable = *in.Returnable
	}
	sku := trimmed(&in.SKU)
	if sku != nil {
		up := strings.ToUpper(*sku)
		sku = &up
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (InventoryItem, error) {
		var id uuid.UUID
		err := q.QueryRow(ctx, `
			INSERT INTO inventory_items (sku, name, category, unit, min_stock, returnable)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			sku, trim(in.Name), in.Category, trim(in.Unit), round3(in.Min), returnable).Scan(&id)
		if database.IsUniqueViolation(err) {
			return InventoryItem{}, ErrSKUTaken.WithField("body.sku")
		}
		if err != nil {
			return InventoryItem{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "inventory.item_created", Entity: "inventory_item", EntityID: &id,
			After: map[string]any{"sku": sku, "name": trim(in.Name), "category": in.Category, "min": in.Min},
		}); err != nil {
			return InventoryItem{}, err
		}
		return itemByID(ctx, q, id)
	})
}

// UpdateItem edita un articulo.
func (s *Service) UpdateItem(ctx context.Context, actor authz.Actor, id uuid.UUID, in InventoryItemUpdate) (InventoryItem, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (InventoryItem, error) {
		cur, err := itemByID(ctx, q, id)
		if err != nil {
			return InventoryItem{}, err
		}
		sku := &cur.SKU
		if cur.SKU == "" {
			sku = nil
		}
		if in.SKU.Sent {
			sku = nil
			if !in.SKU.Null {
				if v := strings.ToUpper(trim(in.SKU.Value)); v != "" {
					sku = &v
				}
			}
		}
		next := cur
		if in.Name != nil {
			next.Name = trim(*in.Name)
		}
		if in.Category != nil {
			next.Category = *in.Category
		}
		if in.Unit != nil {
			next.Unit = trim(*in.Unit)
		}
		if in.Min != nil {
			next.Min = round3(*in.Min)
		}
		if in.Returnable != nil {
			next.Returnable = *in.Returnable
		}
		if in.IsActive != nil {
			next.IsActive = *in.IsActive
		}
		_, err = q.Exec(ctx, `
			UPDATE inventory_items SET sku = $2, name = $3, category = $4, unit = $5, min_stock = $6,
			       returnable = $7, is_active = $8, updated_at = now() WHERE id = $1`,
			id, sku, next.Name, next.Category, next.Unit, next.Min, next.Returnable, next.IsActive)
		if database.IsUniqueViolation(err) {
			return InventoryItem{}, ErrSKUTaken.WithField("body.sku")
		}
		if err != nil {
			return InventoryItem{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "inventory.item_updated", Entity: "inventory_item", EntityID: &id,
			Before: cur, After: next,
		}); err != nil {
			return InventoryItem{}, err
		}
		return itemByID(ctx, q, id)
	})
}

// CreateWarehouse da de alta una bodega (central o de una propiedad).
func (s *Service) CreateWarehouse(ctx context.Context, actor authz.Actor, in WarehouseCreate) (Warehouse, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Warehouse, error) {
		var id uuid.UUID
		err := q.QueryRow(ctx, `INSERT INTO warehouses (name, property_id) VALUES ($1, $2) RETURNING id`,
			trim(in.Name), in.PropertyID).Scan(&id)
		if database.IsUniqueViolation(err) {
			return Warehouse{}, ErrWarehouseTaken.WithField("body.name")
		}
		if database.IsForeignKeyViolation(err) {
			return Warehouse{}, ErrUnknownProperty.WithField("body.property_id")
		}
		if err != nil {
			return Warehouse{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "inventory.warehouse_created", Entity: "warehouse", EntityID: &id,
			PropertyID: in.PropertyID, After: map[string]any{"name": trim(in.Name)},
		}); err != nil {
			return Warehouse{}, err
		}
		return warehouseByID(ctx, q, id)
	})
}

// UpdateWarehouse renombra o desactiva una bodega.
func (s *Service) UpdateWarehouse(ctx context.Context, actor authz.Actor, id uuid.UUID, in WarehouseUpdate) (Warehouse, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Warehouse, error) {
		cur, err := warehouseByID(ctx, q, id)
		if err != nil {
			return Warehouse{}, err
		}
		next := cur
		if in.Name != nil {
			next.Name = trim(*in.Name)
		}
		if in.IsActive != nil {
			next.IsActive = *in.IsActive
		}
		_, err = q.Exec(ctx, `UPDATE warehouses SET name = $2, is_active = $3, updated_at = now() WHERE id = $1`,
			id, next.Name, next.IsActive)
		if database.IsUniqueViolation(err) {
			return Warehouse{}, ErrWarehouseTaken.WithField("body.name")
		}
		if err != nil {
			return Warehouse{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "inventory.warehouse_updated", Entity: "warehouse", EntityID: &id,
			Before: cur, After: next,
		}); err != nil {
			return Warehouse{}, err
		}
		return next, nil
	})
}

func itemByID(ctx context.Context, q database.Querier, id uuid.UUID) (InventoryItem, error) {
	rows, err := q.Query(ctx, selectItem+` WHERE i.id = $1`, id)
	if err != nil {
		return InventoryItem{}, err
	}
	it, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[InventoryItem])
	if errors.Is(err, pgx.ErrNoRows) {
		return InventoryItem{}, ErrItemNotFound
	}
	return it, err
}

func warehouseByID(ctx context.Context, q database.Querier, id uuid.UUID) (Warehouse, error) {
	rows, err := q.Query(ctx, selectWarehouse+` WHERE w.id = $1`, id)
	if err != nil {
		return Warehouse{}, err
	}
	w, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Warehouse])
	if errors.Is(err, pgx.ErrNoRows) {
		return Warehouse{}, ErrWarehouseNotFound
	}
	return w, err
}

// --- Movimientos -------------------------------------------------------------------

// Move registra un movimiento de la web o de la app. entry, adjustment y
// transfer requieren inventory.manage; checkout y return, inventory.checkout
// para uno mismo (o inventory.manage para otra persona).
func (s *Service) Move(ctx context.Context, actor authz.Actor, in StockChange) error {
	manage := actor.Permissions[authz.InventoryManage]
	person := actor.UserID
	switch in.Kind {
	case "entry", "adjustment", "transfer":
		if !manage {
			return ErrForbiddenMovement
		}
	case "checkout", "return":
		if in.UserID != nil && *in.UserID != actor.UserID {
			if !manage {
				return ErrForbiddenMovement
			}
			person = *in.UserID
		} else if !manage && !actor.Permissions[authz.InventoryCheckout] {
			return ErrForbiddenMovement
		}
	}
	qty := round3(in.Qty)
	if qty == 0 || (in.Kind != "adjustment" && qty < 0) {
		return ErrInvalidQuantity.WithField("body.qty")
	}
	note := trimmed(in.Note)
	if in.Kind == "adjustment" && note == nil {
		return ErrNoteRequired.WithField("body.note")
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		if err := activeItem(ctx, q, in.ItemID); err != nil {
			return err
		}
		if err := activeWarehouse(ctx, q, in.WarehouseID, "body.warehouse_id"); err != nil {
			return err
		}
		mv := movement{Type: in.Kind, ItemID: in.ItemID, Qty: qty, Note: note, By: actor.UserID}
		switch in.Kind {
		case "entry":
			mv.To = &in.WarehouseID
			if err := addStock(ctx, q, in.WarehouseID, in.ItemID, qty); err != nil {
				return err
			}
		case "adjustment":
			mv.To = &in.WarehouseID
			if err := addStock(ctx, q, in.WarehouseID, in.ItemID, qty); err != nil {
				return err
			}
		case "transfer":
			if in.ToWarehouseID == nil || *in.ToWarehouseID == in.WarehouseID {
				return ErrSameWarehouse.WithField("body.to_warehouse_id")
			}
			if err := activeWarehouse(ctx, q, *in.ToWarehouseID, "body.to_warehouse_id"); err != nil {
				return err
			}
			mv.From, mv.To = &in.WarehouseID, in.ToWarehouseID
			if err := addStock(ctx, q, in.WarehouseID, in.ItemID, -qty); err != nil {
				return err
			}
			if err := addStock(ctx, q, *in.ToWarehouseID, in.ItemID, qty); err != nil {
				return err
			}
		case "checkout":
			mv.From, mv.Employee = &in.WarehouseID, &person
			if err := addStock(ctx, q, in.WarehouseID, in.ItemID, -qty); err != nil {
				return err
			}
			if err := addCustody(ctx, q, person, in.ItemID, qty); err != nil {
				return err
			}
		case "return":
			mv.To, mv.Employee = &in.WarehouseID, &person
			if err := addCustody(ctx, q, person, in.ItemID, -qty); err != nil {
				return err
			}
			if err := addStock(ctx, q, in.WarehouseID, in.ItemID, qty); err != nil {
				return err
			}
		}
		return record(ctx, q, mv)
	})
}

// ReturnCustody devuelve a una bodega (la central si no se indica) lo que una
// persona tiene de un articulo (web: "recibir devolucion").
func (s *Service) ReturnCustody(ctx context.Context, actor authz.Actor, personID uuid.UUID, in CustodyReturn) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		var have float64
		err := q.QueryRow(ctx, `SELECT quantity::float8 FROM employee_custody WHERE user_id = $1 AND item_id = $2`,
			personID, in.ItemID).Scan(&have)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && have <= 0) {
			return ErrCustodyNotFound
		}
		if err != nil {
			return err
		}
		qty := have
		if in.Qty != nil {
			qty = round3(*in.Qty)
		}
		wh := uuid.Nil
		if in.WarehouseID != nil {
			if err := activeWarehouse(ctx, q, *in.WarehouseID, "body.warehouse_id"); err != nil {
				return err
			}
			wh = *in.WarehouseID
		} else if wh, err = DefaultWarehouse(ctx, q, nil); err != nil {
			return err
		}
		return ReturnFromCustody(ctx, q, actor.UserID, personID, in.ItemID, qty, wh, nil, nil)
	})
}

type movement struct {
	Type     string
	ItemID   uuid.UUID
	Qty      float64
	From, To *uuid.UUID
	Employee *uuid.UUID
	TaskID   *uuid.UUID
	Note     *string
	By       uuid.UUID
}

func record(ctx context.Context, q database.Querier, m movement) error {
	var by *uuid.UUID
	if m.By != uuid.Nil {
		by = &m.By
	}
	var id uuid.UUID
	if err := q.QueryRow(ctx, `
		INSERT INTO inventory_movements (item_id, type, quantity, from_warehouse_id, to_warehouse_id,
		                                 employee_user_id, task_id, notes, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		m.ItemID, m.Type, m.Qty, m.From, m.To, m.Employee, m.TaskID, m.Note, by).Scan(&id); err != nil {
		return err
	}
	return audit.Record(ctx, q, audit.Entry{
		Actor: by, Action: "inventory.movement", Entity: "inventory_movement", EntityID: &id,
		After: map[string]any{"type": m.Type, "item_id": m.ItemID, "qty": m.Qty, "from": m.From, "to": m.To,
			"employee": m.Employee, "task_id": m.TaskID, "note": m.Note},
	})
}

// addStock suma (o resta) existencia sin dejarla negativa.
func addStock(ctx context.Context, q database.Querier, warehouseID, itemID uuid.UUID, delta float64) error {
	if delta >= 0 {
		_, err := q.Exec(ctx, `
			INSERT INTO inventory_stock (warehouse_id, item_id, quantity) VALUES ($1, $2, $3)
			ON CONFLICT (warehouse_id, item_id) DO UPDATE SET quantity = inventory_stock.quantity + $3, updated_at = now()`,
			warehouseID, itemID, delta)
		return err
	}
	tag, err := q.Exec(ctx, `
		UPDATE inventory_stock SET quantity = quantity + $3, updated_at = now()
		WHERE warehouse_id = $1 AND item_id = $2 AND quantity + $3 >= 0`, warehouseID, itemID, delta)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotEnoughStock.WithField("body.qty")
	}
	return nil
}

// addCustody suma (o resta) a la custodia de la persona sin dejarla negativa.
func addCustody(ctx context.Context, q database.Querier, userID, itemID uuid.UUID, delta float64) error {
	if delta >= 0 {
		_, err := q.Exec(ctx, `
			INSERT INTO employee_custody (user_id, item_id, quantity) VALUES ($1, $2, $3)
			ON CONFLICT (user_id, item_id) DO UPDATE SET quantity = employee_custody.quantity + $3,
			    since = CASE WHEN employee_custody.quantity = 0 THEN now() ELSE employee_custody.since END,
			    updated_at = now()`,
			userID, itemID, delta)
		return err
	}
	tag, err := q.Exec(ctx, `
		UPDATE employee_custody SET quantity = quantity + $3, updated_at = now()
		WHERE user_id = $1 AND item_id = $2 AND quantity + $3 >= 0`, userID, itemID, delta)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotEnoughCustody
	}
	_, err = q.Exec(ctx, `DELETE FROM employee_custody WHERE user_id = $1 AND item_id = $2 AND quantity = 0`, userID, itemID)
	return err
}

func activeItem(ctx context.Context, q database.Querier, id uuid.UUID) error {
	var ok bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM inventory_items WHERE id = $1 AND is_active)`, id).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrUnknownItem.WithField("body.item_id")
	}
	return nil
}

func activeWarehouse(ctx context.Context, q database.Querier, id uuid.UUID, field string) error {
	var ok bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM warehouses WHERE id = $1 AND is_active)`, id).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrUnknownWarehouse.WithField(field)
	}
	return nil
}

// --- Custodia para el cierre de tareas ---------------------------------------------

// Custody es un articulo en custodia (para conciliar al cerrar).
type Custody struct {
	ItemID     uuid.UUID `db:"item_id"`
	Name       string    `db:"name"`
	Unit       string    `db:"unit"`
	Category   string    `db:"category"`
	Returnable bool      `db:"returnable"`
	Quantity   float64   `db:"quantity"`
}

// CustodyOf devuelve la custodia de la persona y la bloquea hasta el fin de
// la transaccion.
func CustodyOf(ctx context.Context, q database.Querier, userID uuid.UUID) ([]Custody, error) {
	return collect[Custody](ctx, q, `
		SELECT c.item_id, i.name, i.unit, i.category, i.returnable, c.quantity::float8 AS quantity
		FROM employee_custody c JOIN inventory_items i ON i.id = c.item_id
		WHERE c.user_id = $1 AND c.quantity > 0 ORDER BY i.name
		FOR UPDATE OF c`, userID)
}

// ReturnFromCustody pasa qty de la custodia de la persona a la bodega.
func ReturnFromCustody(ctx context.Context, q database.Querier, by, userID, itemID uuid.UUID, qty float64,
	warehouseID uuid.UUID, taskID *uuid.UUID, note *string,
) error {
	qty = round3(qty)
	if qty <= 0 {
		return nil
	}
	if err := addCustody(ctx, q, userID, itemID, -qty); err != nil {
		return err
	}
	if err := addStock(ctx, q, warehouseID, itemID, qty); err != nil {
		return err
	}
	return record(ctx, q, movement{Type: "return", ItemID: itemID, Qty: qty, To: &warehouseID, Employee: &userID,
		TaskID: taskID, Note: note, By: by})
}

// ConsumeFromCustody descuenta de la custodia lo gastado en una tarea.
func ConsumeFromCustody(ctx context.Context, q database.Querier, by, userID, itemID uuid.UUID, qty float64, taskID *uuid.UUID) error {
	qty = round3(qty)
	if qty <= 0 {
		return nil
	}
	if err := addCustody(ctx, q, userID, itemID, -qty); err != nil {
		return err
	}
	return record(ctx, q, movement{Type: "consumption", ItemID: itemID, Qty: qty, Employee: &userID, TaskID: taskID, By: by})
}

// WriteOff quita de la custodia lo que falto (queda en la diferencia).
func WriteOff(ctx context.Context, q database.Querier, userID, itemID uuid.UUID, qty float64) error {
	qty = round3(qty)
	if qty <= 0 {
		return nil
	}
	return addCustody(ctx, q, userID, itemID, -qty)
}

// OpenDiscrepancy registra una diferencia y avisa a quienes concilian.
func (s *Service) OpenDiscrepancy(ctx context.Context, q database.Querier, tenantID, by, userID uuid.UUID, c Custody,
	expected, returned float64, taskID *uuid.UUID, taskNumber *int,
) (uuid.UUID, error) {
	var id uuid.UUID
	if err := q.QueryRow(ctx, `
		INSERT INTO inventory_discrepancies (user_id, item_id, task_id, expected_qty, returned_qty)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`, userID, c.ItemID, taskID, round3(expected), round3(returned)).Scan(&id); err != nil {
		return uuid.Nil, err
	}
	if err := audit.Record(ctx, q, audit.Entry{
		Actor: &by, Action: "inventory.discrepancy_opened", Entity: "inventory_discrepancy", EntityID: &id,
		After: map[string]any{"user_id": userID, "item": c.Name, "expected": expected, "returned": returned, "task_id": taskID},
	}); err != nil {
		return uuid.Nil, err
	}
	return id, s.bus.Publish(ctx, q, events.Event{
		Type: EventDiscrepancyOpened, TenantID: tenantID, ActorID: &by,
		Data: DiscrepancyEvent{ID: id, UserID: userID, ItemName: c.Name, Expected: expected, Returned: returned,
			TaskID: taskID, TaskNumber: taskNumber},
	})
}

// --- Diferencias y solicitudes --------------------------------------------------

// SetDiscrepancy justifica o cierra una diferencia.
func (s *Service) SetDiscrepancy(ctx context.Context, actor authz.Actor, id uuid.UUID, in InventoryDecision) error {
	if in.Status != "open" && in.Status != "justified" && in.Status != "closed" {
		return ErrInvalidTransition.WithField("body.status")
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		var cur string
		err := q.QueryRow(ctx, `SELECT status FROM inventory_discrepancies WHERE id = $1 FOR UPDATE`, id).Scan(&cur)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDiscrepancyNotFound
		}
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			UPDATE inventory_discrepancies SET status = $2::text, justification = coalesce($3::text, justification),
			       resolved_by = CASE WHEN $2::text = 'open' THEN NULL ELSE $4::uuid END,
			       resolved_at = CASE WHEN $2::text = 'open' THEN NULL ELSE now() END, updated_at = now()
			WHERE id = $1`, id, in.Status, trimmed(in.Note), actor.UserID); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "inventory.discrepancy_" + in.Status, Entity: "inventory_discrepancy", EntityID: &id,
			Before: map[string]any{"status": cur}, After: map[string]any{"status": in.Status, "note": trimmed(in.Note)},
		})
	})
}

// CreateRequest pide reposicion (app).
func (s *Service) CreateRequest(ctx context.Context, actor authz.Actor, in RestockRequestCreate) (RestockRequest, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (RestockRequest, error) {
		var wh *uuid.UUID
		if in.PropertyID != nil {
			var ok bool
			if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM properties WHERE id = $1 AND deleted_at IS NULL)`,
				*in.PropertyID).Scan(&ok); err != nil {
				return RestockRequest{}, err
			}
			if !ok {
				return RestockRequest{}, ErrUnknownProperty.WithField("body.property_id")
			}
			if id, err := DefaultWarehouse(ctx, q, in.PropertyID); err == nil {
				wh = &id
			}
		}
		var id uuid.UUID
		if err := q.QueryRow(ctx, `
			INSERT INTO restock_requests (property_id, warehouse_id, requested_by, notes) VALUES ($1, $2, $3, $4) RETURNING id`,
			in.PropertyID, wh, actor.UserID, trimmed(in.Note)).Scan(&id); err != nil {
			return RestockRequest{}, err
		}
		for i, l := range in.Items {
			if err := activeItem(ctx, q, l.ItemID); err != nil {
				return RestockRequest{}, ErrUnknownItem.WithField("body.items[" + itoa(i) + "].item_id")
			}
			if _, err := q.Exec(ctx, `
				INSERT INTO restock_request_items (request_id, item_id, quantity) VALUES ($1, $2, $3)
				ON CONFLICT (request_id, item_id) DO UPDATE SET quantity = restock_request_items.quantity + $3`,
				id, l.ItemID, round3(l.Quantity)); err != nil {
				return RestockRequest{}, err
			}
		}
		r, err := requestByID(ctx, q, id)
		if err != nil {
			return RestockRequest{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "inventory.restock_requested", Entity: "restock_request", EntityID: &id,
			PropertyID: in.PropertyID, After: r,
		}); err != nil {
			return RestockRequest{}, err
		}
		return r, s.bus.Publish(ctx, q, events.Event{
			Type: EventRestockRequested, TenantID: actor.TenantID, ActorID: &actor.UserID, Data: RestockEvent{Request: r},
		})
	})
}

// SetRequest aprueba, rechaza o marca surtida una solicitud.
func (s *Service) SetRequest(ctx context.Context, actor authz.Actor, id uuid.UUID, in InventoryDecision) error {
	allowed := map[string][]string{
		"pending":  {"approved", "rejected", "fulfilled"},
		"approved": {"fulfilled", "rejected"},
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		var cur string
		err := q.QueryRow(ctx, `SELECT status FROM restock_requests WHERE id = $1 FOR UPDATE`, id).Scan(&cur)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRequestNotFound
		}
		if err != nil {
			return err
		}
		ok := false
		for _, s := range allowed[cur] {
			ok = ok || s == in.Status
		}
		if !ok {
			if cur != "pending" && cur != "approved" {
				return ErrDecisionNotOpen
			}
			return ErrInvalidTransition.WithField("body.status")
		}
		if _, err := q.Exec(ctx, `
			UPDATE restock_requests SET status = $2::text, notes = coalesce($3::text, notes), decided_by = $4, decided_at = now(),
			       updated_at = now() WHERE id = $1`, id, in.Status, trimmed(in.Note), actor.UserID); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "inventory.restock_" + in.Status, Entity: "restock_request", EntityID: &id,
			Before: map[string]any{"status": cur}, After: map[string]any{"status": in.Status, "note": trimmed(in.Note)},
		})
	})
}

func requestByID(ctx context.Context, q database.Querier, id uuid.UUID) (RestockRequest, error) {
	rows, err := q.Query(ctx, selectRequest+` WHERE r.id = $1`, id)
	if err != nil {
		return RestockRequest{}, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[RestockRequest])
	if errors.Is(err, pgx.ErrNoRows) {
		return RestockRequest{}, ErrRequestNotFound
	}
	return r, err
}

func itoa(i int) string {
	const digits = "0123456789"
	if i < 10 {
		return digits[i : i+1]
	}
	return itoa(i/10) + digits[i%10:i%10+1]
}
