package maintenance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/modules/alerts"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Trabajos del modulo (los registra app.NewJobRegistry). Corren cada hora:
// el recurrente (sin empresa) encola uno por empresa activa, que corre con
// el schema de esa empresa. Ambos son idempotentes.
const (
	JobMaintenanceDue = "maintenance_due"
	JobLowStock       = "low_stock"
)

// HandleJob atiende maintenance_due y low_stock.
func (s *Service) HandleJob(ctx context.Context, job jobs.Running) error {
	if job.TenantID == nil {
		return s.fanOut(ctx, job.Type)
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		var err error
		switch job.Type {
		case JobMaintenanceDue:
			_, err = s.RunDue(ctx, q, *job.TenantID)
		case JobLowStock:
			_, err = s.RunLowStock(ctx, q, *job.TenantID)
		default:
			err = fmt.Errorf("trabajo desconocido %s", job.Type)
		}
		return err
	})
}

// fanOut encola el trabajo para cada empresa activa.
func (s *Service) fanOut(ctx context.Context, jobType string) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		rows, err := q.Query(ctx, `SELECT id FROM shared.tenants WHERE is_active ORDER BY slug`)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := jobs.Enqueue(ctx, q, jobs.Job{
				Type: jobType, TenantID: &id, DedupeKey: jobType + ":" + id.String(), MaxAttempts: 3,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// DueResult es lo que hizo una pasada de maintenance_due.
type DueResult struct {
	Tickets int // incidencias de preventivo creadas
	Overdue int // alertas de vencido nuevas
}

// RunDue genera las incidencias de los preventivos que entraron en su
// anticipacion (next_due_at - lead_days <= hoy) y levanta la alerta de los
// vencidos (fase03.md §3). q opera en el schema de la empresa.
func (s *Service) RunDue(ctx context.Context, q database.Querier, tenantID uuid.UUID) (DueResult, error) {
	var res DueResult
	today, err := s.today(ctx, q)
	if err != nil {
		return res, err
	}
	due, err := listPlans(ctx, q, today, `mp.is_active AND a.is_active
		AND mp.next_due_at - mp.lead_days <= $1::date AND ot.id IS NULL`)
	if err != nil {
		return res, err
	}
	for _, p := range due {
		created, err := s.planTicket(ctx, q, tenantID, today, p)
		if err != nil {
			return res, err
		}
		if created {
			res.Tickets++
		}
	}
	overdue, err := listPlans(ctx, q, today, `mp.is_active AND a.is_active AND mp.next_due_at < $1::date`)
	if err != nil {
		return res, err
	}
	for _, p := range overdue {
		severity := alerts.SeverityMedium
		if addDays(p.NextDueAt, 7) < today {
			severity = alerts.SeverityHigh
		}
		_, created, err := s.alerts.Raise(ctx, q, tenantID, alerts.Raise{
			Type: alerts.TypeMaintenanceOverdue, Severity: severity, EntityType: "maintenance_plan", EntityID: p.ID,
			PropertyID: p.PropertyID,
			Message:    fmt.Sprintf("Preventivo vencido: %s · %s, %s. Tocaba el %s", p.Name, p.AssetName, placeOfPlan(p), shortDate(p.NextDueAt)),
		})
		if err != nil {
			return res, err
		}
		if created {
			res.Overdue++
		}
	}
	return res, nil
}

// planTicket crea la incidencia del preventivo. Si otra pasada ya la creo
// (indice uq_maintenance_tickets_open_plan), no hace nada.
func (s *Service) planTicket(ctx context.Context, q database.Querier, tenantID uuid.UUID, today string, p PlanRead) (bool, error) {
	urgency := "medium"
	if p.NextDueAt < today {
		urgency = "high"
	}
	status := "open"
	if p.ResponsibleID != nil || p.VendorID != nil {
		status = "assigned"
	}
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO maintenance_tickets (property_id, building_id, asset_id, plan_id, title, description, urgency,
		                                 status, assigned_to, vendor_id)
		SELECT a.property_id, CASE WHEN a.property_id IS NULL THEN a.building_id END, a.id, $1, $2, $3, $4, $5, $6, $7
		FROM assets a WHERE a.id = $8
		ON CONFLICT (plan_id) WHERE plan_id IS NOT NULL AND status IN ('open', 'assigned', 'in_progress') DO NOTHING
		RETURNING id`,
		p.ID, "Preventivo: "+p.Name,
		fmt.Sprintf("Toca el %s (cada %d días).", p.NextDueAt, p.FrequencyDays),
		urgency, status, p.ResponsibleID, p.VendorID, p.AssetID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	t, err := s.created(ctx, q, tenantID, nil, id, map[string]any{"plan": p.Name, "due": p.NextDueAt})
	if err != nil {
		return false, err
	}
	if status == "assigned" {
		if err := addEvent(ctx, q, id, "assigned", nil, map[string]any{
			"assignee_name": t.AssigneeName, "vendor_name": t.VendorName,
		}); err != nil {
			return false, err
		}
		if p.ResponsibleID != nil {
			// Actor del sistema: UserID vacio.
			if err := s.linkTask(ctx, q, authz.Actor{TenantID: tenantID}, &t); err != nil {
				return false, err
			}
		}
	}
	return true, s.bus.Publish(ctx, q, events.Event{
		Type: EventMaintenanceDue, TenantID: tenantID, Data: PlanDueEvent{Plan: p, Ticket: t},
	})
}

var monthsES = [...]string{"ene", "feb", "mar", "abr", "may", "jun", "jul", "ago", "sep", "oct", "nov", "dic"}

// shortDate convierte "2026-11-16" en "16 nov" para los mensajes que se leen
// en la bandeja y en Inicio; si no es una fecha, la deja igual.
func shortDate(day string) string {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day
	}
	return fmt.Sprintf("%d %s", t.Day(), monthsES[t.Month()-1])
}

func placeOfPlan(p PlanRead) string {
	switch {
	case p.PropertyCode != nil:
		return *p.PropertyCode
	case p.BuildingName != nil:
		return *p.BuildingName
	default:
		return "sin ubicación"
	}
}

// lowItem es un articulo bajo minimo en una bodega.
type lowItem struct {
	ItemID    uuid.UUID
	Name      string
	Unit      string
	Min       float64
	Warehouse string
	Quantity  float64
}

// RunLowStock levanta una alerta low_stock por articulo con alguna bodega bajo
// su minimo y resuelve las de los que ya se repusieron. Usa las tablas de
// inventario de Persona B (fase02.md §2: inventory_items.min_stock,
// inventory_stock.quantity, warehouses.name); mientras no existan, no hace
// nada. Devuelve cuantas alertas nuevas levanto.
func (s *Service) RunLowStock(ctx context.Context, q database.Querier, tenantID uuid.UUID) (int, error) {
	low, err := lowItems(ctx, q)
	if err != nil || low == nil {
		return 0, err
	}
	created, items := 0, []uuid.UUID{}
	for i := 0; i < len(low); {
		j := i
		for j < len(low) && low[j].ItemID == low[i].ItemID {
			j++
		}
		message, severity := lowMessage(low[i:j])
		_, isNew, err := s.alerts.Raise(ctx, q, tenantID, alerts.Raise{
			Type: alerts.TypeLowStock, Severity: severity, EntityType: "inventory_item", EntityID: low[i].ItemID,
			Message: message,
		})
		if err != nil {
			return created, err
		}
		if isNew {
			created++
		}
		items = append(items, low[i].ItemID)
		i = j
	}
	_, err = q.Exec(ctx, `
		UPDATE alerts SET status = 'resolved', resolved_at = now(), updated_at = now()
		WHERE type = 'low_stock' AND status <> 'resolved' AND NOT (entity_id = ANY($1::uuid[]))`, items)
	return created, err
}

// lowItems lee los articulos bajo minimo por bodega, el peor primero. nil si
// las tablas de inventario aun no existen.
func lowItems(ctx context.Context, q database.Querier) ([]lowItem, error) {
	var ready bool
	if err := q.QueryRow(ctx, `
		SELECT to_regclass('inventory_stock') IS NOT NULL AND to_regclass('inventory_items') IS NOT NULL
		   AND to_regclass('warehouses') IS NOT NULL`).Scan(&ready); err != nil || !ready {
		return nil, err
	}
	rows, err := q.Query(ctx, `
		SELECT i.id, i.name, coalesce(i.unit, ''), i.min_stock::float8, w.name, s.quantity::float8
		FROM inventory_stock s
		JOIN inventory_items i ON i.id = s.item_id
		JOIN warehouses w ON w.id = s.warehouse_id
		WHERE i.is_active AND w.is_active AND i.min_stock > 0 AND s.quantity < i.min_stock
		ORDER BY i.id, s.quantity / i.min_stock, w.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	low := []lowItem{}
	for rows.Next() {
		var l lowItem
		if err := rows.Scan(&l.ItemID, &l.Name, &l.Unit, &l.Min, &l.Warehouse, &l.Quantity); err != nil {
			return nil, err
		}
		low = append(low, l)
	}
	return low, rows.Err()
}

// lowMessage arma el mensaje de un articulo (sus bodegas bajo minimo, la peor
// primero) y su severidad: alta si alguna se quedo en cero.
func lowMessage(group []lowItem) (message, severity string) {
	worst := group[0]
	message = fmt.Sprintf("%s: %s de %s %s en %s", worst.Name, num(worst.Quantity), num(worst.Min), worst.Unit, worst.Warehouse)
	if extra := len(group) - 1; extra > 0 {
		message += fmt.Sprintf(" y %d bodega%s más", extra, map[bool]string{true: "s", false: ""}[extra > 1])
	}
	severity = alerts.SeverityMedium
	if worst.Quantity <= 0 {
		severity = alerts.SeverityHigh
	}
	return strings.Join(strings.Fields(message), " "), severity
}

func num(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", f), "0"), ".")
}
