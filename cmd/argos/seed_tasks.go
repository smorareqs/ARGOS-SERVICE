package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/app"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/inventory"
	"github.com/smorareqs/argos-service/internal/modules/tasks"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
)

// Semilla de la fase 2 de Persona B: inventario (bodega central, articulos,
// existencias, una custodia y una solicitud) y el itinerario de dos semanas
// alrededor de hoy (pasadas validadas, hoy a medias, futuras pendientes).

type demoItem struct {
	sku, name, category, unit string
	min, stock                float64
}

var demoInventory = []demoItem{
	{"TOA-BAN", "Toalla de baño", "linens", "unidad", 20, 64},
	{"TOA-MAN", "Toalla de manos", "linens", "unidad", 20, 48},
	{"SAB-QUE", "Juego de sábanas queen", "linens", "juego", 10, 26},
	{"JAB-TOC", "Jabón de tocador", "amenities", "unidad", 50, 180},
	{"SHA-30", "Shampoo 30 ml", "amenities", "unidad", 40, 12},
	{"PAP-HIG", "Papel higiénico", "amenities", "rollo", 60, 140},
	{"DES-MUL", "Desinfectante multiusos", "cleaning", "litro", 8, 15},
	{"BOL-BAS", "Bolsas de basura", "cleaning", "unidad", 100, 240},
	{"CAF-MOL", "Café molido", "kitchen", "bolsa", 10, 18},
	{"LLA-MAE", "Llave maestra de bodega", "keys", "unidad", 0, 2},
}

// seedTasks siembra inventario e itinerario en cada empresa (idempotente: no
// hace nada si la empresa ya tiene bodegas).
func seedTasks(ctx context.Context, services app.Services, ids map[string]tenants.Tenant) error {
	for slug, t := range ids {
		ops, ok := demoOperations[slug]
		if !ok {
			continue
		}
		admin, err := services.Auth.UserByEmail(ctx, ops.admin)
		if err != nil {
			return err
		}
		s := taskSeeder{
			services: services, ctx: database.WithTenantSchema(ctx, t.SchemaName), tenant: t,
			admin: authz.Actor{UserID: admin.ID, TenantID: t.ID, Permissions: allPermissions()},
		}
		if err := s.run(); err != nil {
			return fmt.Errorf("tareas e inventario de %s: %w", slug, err)
		}
	}
	fmt.Println("Inventario e itinerario de demostracion listos.")
	return nil
}

type taskSeeder struct {
	services app.Services
	ctx      context.Context
	tenant   tenants.Tenant
	admin    authz.Actor
}

func (s *taskSeeder) run() error {
	existing, err := s.services.Inventory.Warehouses(s.ctx)
	if err != nil || len(existing) > 0 {
		return err
	}
	wh, err := s.services.Inventory.CreateWarehouse(s.ctx, s.admin, inventory.WarehouseCreate{Name: "Bodega central"})
	if err != nil {
		return err
	}
	items := map[string]uuid.UUID{}
	for _, it := range demoInventory {
		created, err := s.services.Inventory.CreateItem(s.ctx, s.admin, inventory.InventoryItemCreate{
			SKU: it.sku, Name: it.name, Category: it.category, Unit: it.unit, Min: it.min,
		})
		if err != nil {
			return err
		}
		items[it.sku] = created.ID
		if err := s.services.Inventory.Move(s.ctx, s.admin, inventory.StockChange{
			Kind: "entry", ItemID: created.ID, WarehouseID: wh.ID, Qty: it.stock, Note: ptr("Inventario inicial"),
		}); err != nil {
			return err
		}
	}
	pairs, err := s.assignments()
	if err != nil {
		return err
	}
	// La primera persona de limpieza lleva blancos y amenidades en custodia y
	// pidio reposicion de shampoo.
	for _, p := range pairs {
		if !p.cleaner {
			continue
		}
		for sku, qty := range map[string]float64{"TOA-BAN": 6, "TOA-MAN": 4, "JAB-TOC": 10, "SHA-30": 6} {
			if err := s.services.Inventory.Move(s.ctx, s.admin, inventory.StockChange{
				Kind: "checkout", ItemID: items[sku], WarehouseID: wh.ID, Qty: qty, UserID: &p.user,
			}); err != nil {
				return err
			}
		}
		actor := authz.Actor{UserID: p.user, TenantID: s.tenant.ID, Permissions: map[authz.Permission]bool{authz.InventoryCheckout: true}}
		if _, err := s.services.Inventory.CreateRequest(s.ctx, actor, inventory.RestockRequestCreate{
			PropertyID: &p.property, Items: []inventory.RestockLine{{ItemID: items["SHA-30"], Quantity: 24}},
			Note: ptr("Ya casi no queda shampoo en la bodega."),
		}); err != nil {
			return err
		}
		break
	}
	if err := s.itinerary(pairs); err != nil {
		return err
	}
	// Las limpiezas de salida de reservas que ya pasaron quedan validadas.
	return s.services.Tasks.DB().InTx(s.ctx, func(q database.Querier) error {
		_, err := q.Exec(s.ctx, `
			UPDATE tasks SET status = 'validated',
			       started_at = scheduled_date + coalesce(window_start, '11:00'::time) + interval '6 hours',
			       finished_at = scheduled_date + coalesce(window_start, '11:00'::time) + interval '7 hours 40 minutes',
			       validated_at = scheduled_date + coalesce(window_start, '11:00'::time) + interval '9 hours',
			       validated_by = $1, updated_at = now()
			WHERE reservation_id IS NOT NULL AND status = 'pending' AND assigned_to IS NOT NULL
			  AND scheduled_date < (now() AT TIME ZONE 'America/Guatemala')::date`, s.admin.UserID)
		return err
	})
}

type assignment struct {
	user, property uuid.UUID
	cleaner        bool
}

func (s *taskSeeder) assignments() ([]assignment, error) {
	return database.InTxValue(s.ctx, s.services.Tasks.DB(), func(q database.Querier) ([]assignment, error) {
		rows, err := q.Query(s.ctx, `
			SELECT pa.user_id, pa.property_id, r.key = 'cleaner'
			FROM property_assignments pa
			JOIN properties p ON p.id = pa.property_id AND p.deleted_at IS NULL
			JOIN shared.memberships m ON m.user_id = pa.user_id AND m.tenant_id = $1 AND m.status = 'active'
			JOIN shared.roles r ON r.id = m.role_id
			ORDER BY r.key, p.code, pa.user_id`, s.tenant.ID)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, func(r pgx.CollectableRow) (assignment, error) {
			var a assignment
			err := r.Scan(&a.user, &a.property, &a.cleaner)
			return a, err
		})
	})
}

// itinerary programa de 6 dias atras a 6 adelante: cada asignacion tiene una
// tarea cada dos dias (limpieza o inspeccion segun el rol).
func (s *taskSeeder) itinerary(pairs []assignment) error {
	loc, err := time.LoadLocation("America/Guatemala")
	if err != nil {
		loc = time.UTC
	}
	today := time.Now().In(loc)
	day := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	slots := [][2]string{{"09:00", "11:00"}, {"11:30", "14:00"}, {"15:00", "17:00"}}
	cleanKinds := []string{"checkout_cleaning", "deep_cleaning", "touch_up"}
	perDay := map[string]int{}
	for i, p := range pairs {
		for offset := -6; offset <= 6; offset++ {
			if (p.cleaner && (offset+i)%2 != 0) || (!p.cleaner && (offset+i)%4 != 0) {
				continue
			}
			date := day.AddDate(0, 0, offset)
			if date.Weekday() == time.Sunday {
				continue
			}
			key := p.user.String() + date.Format(time.DateOnly)
			slot := slots[perDay[key]%len(slots)]
			perDay[key]++
			kind := "inspection"
			if p.cleaner {
				kind = cleanKinds[(offset+6+i)%len(cleanKinds)]
			}
			user := p.user
			t, err := s.services.Tasks.Create(s.ctx, s.admin, tasks.TaskCreate{
				Type: kind, PropertyID: p.property, Date: date.Format(time.DateOnly), Start: slot[0], End: slot[1],
				AssigneeID: &user, Priority: map[bool]string{true: "high", false: "normal"}[kind == "checkout_cleaning"],
			})
			if err != nil {
				return err
			}
			if err := s.progress(t, offset, slot, date); err != nil {
				return err
			}
		}
	}
	return nil
}

// progress deja las pasadas validadas (ayer finalizadas) y la primera de hoy
// en curso, con su checklist marcado.
func (s *taskSeeder) progress(t tasks.TaskRead, offset int, slot [2]string, date time.Time) error {
	var status string
	switch {
	case offset < -1:
		status = "validated"
	case offset == -1:
		status = "done"
	case offset == 0 && slot[0] == "09:00":
		status = "in_progress"
	default:
		return nil
	}
	start, _ := time.ParseInLocation("2006-01-02 15:04", date.Format(time.DateOnly)+" "+slot[0], date.Location())
	return s.services.Tasks.DB().InTx(s.ctx, func(q database.Querier) error {
		finished := start.Add(95 * time.Minute)
		if _, err := q.Exec(s.ctx, `
			UPDATE tasks SET status = $2::text, started_at = $3, finished_at = CASE WHEN $2::text = 'in_progress' THEN NULL ELSE $4::timestamptz END,
			       validated_at = CASE WHEN $2::text = 'validated' THEN $4::timestamptz + interval '2 hours' END,
			       validated_by = CASE WHEN $2::text = 'validated' THEN $5::uuid END,
			       start_lat = 14.6, start_lng = -90.51, updated_at = now()
			WHERE id = $1`, t.ID, status, start, finished, s.admin.UserID); err != nil {
			return err
		}
		if _, err := q.Exec(s.ctx, `
			UPDATE task_checklist_items SET is_done = true, done_at = $2
			WHERE task_id = $1 AND ($3::text <> 'in_progress' OR position % 2 = 0)`, t.ID, start.Add(30*time.Minute), status); err != nil {
			return err
		}
		if _, err := q.Exec(s.ctx, `UPDATE task_events SET created_at = $2 WHERE task_id = $1 AND type IN ('created', 'assigned')`,
			t.ID, start.Add(-20*time.Hour)); err != nil {
			return err
		}
		events := []struct {
			typ string
			at  time.Time
		}{{"started", start}}
		if status != "in_progress" {
			events = append(events, struct {
				typ string
				at  time.Time
			}{"finished", finished})
		}
		if status == "validated" {
			events = append(events, struct {
				typ string
				at  time.Time
			}{"validated", finished.Add(2 * time.Hour)})
		}
		for _, e := range events {
			actor := t.AssigneeID
			if e.typ == "validated" {
				actor = &s.admin.UserID
			}
			if _, err := q.Exec(s.ctx, `INSERT INTO task_events (task_id, type, actor_user_id, created_at) VALUES ($1, $2, $3, $4)`,
				t.ID, e.typ, actor, e.at); err != nil {
				return err
			}
		}
		return nil
	})
}
