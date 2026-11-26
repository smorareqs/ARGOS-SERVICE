package tasks

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/inventory"
)

// Ejecucion de la tarea en la app (B3-B7): iniciar con GPS, marcar el
// checklist, registrar insumos y cerrar con la conciliacion de custodia.

// executor exige que el actor sea el asignado (o quien asigna). Una tarea sin
// asignar la toma quien la inicia.
func executor(actor authz.Actor, t TaskRead) error {
	if t.AssigneeID == nil || *t.AssigneeID == actor.UserID || actor.Permissions[authz.TasksAssign] {
		return nil
	}
	return ErrNotYourTask
}

// Start inicia la tarea: guarda el GPS y copia el checklist si falta.
func (s *Service) Start(ctx context.Context, actor authz.Actor, id uuid.UUID, in TaskStart) (TaskDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TaskDetail, error) {
		cur, err := s.lockTask(ctx, q, actor, id)
		if err != nil {
			return TaskDetail{}, err
		}
		if err := executor(actor, cur); err != nil {
			return TaskDetail{}, err
		}
		if cur.Status != "pending" && cur.Status != "returned" {
			return TaskDetail{}, ErrTransition
		}
		if _, err := q.Exec(ctx, `
			UPDATE tasks SET status = 'in_progress', started_at = coalesce(started_at, now()),
			       start_lat = coalesce(start_lat, $2), start_lng = coalesce(start_lng, $3),
			       assigned_to = coalesce(assigned_to, $4), updated_at = now()
			WHERE id = $1`, id, in.Lat, in.Lng, actor.UserID); err != nil {
			return TaskDetail{}, err
		}
		if kinds[cur.Type] == "cleaning" || cur.Type == "inspection" {
			if err := copyChecklist(ctx, q, id, cur.PropertyID); err != nil {
				return TaskDetail{}, err
			}
		}
		data := map[string]any{"lat": in.Lat, "lng": in.Lng}
		if cur.Status == "returned" {
			data["note"] = "Retomada después de la devolución"
		}
		if err := addEvent(ctx, q, id, "started", &actor.UserID, data); err != nil {
			return TaskDetail{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "task.started", Entity: "task", EntityID: &id, PropertyID: &cur.PropertyID,
			Before: map[string]any{"status": cur.Status}, After: map[string]any{"status": "in_progress", "lat": in.Lat, "lng": in.Lng},
		}); err != nil {
			return TaskDetail{}, err
		}
		t, err := s.byID(ctx, q, id)
		if err != nil {
			return TaskDetail{}, err
		}
		return s.detail(ctx, q, actor, t)
	})
}

// UpdateItem marca o desmarca un punto del checklist y guarda su nota.
func (s *Service) UpdateItem(ctx context.Context, actor authz.Actor, id, itemID uuid.UUID, in TaskItemUpdate) (TaskChecklistItem, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TaskChecklistItem, error) {
		cur, err := s.lockTask(ctx, q, actor, id)
		if err != nil {
			return TaskChecklistItem{}, err
		}
		if err := executor(actor, cur); err != nil {
			return TaskChecklistItem{}, err
		}
		if cur.Status != "in_progress" {
			return TaskChecklistItem{}, ErrTransition
		}
		var notes *string
		if in.Notes.Sent && !in.Notes.Null {
			notes = trimmed(&in.Notes.Value)
		}
		tag, err := q.Exec(ctx, `
			UPDATE task_checklist_items SET
			       is_done = coalesce($3, is_done),
			       done_at = CASE WHEN $3 IS NULL THEN done_at WHEN $3 THEN coalesce(done_at, now()) ELSE NULL END,
			       notes = CASE WHEN $4 THEN $5 ELSE notes END, updated_at = now()
			WHERE id = $2 AND task_id = $1`, id, itemID, in.IsDone, in.Notes.Sent, notes)
		if err != nil {
			return TaskChecklistItem{}, err
		}
		if tag.RowsAffected() == 0 {
			return TaskChecklistItem{}, ErrItemNotFound
		}
		if _, err := q.Exec(ctx, `UPDATE tasks SET updated_at = now() WHERE id = $1`, id); err != nil {
			return TaskChecklistItem{}, err
		}
		t, err := s.byID(ctx, q, id)
		if err != nil {
			return TaskChecklistItem{}, err
		}
		d, err := s.detail(ctx, q, actor, t)
		if err != nil {
			return TaskChecklistItem{}, err
		}
		for _, it := range d.Items {
			if it.ID == itemID {
				return it, nil
			}
		}
		return TaskChecklistItem{}, ErrItemNotFound
	})
}

// AddConsumptions registra insumos usados de la custodia propia.
func (s *Service) AddConsumptions(ctx context.Context, actor authz.Actor, id uuid.UUID, in TaskConsumptions) (TaskRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TaskRead, error) {
		cur, err := s.lockTask(ctx, q, actor, id)
		if err != nil {
			return TaskRead{}, err
		}
		if err := executor(actor, cur); err != nil {
			return TaskRead{}, err
		}
		if cur.Status != "in_progress" {
			return TaskRead{}, ErrTransition
		}
		custody, err := inventory.CustodyOf(ctx, q, actor.UserID)
		if err != nil {
			return TaskRead{}, err
		}
		parts := []string{}
		for i, l := range in.Items {
			c, ok := findCustody(custody, l.ItemID)
			if !ok {
				return TaskRead{}, ErrCustodyUnknown.WithField(fmt.Sprintf("body.items[%d].item_id", i))
			}
			if c.Returnable {
				return TaskRead{}, ErrCustodyConsumed.WithField(fmt.Sprintf("body.items[%d].item_id", i))
			}
			if err := inventory.ConsumeFromCustody(ctx, q, actor.UserID, actor.UserID, l.ItemID, l.Quantity, &id); err != nil {
				if errors.Is(err, inventory.ErrNotEnoughCustody) {
					return TaskRead{}, ErrCustodyTooMuch.WithField(fmt.Sprintf("body.items[%d].quantity", i))
				}
				return TaskRead{}, err
			}
			if _, err := q.Exec(ctx, `INSERT INTO task_consumptions (task_id, item_id, quantity, created_by) VALUES ($1, $2, $3, $4)`,
				id, l.ItemID, l.Quantity, actor.UserID); err != nil {
				return TaskRead{}, err
			}
			parts = append(parts, fmt.Sprintf("%s %s", c.Name, num(l.Quantity)))
		}
		if err := addEvent(ctx, q, id, "consumption", &actor.UserID, map[string]any{"summary": summary(parts)}); err != nil {
			return TaskRead{}, err
		}
		if _, err := q.Exec(ctx, `UPDATE tasks SET updated_at = now() WHERE id = $1`, id); err != nil {
			return TaskRead{}, err
		}
		return s.byID(ctx, q, id)
	})
}

func findCustody(list []inventory.Custody, id uuid.UUID) (inventory.Custody, bool) {
	for _, c := range list {
		if c.ItemID == id {
			return c, true
		}
	}
	return inventory.Custody{}, false
}

func num(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

// Complete cierra la tarea (prueba de salida, fase02.md §3) en una sola
// transaccion: checklist completo, fotos obligatorias, GPS de fin y la
// conciliacion de toda la custodia (devuelto + usado + se queda = custodia;
// cada diferencia abre una discrepancia). Si la tarea viene de una
// incidencia, la resuelve.
func (s *Service) Complete(ctx context.Context, actor authz.Actor, id uuid.UUID, in TaskComplete) (TaskDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TaskDetail, error) {
		cur, err := s.lockTask(ctx, q, actor, id)
		if err != nil {
			return TaskDetail{}, err
		}
		if err := executor(actor, cur); err != nil {
			return TaskDetail{}, err
		}
		if cur.Status != "in_progress" {
			return TaskDetail{}, ErrTransition
		}
		if err := checklistReady(ctx, q, id); err != nil {
			return TaskDetail{}, err
		}
		person := actor.UserID
		if cur.AssigneeID != nil {
			person = *cur.AssigneeID
		}
		parts, err := s.reconcile(ctx, q, actor, person, cur, in.Custody)
		if err != nil {
			return TaskDetail{}, err
		}
		notes := trimmed(in.Notes)
		if _, err := q.Exec(ctx, `
			UPDATE tasks SET status = 'done', finished_at = now(), end_lat = $2, end_lng = $3, completion_notes = $4,
			       updated_at = now()
			WHERE id = $1`, id, in.Lat, in.Lng, notes); err != nil {
			return TaskDetail{}, err
		}
		data := map[string]any{"lat": in.Lat, "lng": in.Lng}
		if notes != nil {
			data["note"] = *notes
		}
		if err := addEvent(ctx, q, id, "finished", &actor.UserID, data); err != nil {
			return TaskDetail{}, err
		}
		if len(parts) > 0 {
			if err := addEvent(ctx, q, id, "discrepancy", &actor.UserID, map[string]any{"summary": summary(parts)}); err != nil {
				return TaskDetail{}, err
			}
		}
		if cur.TicketID != nil && s.maintenance != nil {
			resolution := "Resuelta al cerrar la tarea #" + fmt.Sprint(cur.Number) + "."
			if notes != nil {
				resolution = *notes
			}
			if _, err := s.maintenance.ResolveFromTask(ctx, q, actor, id, resolution); err != nil {
				return TaskDetail{}, err
			}
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "task.completed", Entity: "task", EntityID: &id, PropertyID: &cur.PropertyID,
			Before: map[string]any{"status": cur.Status},
			After:  map[string]any{"status": "done", "lat": in.Lat, "lng": in.Lng, "discrepancies": len(parts)},
		}); err != nil {
			return TaskDetail{}, err
		}
		t, err := s.byID(ctx, q, id)
		if err != nil {
			return TaskDetail{}, err
		}
		if err := s.bus.Publish(ctx, q, events.Event{Type: EventCompleted, TenantID: actor.TenantID, ActorID: &actor.UserID,
			Data: TaskEvent{Task: t}}); err != nil {
			return TaskDetail{}, err
		}
		return s.detail(ctx, q, actor, t)
	})
}

// checklistReady verifica puntos marcados y fotos obligatorias.
func checklistReady(ctx context.Context, q database.Querier, taskID uuid.UUID) error {
	var pending, missing int
	err := q.QueryRow(ctx, `
		WITH photos AS (
			SELECT ci.id, ci.is_done, ci.requires_photo, ci.requires_before_after,
			       count(f.id) AS total,
			       count(f.id) FILTER (WHERE f.kind = 'before') AS before,
			       count(f.id) FILTER (WHERE f.kind = 'after') AS after
			FROM task_checklist_items ci
			LEFT JOIN files f ON f.entity_type = 'task_checklist_item' AND f.entity_id = ci.id AND f.module = 'tasks'
			     AND f.status = 'ready' AND f.deleted_at IS NULL
			WHERE ci.task_id = $1 GROUP BY ci.id)
		SELECT count(*) FILTER (WHERE NOT is_done)::int,
		       count(*) FILTER (WHERE (requires_photo AND total = 0) OR (requires_before_after AND (before = 0 OR after = 0)))::int
		FROM photos`, taskID).Scan(&pending, &missing)
	if err != nil {
		return err
	}
	if pending > 0 {
		return ErrChecklistPending
	}
	if missing > 0 {
		return ErrPhotosMissing
	}
	return nil
}

// reconcile concilia la custodia de la persona al cerrar. Devuelve el
// resumen de las diferencias.
func (s *Service) reconcile(ctx context.Context, q database.Querier, actor authz.Actor, person uuid.UUID, t TaskRead,
	declared []TaskCustodyReturn,
) ([]string, error) {
	custody, err := inventory.CustodyOf(ctx, q, person)
	if err != nil {
		return nil, err
	}
	byItem := map[uuid.UUID]TaskCustodyReturn{}
	for i, d := range declared {
		if _, ok := findCustody(custody, d.ItemID); !ok {
			return nil, ErrCustodyUnknown.WithField(fmt.Sprintf("body.custody[%d].item_id", i))
		}
		byItem[d.ItemID] = d
	}
	for _, c := range custody {
		if _, ok := byItem[c.ItemID]; !ok {
			return nil, ErrCustodyMissing.WithField("body.custody")
		}
	}
	if len(custody) == 0 {
		return nil, nil
	}
	warehouse, err := inventory.DefaultWarehouse(ctx, q, &t.PropertyID)
	if err != nil {
		return nil, err
	}
	parts := []string{}
	for i, c := range custody {
		d := byItem[c.ItemID]
		if c.Returnable && d.Consumed > 0 {
			return nil, ErrCustodyConsumed.WithField(fmt.Sprintf("body.custody[%d].consumed", i))
		}
		accounted := d.Returned + d.Consumed + d.Kept
		if accounted-c.Quantity > 0.0005 {
			return nil, ErrCustodyTooMuch.WithField(fmt.Sprintf("body.custody[%d]", i))
		}
		if err := inventory.ReturnFromCustody(ctx, q, actor.UserID, person, c.ItemID, d.Returned, warehouse, &t.ID, nil); err != nil {
			return nil, err
		}
		if d.Consumed > 0 {
			if err := inventory.ConsumeFromCustody(ctx, q, actor.UserID, person, c.ItemID, d.Consumed, &t.ID); err != nil {
				return nil, err
			}
			if _, err := q.Exec(ctx, `INSERT INTO task_consumptions (task_id, item_id, quantity, created_by) VALUES ($1, $2, $3, $4)`,
				t.ID, c.ItemID, d.Consumed, actor.UserID); err != nil {
				return nil, err
			}
		}
		if missing := c.Quantity - accounted; missing > 0.0005 {
			// Lo que falta sale de la custodia y queda en la diferencia.
			if err := inventory.WriteOff(ctx, q, person, c.ItemID, missing); err != nil {
				return nil, err
			}
			expected := c.Quantity - d.Consumed - d.Kept
			number := t.Number
			if _, err := s.inventory.OpenDiscrepancy(ctx, q, actor.TenantID, actor.UserID, person, c, expected, d.Returned,
				&t.ID, &number); err != nil {
				return nil, err
			}
			parts = append(parts, fmt.Sprintf("%s: %s de %s", c.Name, num(d.Returned), num(expected)))
		}
	}
	return parts, nil
}

// ticketTask es la fila minima de una tarea ligada a una incidencia.
func ticketTask(ctx context.Context, q database.Querier, id uuid.UUID) (string, *uuid.UUID, error) {
	var status string
	var assignee *uuid.UUID
	err := q.QueryRow(ctx, `SELECT status, assigned_to FROM tasks WHERE id = $1`, id).Scan(&status, &assignee)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	}
	return status, assignee, err
}
