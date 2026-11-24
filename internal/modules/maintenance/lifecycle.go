package maintenance

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Ciclo de una incidencia (fase03.md §3):
//
//	open ──assign──▶ assigned ──start──▶ in_progress ──resolve──▶ resolved ──close──▶ closed
//	  │                 │                                            │
//	  └────reject───────┴──▶ rejected ◀──────── reopen ──────────────┘ (vuelve a open/assigned)
//
// resolve tambien acepta open y assigned (se arreglo en el momento).

// Assign fija el colaborador y el proveedor de la incidencia (el cuerpo es la
// asignacion completa: lo que no viene queda vacio).
func (s *Service) Assign(ctx context.Context, actor authz.Actor, id uuid.UUID, in TicketAssign) (TicketDetail, error) {
	if in.AssigneeID == nil && in.VendorID == nil {
		return TicketDetail{}, ErrNoAssignee.WithField("body.assignee_id")
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TicketDetail, error) {
		cur, err := s.visibleTicket(ctx, q, actor, id)
		if err != nil {
			return TicketDetail{}, err
		}
		if err := checkAssignment(ctx, q, actor, cur, in); err != nil {
			return TicketDetail{}, err
		}
		t, err := s.applyAssignment(ctx, q, actor, cur, in)
		if err != nil {
			return TicketDetail{}, err
		}
		data := map[string]any{"assignee_name": t.AssigneeName, "vendor_name": t.VendorName}
		if n := trim(in.Note); n != "" {
			data["note"] = n
		}
		if err := s.step(ctx, q, actor, t, "assigned", "ticket.assigned", data); err != nil {
			return TicketDetail{}, err
		}
		if err := s.bus.Publish(ctx, q, events.Event{
			Type: EventTicketAssigned, TenantID: actor.TenantID, ActorID: &actor.UserID,
			Data: TicketEvent{Ticket: t, PreviousAssignee: cur.AssigneeID},
		}); err != nil {
			return TicketDetail{}, err
		}
		return s.detail(ctx, q, actor.TenantID, t)
	})
}

// applyAssignment guarda la asignacion (una en curso sigue en curso si no
// cambia el colaborador) y, si hay colaborador nuevo, liga su tarea.
func (s *Service) applyAssignment(ctx context.Context, q database.Querier, actor authz.Actor, cur TicketRead, in TicketAssign) (TicketRead, error) {
	status := "assigned"
	if cur.Status == "in_progress" && sameID(cur.AssigneeID, in.AssigneeID) {
		status = "in_progress"
	}
	if _, err := q.Exec(ctx, `
		UPDATE maintenance_tickets SET assigned_to = $2, vendor_id = $3, status = $4, updated_at = now()
		WHERE id = $1`, cur.ID, in.AssigneeID, in.VendorID, status); err != nil {
		return TicketRead{}, err
	}
	t, err := ticketByID(ctx, q, cur.ID)
	if err != nil {
		return TicketRead{}, err
	}
	if in.AssigneeID != nil && !sameID(in.AssigneeID, cur.AssigneeID) {
		err = s.linkTask(ctx, q, actor, &t)
	}
	return t, err
}

// checkAssignment valida que el ticket siga abierto, que el colaborador
// atienda mantenimiento y que el proveedor exista.
func checkAssignment(ctx context.Context, q database.Querier, actor authz.Actor, cur TicketRead, in TicketAssign) error {
	if !slices.Contains(activeStatuses, cur.Status) {
		return ErrTicketTransition
	}
	if in.AssigneeID != nil {
		ok, err := canExecute(ctx, q, actor.TenantID, *in.AssigneeID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrInvalidAssignee.WithField("body.assignee_id")
		}
	}
	return checkVendor(ctx, q, in.VendorID, "body.vendor_id")
}

// linkTask pide al modulo de tareas la tarea del ticket asignado y guarda su id.
func (s *Service) linkTask(ctx context.Context, q database.Querier, actor authz.Actor, t *TicketRead) error {
	if s.linker == nil {
		return nil
	}
	taskID, err := s.linker(ctx, q, actor, *t)
	if err != nil || taskID == nil {
		return err
	}
	if _, err := q.Exec(ctx, `UPDATE maintenance_tickets SET task_id = $2 WHERE id = $1`, t.ID, *taskID); err != nil {
		return err
	}
	t.TaskID = taskID
	return nil
}

// Start marca la incidencia en curso. Sin asignado, la toma quien la inicia.
func (s *Service) Start(ctx context.Context, actor authz.Actor, id uuid.UUID) (TicketDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TicketDetail, error) {
		cur, err := s.executable(ctx, q, actor, id, "open", "assigned")
		if err != nil {
			return TicketDetail{}, err
		}
		if _, err := q.Exec(ctx, `
			UPDATE maintenance_tickets SET status = 'in_progress',
			       assigned_to = CASE WHEN assigned_to IS NULL AND vendor_id IS NULL THEN $2 ELSE assigned_to END,
			       updated_at = now()
			WHERE id = $1`, id, actor.UserID); err != nil {
			return TicketDetail{}, err
		}
		return s.after(ctx, q, actor, cur.ID, "started", "ticket.started", nil)
	})
}

// Resolve marca la incidencia resuelta con lo que se hizo, el costo y fotos.
// Si nacio de un preventivo, el plan avanza una frecuencia.
func (s *Service) Resolve(ctx context.Context, actor authz.Actor, id uuid.UUID, in TicketResolve) (TicketDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TicketDetail, error) {
		cur, err := s.executable(ctx, q, actor, id, activeStatuses...)
		if err != nil {
			return TicketDetail{}, err
		}
		if n, err := attachFiles(ctx, q, in.PhotoIDs, "ticket_resolution", "maintenance_ticket", id, actor.UserID); err != nil {
			return TicketDetail{}, err
		} else if n != len(in.PhotoIDs) {
			return TicketDetail{}, ErrInvalidFiles.WithField("body.photo_file_ids")
		}
		t, err := s.resolve(ctx, q, actor, cur, in)
		if err != nil {
			return TicketDetail{}, err
		}
		return s.detail(ctx, q, actor.TenantID, t)
	})
}

// ResolveFromTask resuelve la incidencia ligada a la tarea que se acaba de
// cerrar. Lo llama el modulo de tareas (Persona B) con su transaccion.
// Devuelve false si la tarea no tenia incidencia abierta.
func (s *Service) ResolveFromTask(ctx context.Context, q database.Querier, actor authz.Actor, taskID uuid.UUID, resolution string) (bool, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		SELECT id FROM maintenance_tickets WHERE task_id = $1 AND status = ANY($2::text[])`, taskID, activeStatuses).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // sin incidencia abierta: nada que hacer
	}
	if err != nil {
		return false, err
	}
	cur, err := ticketByID(ctx, q, id)
	if err != nil {
		return false, err
	}
	if trim(resolution) == "" {
		resolution = "Resuelta al cerrar la tarea."
	}
	_, err = s.resolve(ctx, q, actor, cur, TicketResolve{Resolution: resolution})
	return err == nil, err
}

func (s *Service) resolve(ctx context.Context, q database.Querier, actor authz.Actor, cur TicketRead, in TicketResolve) (TicketRead, error) {
	if _, err := q.Exec(ctx, `
		UPDATE maintenance_tickets SET status = 'resolved', resolved_at = now(), resolution = $2,
		       cost = coalesce($3, cost),
		       assigned_to = CASE WHEN assigned_to IS NULL AND vendor_id IS NULL THEN $4 ELSE assigned_to END,
		       updated_at = now()
		WHERE id = $1`, cur.ID, trim(in.Resolution), in.Cost, actor.UserID); err != nil {
		return TicketRead{}, err
	}
	if cur.PlanID != nil {
		today, err := s.today(ctx, q)
		if err != nil {
			return TicketRead{}, err
		}
		if err := advancePlan(ctx, q, *cur.PlanID, today); err != nil {
			return TicketRead{}, err
		}
	}
	if in.AssetOperational && cur.AssetID != nil {
		if err := s.assetBackOnline(ctx, q, actor, *cur.AssetID); err != nil {
			return TicketRead{}, err
		}
	}
	data := map[string]any{"resolution": trim(in.Resolution), "photos": len(in.PhotoIDs)}
	if in.Cost != nil {
		data["cost"] = *in.Cost
	}
	t, err := ticketByID(ctx, q, cur.ID)
	if err != nil {
		return TicketRead{}, err
	}
	if err := s.step(ctx, q, actor, t, "resolved", "ticket.resolved", data); err != nil {
		return TicketRead{}, err
	}
	return t, s.bus.Publish(ctx, q, events.Event{
		Type: EventTicketResolved, TenantID: actor.TenantID, ActorID: &actor.UserID, Data: TicketEvent{Ticket: t},
	})
}

// assetBackOnline deja el activo en operational y resuelve su alerta.
func (s *Service) assetBackOnline(ctx context.Context, q database.Querier, actor authz.Actor, assetID uuid.UUID) error {
	tag, err := q.Exec(ctx, `
		UPDATE assets SET status = 'operational', updated_at = now() WHERE id = $1 AND status <> 'operational'`, assetID)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	a, err := assetByID(ctx, q, assetID)
	if err != nil {
		return err
	}
	if err := s.syncAssetAlert(ctx, q, actor, a); err != nil {
		return err
	}
	return audit.Record(ctx, q, audit.Entry{
		Actor: &actor.UserID, Action: "asset.status_changed", Entity: "asset", EntityID: &assetID, PropertyID: a.PropertyID,
		After: map[string]any{"status": "operational"},
	})
}

// Close da por verificada una incidencia resuelta.
func (s *Service) Close(ctx context.Context, actor authz.Actor, id uuid.UUID, in TicketReason) (TicketDetail, error) {
	return s.manage(ctx, actor, id, []string{"resolved"}, false, in.Reason, "closed", `
		UPDATE maintenance_tickets SET status = 'closed', closed_at = now(), updated_at = now() WHERE id = $1`)
}

// Reject descarta una incidencia (duplicada, no es falla). Pide motivo.
func (s *Service) Reject(ctx context.Context, actor authz.Actor, id uuid.UUID, in TicketReason) (TicketDetail, error) {
	return s.manage(ctx, actor, id, []string{"open", "assigned"}, true, in.Reason, "rejected", `
		UPDATE maintenance_tickets SET status = 'rejected', updated_at = now() WHERE id = $1`)
}

// Reopen devuelve a abierta (o asignada) una incidencia resuelta o rechazada. Pide motivo.
func (s *Service) Reopen(ctx context.Context, actor authz.Actor, id uuid.UUID, in TicketReason) (TicketDetail, error) {
	return s.manage(ctx, actor, id, []string{"resolved", "rejected"}, true, in.Reason, "reopened", `
		UPDATE maintenance_tickets
		SET status = CASE WHEN assigned_to IS NULL AND vendor_id IS NULL THEN 'open' ELSE 'assigned' END,
		    resolved_at = NULL, resolution = NULL, updated_at = now()
		WHERE id = $1`)
}

// manage aplica una transicion de administracion con nota o motivo.
func (s *Service) manage(ctx context.Context, actor authz.Actor, id uuid.UUID, from []string, needReason bool, reason, kind, update string) (TicketDetail, error) {
	reason = trim(reason)
	if needReason && reason == "" {
		return TicketDetail{}, ErrReasonRequired.WithField("body.reason")
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TicketDetail, error) {
		cur, err := s.visibleTicket(ctx, q, actor, id)
		if err != nil {
			return TicketDetail{}, err
		}
		if !slices.Contains(from, cur.Status) {
			return TicketDetail{}, ErrTicketTransition
		}
		if _, err := q.Exec(ctx, update, id); err != nil {
			if database.IsUniqueViolation(err) {
				return TicketDetail{}, ErrPlanTicketOpen
			}
			return TicketDetail{}, err
		}
		var data map[string]any
		if reason != "" {
			data = map[string]any{"reason": reason}
		}
		return s.after(ctx, q, actor, id, kind, "ticket."+kind, data)
	})
}

// executable devuelve el ticket si esta en uno de los estados y el actor puede
// trabajarlo: es el asignado, no hay asignado o administra mantenimiento.
func (s *Service) executable(ctx context.Context, q database.Querier, actor authz.Actor, id uuid.UUID, from ...string) (TicketRead, error) {
	cur, err := s.visibleTicket(ctx, q, actor, id)
	if err != nil {
		return TicketRead{}, err
	}
	if !slices.Contains(from, cur.Status) {
		return TicketRead{}, ErrTicketTransition
	}
	if cur.AssigneeID != nil && *cur.AssigneeID != actor.UserID && !manager(actor) {
		return TicketRead{}, ErrNotYourTicket
	}
	return cur, nil
}

// after relee el ticket, registra el paso y devuelve el detalle.
func (s *Service) after(ctx context.Context, q database.Querier, actor authz.Actor, id uuid.UUID, kind, action string, data map[string]any) (TicketDetail, error) {
	t, err := ticketByID(ctx, q, id)
	if err != nil {
		return TicketDetail{}, err
	}
	if err := s.step(ctx, q, actor, t, kind, action, data); err != nil {
		return TicketDetail{}, err
	}
	return s.detail(ctx, q, actor.TenantID, t)
}

// step agrega el evento a la linea de tiempo y a la bitacora.
func (s *Service) step(ctx context.Context, q database.Querier, actor authz.Actor, t TicketRead, kind, action string, data map[string]any) error {
	if err := addEvent(ctx, q, t.ID, kind, &actor.UserID, data); err != nil {
		return err
	}
	after := map[string]any{"number": t.Number, "status": t.Status}
	for k, v := range data {
		after[k] = v
	}
	return audit.Record(ctx, q, audit.Entry{
		Actor: &actor.UserID, Action: action, Entity: "maintenance_ticket", EntityID: &t.ID, PropertyID: t.PropertyID,
		After: after,
	})
}
