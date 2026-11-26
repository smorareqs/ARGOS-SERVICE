package tasks

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/dashboard"
	"github.com/smorareqs/argos-service/internal/modules/maintenance"
	"github.com/smorareqs/argos-service/internal/modules/reservations"
)

// AccessGate (properties.SetAccessGate): limpieza y mantenimiento ven el
// acceso de la propiedad solo el dia que tienen una tarea viva ahi.
func (s *Service) AccessGate(ctx context.Context, q database.Querier, actor authz.Actor, propertyID uuid.UUID) (bool, error) {
	today, err := s.today(ctx, q)
	if err != nil {
		return false, err
	}
	var ok bool
	err = q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM tasks WHERE property_id = $1 AND assigned_to = $2 AND scheduled_date = $3
		               AND status IN ('pending', 'in_progress', 'returned', 'done'))`, propertyID, actor.UserID, today).Scan(&ok)
	return ok, err
}

// LinkTicket (maintenance.SetTaskLinker) crea o reasigna la tarea de
// mantenimiento de una incidencia asignada a un colaborador. Un ticket de un
// activo del edificio (sin propiedad) no genera tarea.
func (s *Service) LinkTicket(ctx context.Context, q database.Querier, actor authz.Actor, t maintenance.TicketRead) (*uuid.UUID, error) {
	if t.PropertyID == nil || t.AssigneeID == nil {
		return nil, nil
	}
	if t.TaskID != nil {
		status, prev, err := ticketTask(ctx, q, *t.TaskID)
		if err != nil {
			return nil, err
		}
		if status == "pending" || status == "in_progress" || status == "returned" {
			if !sameID(prev, t.AssigneeID) {
				if _, err := q.Exec(ctx, `UPDATE tasks SET assigned_to = $2, assigned_by = $3, updated_at = now() WHERE id = $1`,
					*t.TaskID, *t.AssigneeID, actor.UserID); err != nil {
					return nil, err
				}
				if err := addEvent(ctx, q, *t.TaskID, "reassigned", &actor.UserID, map[string]any{
					"assignee_id": t.AssigneeID, "assignee_name": userName(ctx, q, t.AssigneeID), "previous": prev,
				}); err != nil {
					return nil, err
				}
				task, err := s.byID(ctx, q, *t.TaskID)
				if err != nil {
					return nil, err
				}
				if err := s.bus.Publish(ctx, q, events.Event{Type: EventAssigned, TenantID: actor.TenantID, ActorID: &actor.UserID,
					Data: TaskEvent{Task: task, PreviousAssignee: prev}}); err != nil {
					return nil, err
				}
			}
			return t.TaskID, nil
		}
	}
	today, err := s.today(ctx, q)
	if err != nil {
		return nil, err
	}
	priority := "normal"
	if t.Urgency == "high" || t.Urgency == "critical" {
		priority = "high"
	}
	title := t.Title
	// Quien asigna ya valido que la persona puede atender incidencias; la
	// tarea no exige la asignacion de la propiedad (mantenimiento viaja).
	var id uuid.UUID
	if err := q.QueryRow(ctx, `
		INSERT INTO tasks (property_id, type, kind, priority, scheduled_date, window_start, window_end, assigned_to,
		                   assigned_by, created_by, maintenance_ticket_id, notes)
		VALUES ($1, 'maintenance', 'maintenance', $2, $3, '08:00', '18:00', $4, $5, $5, $6, $7) RETURNING id`,
		*t.PropertyID, priority, today, *t.AssigneeID, actor.UserID, t.ID, title).Scan(&id); err != nil {
		return nil, err
	}
	if err := addEvent(ctx, q, id, "created", &actor.UserID, map[string]any{"source": "ticket", "ticket_number": t.Number}); err != nil {
		return nil, err
	}
	if err := addEvent(ctx, q, id, "assigned", &actor.UserID, map[string]any{
		"assignee_id": t.AssigneeID, "assignee_name": userName(ctx, q, t.AssigneeID),
	}); err != nil {
		return nil, err
	}
	task, err := s.byID(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if err := s.bus.Publish(ctx, q, events.Event{Type: EventAssigned, TenantID: actor.TenantID, ActorID: &actor.UserID,
		Data: TaskEvent{Task: task}}); err != nil {
		return nil, err
	}
	return &id, nil
}

// TodayBlock (dashboard.SetTasksSource) arma el bloque de limpiezas del Inicio.
func (s *Service) TodayBlock(ctx context.Context, q database.Querier, actor authz.Actor, scope authz.PropertyScope, today string) (dashboard.Tasks, error) {
	all, ids := scope.SQLArgs()
	out := dashboard.Tasks{Available: true, Ranking: []dashboard.RankRow{}, Status: map[uuid.UUID]string{}}
	rows, err := q.Query(ctx, `
		SELECT t.property_id, t.status, t.is_late, t.assigned_to, coalesce(u.full_name, '')
		FROM tasks t LEFT JOIN shared.users u ON u.id = t.assigned_to
		WHERE t.scheduled_date = $3 AND t.status <> 'cancelled' AND ($1::bool OR t.property_id = ANY($2::uuid[]))`,
		all, ids, today)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	rank := map[uuid.UUID]*dashboard.RankRow{}
	order := []uuid.UUID{}
	field := map[uuid.UUID]bool{}
	worst := map[uuid.UUID]int{}
	weight := map[string]int{"ready": 1, "pending": 2, "cleaning": 3, "late": 4}
	for rows.Next() {
		var pid uuid.UUID
		var status, name string
		var late bool
		var who *uuid.UUID
		if err := rows.Scan(&pid, &status, &late, &who, &name); err != nil {
			return out, err
		}
		out.Total++
		done := status == "done" || status == "validated"
		switch {
		case done:
			out.Done++
		case status == "in_progress":
			out.InProcess++
		default:
			out.Pending++
		}
		if late && !done {
			out.Late++
		}
		state := "pending"
		switch {
		case late && !done:
			state = "late"
		case status == "in_progress":
			state = "cleaning"
		case done:
			state = "ready"
		}
		if weight[state] > worst[pid] {
			worst[pid] = weight[state]
			out.Status[pid] = state
		}
		if who != nil {
			if status == "in_progress" {
				field[*who] = true
			}
			r, ok := rank[*who]
			if !ok {
				r = &dashboard.RankRow{UserID: *who, FullName: name}
				rank[*who] = r
				order = append(order, *who)
			}
			r.Total++
			if done {
				r.Done++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.InField = len(field)
	for _, id := range order {
		out.Ranking = append(out.Ranking, *rank[id])
	}
	sortRanking(out.Ranking)
	if len(out.Ranking) > 5 {
		out.Ranking = out.Ranking[:5]
	}
	return out, nil
}

func sortRanking(r []dashboard.RankRow) {
	for i := 1; i < len(r); i++ {
		for j := i; j > 0 && better(r[j], r[j-1]); j-- {
			r[j], r[j-1] = r[j-1], r[j]
		}
	}
}

func better(a, b dashboard.RankRow) bool {
	if a.Done != b.Done {
		return a.Done > b.Done
	}
	return a.FullName < b.FullName
}

// --- Reservas ------------------------------------------------------------------------

// Subscribe engancha las limpiezas de salida a los eventos de reservas.
func (s *Service) Subscribe(bus *events.Bus) {
	bus.Subscribe(reservations.EventConfirmed, s.onReservationConfirmed)
	bus.Subscribe(reservations.EventUpdated, s.onReservationUpdated)
	bus.Subscribe(reservations.EventCancelled, s.onReservationCancelled)
}

func reservationOf(e events.Event) (reservations.ReservationRead, bool) {
	switch d := e.Data.(type) {
	case reservations.ReservationEvent:
		return d.Reservation, true
	case *reservations.ReservationEvent:
		return d.Reservation, d != nil
	}
	return reservations.ReservationRead{}, false
}

func (s *Service) onReservationConfirmed(ctx context.Context, q database.Querier, e events.Event) error {
	r, ok := reservationOf(e)
	if !ok {
		return nil
	}
	_, err := s.checkoutCleaning(ctx, q, e.TenantID, e.ActorID, r.ID)
	return err
}

// onReservationUpdated mueve la limpieza de salida pendiente a la nueva fecha
// y hora de salida.
func (s *Service) onReservationUpdated(ctx context.Context, q database.Querier, e events.Event) error {
	r, ok := reservationOf(e)
	if !ok {
		return nil
	}
	rows, err := q.Query(ctx, `
		SELECT id, to_char(scheduled_date, 'YYYY-MM-DD'), to_char(window_start, 'HH24:MI') FROM tasks
		WHERE reservation_id = $1 AND kind = 'checkout_cleaning' AND status IN ('pending', 'returned')`, r.ID)
	if err != nil {
		return err
	}
	type row struct {
		ID    uuid.UUID
		Date  string
		Start *string
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
	if err != nil {
		return err
	}
	if len(list) == 0 && (r.Status == "confirmed" || r.Status == "checked_in") {
		_, err := s.checkoutCleaning(ctx, q, e.TenantID, e.ActorID, r.ID)
		return err
	}
	for _, t := range list {
		start := ""
		if t.Start != nil {
			start = *t.Start
		}
		if t.Date == r.CheckOutDate && start == r.CheckOutTime {
			continue
		}
		if _, err := q.Exec(ctx, `
			UPDATE tasks SET scheduled_date = $2, window_start = $3::time,
			       window_end = CASE WHEN $3::time >= '19:59'::time THEN '23:59'::time ELSE $3::time + interval '4 hours' END,
			       is_late = false, updated_at = now()
			WHERE id = $1`, t.ID, r.CheckOutDate, r.CheckOutTime); err != nil {
			return err
		}
		task, err := s.byID(ctx, q, t.ID)
		if err != nil {
			return err
		}
		note := "Cambió la salida de la reserva " + r.Code
		if err := addEvent(ctx, q, t.ID, "rescheduled", e.ActorID, map[string]any{
			"date": task.Date, "start": task.Start, "end": task.End, "note": note,
		}); err != nil {
			return err
		}
		if err := s.bus.Publish(ctx, q, events.Event{Type: EventRescheduled, TenantID: e.TenantID, ActorID: e.ActorID,
			Data: TaskEvent{Task: task, Note: &note}}); err != nil {
			return err
		}
	}
	return nil
}

// onReservationCancelled cancela la limpieza de salida que aun no empieza.
func (s *Service) onReservationCancelled(ctx context.Context, q database.Querier, e events.Event) error {
	r, ok := reservationOf(e)
	if !ok {
		return nil
	}
	rows, err := q.Query(ctx, `
		UPDATE tasks SET status = 'cancelled', updated_at = now()
		WHERE reservation_id = $1 AND kind = 'checkout_cleaning' AND status = 'pending' RETURNING id`, r.ID)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range ids {
		note := fmt.Sprintf("Se canceló la reserva %s", r.Code)
		if err := addEvent(ctx, q, id, "cancelled", e.ActorID, map[string]any{"note": note}); err != nil {
			return err
		}
		task, err := s.byID(ctx, q, id)
		if err != nil {
			return err
		}
		if err := s.bus.Publish(ctx, q, events.Event{Type: EventCancelled, TenantID: e.TenantID, ActorID: e.ActorID,
			Data: TaskEvent{Task: task, Note: &note}}); err != nil {
			return err
		}
	}
	return nil
}
