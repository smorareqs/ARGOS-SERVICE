package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/paging"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/inventory"
)

const selectTask = `
	SELECT t.id, t.number, t.kind, t.status, t.priority, t.property_id, p.code AS property_code, p.name AS property_name,
	       coalesce(p.address, b.address) AS property_address,
	       coalesce(p.lat, b.lat)::float8 AS lat, coalesce(p.lng, b.lng)::float8 AS lng,
	       p.building_id, b.name AS building_name, t.assigned_to AS assignee_id, u.full_name AS assignee_name,
	       to_char(t.scheduled_date, 'YYYY-MM-DD') AS date,
	       coalesce(to_char(t.window_start, 'HH24:MI'), '08:00') AS start,
	       coalesce(to_char(t.window_end, 'HH24:MI'), '17:00') AS "end",
	       t.is_late,
	       coalesce((SELECT json_agg(json_build_object('name', s.section, 'total', s.total, 'done', s.done) ORDER BY s.pos)
	                 FROM (SELECT section, min(position) AS pos, count(*)::int AS total,
	                              count(*) FILTER (WHERE is_done)::int AS done
	                       FROM task_checklist_items WHERE task_id = t.id GROUP BY section) s), '[]'::json) AS checklist,
	       (SELECT count(*)::int FROM files f
	        WHERE f.module = 'tasks' AND f.status = 'ready' AND f.deleted_at IS NULL
	          AND ((f.entity_type = 'task' AND f.entity_id = t.id)
	               OR (f.entity_type = 'task_checklist_item'
	                   AND f.entity_id IN (SELECT id FROM task_checklist_items WHERE task_id = t.id)))) AS photos,
	       t.started_at, t.finished_at, t.validated_at, t.reservation_id, r.code AS reservation_code,
	       t.maintenance_ticket_id, t.notes, t.completion_notes,
	       coalesce((SELECT json_agg(json_build_object('item_id', c.item_id, 'item', i.name, 'qty', c.qty, 'unit', i.unit)
	                                 ORDER BY i.name)
	                 FROM (SELECT item_id, sum(quantity)::float8 AS qty FROM task_consumptions
	                       WHERE task_id = t.id GROUP BY item_id) c
	                 JOIN inventory_items i ON i.id = c.item_id), '[]'::json) AS consumption,
	       (SELECT string_agg(i.name || ': ' || trim_scale(d.returned_qty)::text || ' de ' || trim_scale(d.expected_qty)::text,
	                          ', ' ORDER BY i.name)
	        FROM inventory_discrepancies d JOIN inventory_items i ON i.id = d.item_id
	        WHERE d.task_id = t.id AND d.status = 'open') AS discrepancy,
	       t.created_at, t.updated_at
	FROM tasks t
	JOIN properties p ON p.id = t.property_id
	LEFT JOIN buildings b ON b.id = p.building_id
	LEFT JOIN shared.users u ON u.id = t.assigned_to
	LEFT JOIN reservations r ON r.id = t.reservation_id`

// taskScope: tareas de sus propiedades o asignadas a el ($1 todas, $2 ids, $3 el actor).
const taskScope = `($1::bool OR t.property_id = ANY($2::uuid[]) OR t.assigned_to = $3)`

// List devuelve las tareas del alcance por dia y hora.
func (s *Service) List(ctx context.Context, actor authz.Actor, f Filter) ([]TaskRead, *string, error) {
	limit := paging.Limit(f.Limit)
	key, err := paging.Decode(f.Cursor, 3)
	if err != nil {
		return nil, nil, err
	}
	from, to := f.From, f.To
	if f.Date != "" {
		from, to = f.Date, f.Date
	}
	for _, d := range []struct{ v, field string }{{from, "query.from"}, {to, "query.to"}} {
		if d.v != "" {
			if err := validDate(d.v, d.field); err != nil {
				return nil, nil, err
			}
		}
	}
	if from != "" && to != "" {
		a, _ := time.Parse(time.DateOnly, from)
		b, _ := time.Parse(time.DateOnly, to)
		if b.Before(a) || b.Sub(a) > 92*24*time.Hour {
			return nil, nil, ErrInvalidRange.WithField("query.to")
		}
	}
	var afterDate, afterStart, afterNumber *string
	if key != nil {
		afterDate, afterStart, afterNumber = &key[0], &key[1], &key[2]
	}
	var assignee *uuid.UUID
	if f.Mine {
		assignee = &actor.UserID
	} else {
		assignee = f.AssigneeID
	}
	items, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]TaskRead, error) {
		scope, err := authz.ResolvePropertyScope(ctx, q, actor)
		if err != nil {
			return nil, err
		}
		all, ids := scope.SQLArgs()
		rows, err := q.Query(ctx, selectTask+`
			WHERE `+taskScope+`
			  AND ($4::date IS NULL OR t.scheduled_date >= $4)
			  AND ($5::date IS NULL OR t.scheduled_date <= $5)
			  AND ($6::uuid IS NULL OR t.property_id = $6)
			  AND ($7::uuid IS NULL OR t.assigned_to = $7)
			  AND (coalesce(cardinality($8::text[]), 0) = 0 OR t.status = ANY($8::text[]))
			  AND ($9::timestamptz IS NULL OR t.updated_at > $9)
			  AND ($9::timestamptz IS NOT NULL OR coalesce(cardinality($8::text[]), 0) > 0 OR t.status <> 'cancelled')
			  AND ($10::date IS NULL OR (t.scheduled_date, coalesce(t.window_start, '23:59'::time), t.number)
			       > ($10::date, $11::time, $12::int))
			ORDER BY t.scheduled_date, coalesce(t.window_start, '23:59'::time), t.number
			LIMIT $13`,
			all, ids, actor.UserID, nullable(from), nullable(to), f.PropertyID, assignee, f.Statuses, f.UpdatedSince,
			afterDate, afterStart, afterNumber, limit+1)
		if err != nil {
			return nil, err
		}
		list, err := pgx.CollectRows(rows, pgx.RowToStructByName[TaskRead])
		if err != nil {
			return nil, err
		}
		return list, s.withTimeline(ctx, q, list)
	})
	if err != nil {
		return nil, nil, err
	}
	items, next := paging.Page(items, limit, func(t TaskRead) []string {
		return []string{t.Date, t.Start, fmt.Sprint(t.Number)}
	})
	if items == nil {
		items = []TaskRead{}
	}
	return items, next, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Get devuelve la tarea con su checklist y la custodia de quien consulta.
func (s *Service) Get(ctx context.Context, actor authz.Actor, id uuid.UUID) (TaskDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TaskDetail, error) {
		t, err := s.visible(ctx, q, actor, id)
		if err != nil {
			return TaskDetail{}, err
		}
		return s.detail(ctx, q, actor, t)
	})
}

func (s *Service) detail(ctx context.Context, q database.Querier, actor authz.Actor, t TaskRead) (TaskDetail, error) {
	d := TaskDetail{TaskRead: t}
	rows, err := q.Query(ctx, `
		SELECT ci.id, ci.section, ci.position, ci.text, ci.requires_photo, ci.requires_before_after, ci.is_done,
		       ci.done_at, ci.notes,
		       coalesce((SELECT json_agg(json_build_object('file_id', f.id, 'kind', f.kind, 'created_at', f.created_at)
		                                 ORDER BY f.created_at)
		                 FROM files f WHERE f.entity_type = 'task_checklist_item' AND f.entity_id = ci.id
		                   AND f.module = 'tasks' AND f.status = 'ready' AND f.deleted_at IS NULL), '[]'::json) AS photos
		FROM task_checklist_items ci WHERE ci.task_id = $1 ORDER BY ci.position`, t.ID)
	if err != nil {
		return d, err
	}
	type itemRow struct {
		TaskChecklistItem
		Photos json.RawMessage `db:"photos"`
	}
	raw, err := pgx.CollectRows(rows, pgx.RowToStructByName[itemRow])
	if err != nil {
		return d, err
	}
	d.Items = make([]TaskChecklistItem, 0, len(raw))
	for _, r := range raw {
		it := r.TaskChecklistItem
		var photos []struct {
			FileID    uuid.UUID `json:"file_id"`
			Kind      string    `json:"kind"`
			CreatedAt time.Time `json:"created_at"`
		}
		_ = json.Unmarshal(r.Photos, &photos)
		it.Photos = make([]TaskPhoto, 0, len(photos))
		for _, p := range photos {
			it.Photos = append(it.Photos, TaskPhoto{FileID: p.FileID, Kind: p.Kind, CreatedAt: p.CreatedAt})
		}
		d.Items = append(d.Items, it)
	}
	ev, err := q.Query(ctx, `
		SELECT id, kind, created_at FROM files
		WHERE entity_type = 'task' AND entity_id = $1 AND module = 'tasks' AND status = 'ready' AND deleted_at IS NULL
		ORDER BY created_at`, t.ID)
	if err != nil {
		return d, err
	}
	if d.Evidence, err = pgx.CollectRows(ev, pgx.RowToStructByName[TaskPhoto]); err != nil {
		return d, err
	}
	if d.Evidence == nil {
		d.Evidence = []TaskPhoto{}
	}
	custody, err := inventory.CustodyOf(ctx, q, actor.UserID)
	if err != nil {
		return d, err
	}
	d.Custody = make([]TaskCustodyLine, 0, len(custody))
	for _, c := range custody {
		d.Custody = append(d.Custody, TaskCustodyLine{ItemID: c.ItemID, Name: c.Name, Unit: c.Unit, Category: c.Category,
			Returnable: c.Returnable, Quantity: c.Quantity})
	}
	d.CanExecute = actor.Permissions[authz.TasksExecute] &&
		(t.AssigneeID == nil || *t.AssigneeID == actor.UserID || actor.Permissions[authz.TasksAssign])
	d.CanManage = manager(actor)
	return d, nil
}

// visible devuelve la tarea si esta en el alcance (404 si no).
func (s *Service) visible(ctx context.Context, q database.Querier, actor authz.Actor, id uuid.UUID) (TaskRead, error) {
	scope, err := authz.ResolvePropertyScope(ctx, q, actor)
	if err != nil {
		return TaskRead{}, err
	}
	all, ids := scope.SQLArgs()
	rows, err := q.Query(ctx, selectTask+` WHERE t.id = $4 AND `+taskScope, all, ids, actor.UserID, id)
	if err != nil {
		return TaskRead{}, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[TaskRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskRead{}, ErrTaskNotFound
	}
	if err != nil {
		return TaskRead{}, err
	}
	list := []TaskRead{t}
	if err := s.withTimeline(ctx, q, list); err != nil {
		return TaskRead{}, err
	}
	return list[0], nil
}

// byID lee la tarea sin alcance (para eventos internos).
func (s *Service) byID(ctx context.Context, q database.Querier, id uuid.UUID) (TaskRead, error) {
	rows, err := q.Query(ctx, selectTask+` WHERE t.id = $1`, id)
	if err != nil {
		return TaskRead{}, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[TaskRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskRead{}, ErrTaskNotFound
	}
	if err != nil {
		return TaskRead{}, err
	}
	list := []TaskRead{t}
	if err := s.withTimeline(ctx, q, list); err != nil {
		return TaskRead{}, err
	}
	return list[0], nil
}

// withTimeline llena la linea de tiempo de cada tarea con una sola consulta.
func (s *Service) withTimeline(ctx context.Context, q database.Querier, list []TaskRead) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(list))
	idx := map[uuid.UUID]int{}
	for i, t := range list {
		ids[i] = t.ID
		idx[t.ID] = i
		list[i].Timeline = []TaskTimelineEvent{}
		if list[i].Checklist == nil {
			list[i].Checklist = []TaskChecklistSection{}
		}
		if list[i].Consumption == nil {
			list[i].Consumption = []TaskConsumption{}
		}
	}
	rows, err := q.Query(ctx, `
		SELECT e.task_id, e.type, e.data, e.created_at, coalesce(u.full_name, 'Sistema')
		FROM task_events e LEFT JOIN shared.users u ON u.id = e.actor_user_id
		WHERE e.task_id = ANY($1::uuid[]) ORDER BY e.created_at, e.id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID uuid.UUID
		var typ, actor string
		var data map[string]any
		var at time.Time
		if err := rows.Scan(&taskID, &typ, &data, &at, &actor); err != nil {
			return err
		}
		text, detail := describe(typ, data)
		i := idx[taskID]
		list[i].Timeline = append(list[i].Timeline, TaskTimelineEvent{At: at.UTC(), Type: typ, Text: text, Actor: actor, Detail: detail})
	}
	return rows.Err()
}

// describe convierte un evento en texto para la linea de tiempo.
func describe(typ string, data map[string]any) (string, *string) {
	str := func(k string) string {
		v, _ := data[k].(string)
		return v
	}
	var detail *string
	if n := str("note"); n != "" {
		detail = &n
	}
	switch typ {
	case "created":
		if str("source") == "reservation" {
			return "Creada desde la reserva " + str("reservation_code"), detail
		}
		if str("source") == "ticket" {
			return "Creada desde la incidencia #" + fmt.Sprint(data["ticket_number"]), detail
		}
		return "Creada", detail
	case "assigned":
		return "Asignada a " + orNobody(str("assignee_name")), detail
	case "reassigned":
		return "Reasignada a " + orNobody(str("assignee_name")), detail
	case "rescheduled":
		return "Reprogramada para el " + str("date") + " de " + str("start") + " a " + str("end"), detail
	case "started":
		return "Iniciada", detail
	case "checkpoint":
		return "Avance: " + str("text"), detail
	case "consumption":
		return "Registró insumos: " + str("summary"), detail
	case "finished":
		return "Finalizada", detail
	case "discrepancy":
		return "Diferencia de custodia: " + str("summary"), detail
	case "validated":
		return "Validada", detail
	case "returned":
		return "Devuelta para corregir", detail
	case "cancelled":
		return "Cancelada", detail
	case "late":
		if str("reason") == "not_started" {
			return "Retrasada: no se inició a tiempo", detail
		}
		return "Retrasada: no se terminó a tiempo", detail
	}
	return typ, detail
}

func orNobody(s string) string {
	if s == "" {
		return "nadie"
	}
	return s
}

// addEvent registra un evento de la tarea.
func addEvent(ctx context.Context, q database.Querier, taskID uuid.UUID, typ string, by *uuid.UUID, data map[string]any) error {
	if data == nil {
		data = map[string]any{}
	}
	_, err := q.Exec(ctx, `INSERT INTO task_events (task_id, type, actor_user_id, data) VALUES ($1, $2, $3, $4)`,
		taskID, typ, by, data)
	return err
}

// --- Personas asignables -------------------------------------------------------------

// Assignables devuelve a quienes se les puede asignar tareas (tasks.execute).
func (s *Service) Assignables(ctx context.Context, actor authz.Actor) ([]TaskAssignee, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]TaskAssignee, error) {
		rows, err := q.Query(ctx, `
			SELECT u.id, u.full_name AS name, r.name AS role, r.scoped AS scoped,
			       coalesce((SELECT array_agg(pa.property_id) FROM property_assignments pa WHERE pa.user_id = u.id),
			                '{}') AS property_ids
			FROM shared.memberships m
			JOIN shared.users u ON u.id = m.user_id
			JOIN shared.roles r ON r.id = m.role_id
			WHERE m.tenant_id = $1 AND m.status = 'active'
			  AND EXISTS (SELECT 1 FROM shared.role_permissions rp WHERE rp.role_id = m.role_id AND rp.permission = $2)
			ORDER BY u.full_name`, actor.TenantID, string(authz.TasksExecute))
		if err != nil {
			return nil, err
		}
		list, err := pgx.CollectRows(rows, pgx.RowToStructByName[TaskAssignee])
		for i := range list {
			list[i].Initials = initials(list[i].Name)
			if !list[i].Scoped {
				list[i].PropertyIDs = []uuid.UUID{}
			}
		}
		if list == nil {
			list = []TaskAssignee{}
		}
		return list, err
	})
}

// canAssign: tiene tasks.execute en la empresa y, si su rol es de alcance
// restringido, la propiedad asignada.
func canAssign(ctx context.Context, q database.Querier, tenantID, userID, propertyID uuid.UUID) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM shared.memberships m JOIN shared.roles r ON r.id = m.role_id
			WHERE m.tenant_id = $1 AND m.user_id = $2 AND m.status = 'active'
			  AND EXISTS (SELECT 1 FROM shared.role_permissions rp WHERE rp.role_id = m.role_id AND rp.permission = $4)
			  AND (NOT r.scoped OR EXISTS (SELECT 1 FROM property_assignments pa WHERE pa.user_id = $2 AND pa.property_id = $3)))`,
		tenantID, userID, propertyID, string(authz.TasksExecute)).Scan(&ok)
	return ok, err
}

func userName(ctx context.Context, q database.Querier, id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	var n string
	_ = q.QueryRow(ctx, `SELECT full_name FROM shared.users WHERE id = $1`, *id).Scan(&n)
	return n
}

// --- Alta y gestion --------------------------------------------------------------------

// newTask son los datos de una tarea por crear.
type newTask struct {
	Kind          string
	PropertyID    uuid.UUID
	Date          string
	Start, End    string
	AssigneeID    *uuid.UUID
	Priority      string
	Notes         *string
	ReservationID *uuid.UUID
	TicketID      *uuid.UUID
	By            *uuid.UUID
	Source        map[string]any
}

// Create programa una tarea (web: TaskDraft).
func (s *Service) Create(ctx context.Context, actor authz.Actor, in TaskCreate) (TaskRead, error) {
	if err := validDate(in.Date, "body.date"); err != nil {
		return TaskRead{}, err
	}
	if in.Start >= in.End {
		return TaskRead{}, ErrInvalidWindow.WithField("body.end")
	}
	priority := in.Priority
	if priority == "" {
		priority = "normal"
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TaskRead, error) {
		scope, err := authz.ResolvePropertyScope(ctx, q, actor)
		if err != nil {
			return TaskRead{}, err
		}
		var exists bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM properties WHERE id = $1 AND deleted_at IS NULL)`,
			in.PropertyID).Scan(&exists); err != nil {
			return TaskRead{}, err
		}
		if !exists || !scope.Allows(in.PropertyID) {
			return TaskRead{}, ErrUnknownProperty.WithField("body.property_id")
		}
		id, err := s.insert(ctx, q, actor.TenantID, newTask{
			Kind: in.Type, PropertyID: in.PropertyID, Date: in.Date, Start: in.Start, End: in.End,
			AssigneeID: in.AssigneeID, Priority: priority, Notes: trimmed(in.Notes), ReservationID: in.ReservationID,
			By: &actor.UserID,
		})
		if err != nil {
			return TaskRead{}, err
		}
		return s.byID(ctx, q, id)
	})
}

// insert crea la tarea con su checklist, eventos, bitacora y aviso.
func (s *Service) insert(ctx context.Context, q database.Querier, tenantID uuid.UUID, n newTask) (uuid.UUID, error) {
	if n.AssigneeID != nil {
		ok, err := canAssign(ctx, q, tenantID, *n.AssigneeID, n.PropertyID)
		if err != nil {
			return uuid.Nil, err
		}
		if !ok {
			return uuid.Nil, ErrInvalidAssignee.WithField("body.assignee_id")
		}
	}
	var assignedBy *uuid.UUID
	if n.AssigneeID != nil {
		assignedBy = n.By
	}
	var id uuid.UUID
	if err := q.QueryRow(ctx, `
		INSERT INTO tasks (property_id, type, kind, priority, scheduled_date, window_start, window_end, assigned_to,
		                   assigned_by, created_by, reservation_id, maintenance_ticket_id, notes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id`,
		n.PropertyID, kinds[n.Kind], n.Kind, n.Priority, n.Date, n.Start, n.End, n.AssigneeID, assignedBy, n.By,
		n.ReservationID, n.TicketID, n.Notes).Scan(&id); err != nil {
		return uuid.Nil, err
	}
	if kinds[n.Kind] == "cleaning" || n.Kind == "inspection" {
		if err := copyChecklist(ctx, q, id, n.PropertyID); err != nil {
			return uuid.Nil, err
		}
	}
	data := n.Source
	if data == nil {
		data = map[string]any{}
	}
	if err := addEvent(ctx, q, id, "created", n.By, data); err != nil {
		return uuid.Nil, err
	}
	if n.AssigneeID != nil {
		if err := addEvent(ctx, q, id, "assigned", n.By, map[string]any{
			"assignee_id": n.AssigneeID, "assignee_name": userName(ctx, q, n.AssigneeID),
		}); err != nil {
			return uuid.Nil, err
		}
	}
	if err := audit.Record(ctx, q, audit.Entry{
		Actor: n.By, Action: "task.created", Entity: "task", EntityID: &id, PropertyID: &n.PropertyID,
		After: map[string]any{"kind": n.Kind, "date": n.Date, "start": n.Start, "end": n.End, "assigned_to": n.AssigneeID},
	}); err != nil {
		return uuid.Nil, err
	}
	if n.AssigneeID != nil {
		t, err := s.byID(ctx, q, id)
		if err != nil {
			return uuid.Nil, err
		}
		if err := s.bus.Publish(ctx, q, events.Event{Type: EventAssigned, TenantID: tenantID, ActorID: n.By,
			Data: TaskEvent{Task: t}}); err != nil {
			return uuid.Nil, err
		}
	}
	return id, nil
}

// copyChecklist copia la plantilla de la propiedad (o la de por defecto) si la
// tarea aun no tiene checklist.
func copyChecklist(ctx context.Context, q database.Querier, taskID, propertyID uuid.UUID) error {
	_, err := q.Exec(ctx, `
		WITH tpl AS (
			SELECT coalesce(p.checklist_template_id, (SELECT id FROM checklist_templates WHERE is_default LIMIT 1)) AS id
			FROM properties p WHERE p.id = $2
		), done AS (
			UPDATE tasks SET checklist_template_id = (SELECT id FROM tpl) WHERE id = $1 AND checklist_template_id IS NULL
			RETURNING id
		)
		INSERT INTO task_checklist_items (task_id, section, position, text, requires_photo, requires_before_after)
		SELECT $1, i.section, i.position, i.text, i.requires_photo, i.requires_before_after
		FROM checklist_template_items i
		WHERE i.template_id = (SELECT id FROM tpl)
		  AND NOT EXISTS (SELECT 1 FROM task_checklist_items WHERE task_id = $1)
		ORDER BY i.position`, taskID, propertyID)
	return err
}

// lockTask bloquea la fila de la tarea (visible para el actor).
func (s *Service) lockTask(ctx context.Context, q database.Querier, actor authz.Actor, id uuid.UUID) (TaskRead, error) {
	t, err := s.visible(ctx, q, actor, id)
	if err != nil {
		return TaskRead{}, err
	}
	if _, err := q.Exec(ctx, `SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE`, id); err != nil {
		return TaskRead{}, err
	}
	return t, nil
}

// Reschedule mueve la tarea de dia u horario, o la reasigna.
func (s *Service) Reschedule(ctx context.Context, actor authz.Actor, id uuid.UUID, in TaskReschedule) (TaskRead, error) {
	reason := trim(in.Reason)
	if reason == "" {
		return TaskRead{}, ErrReasonRequired.WithField("body.reason")
	}
	if in.Date != nil {
		if err := validDate(*in.Date, "body.date"); err != nil {
			return TaskRead{}, err
		}
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TaskRead, error) {
		cur, err := s.lockTask(ctx, q, actor, id)
		if err != nil {
			return TaskRead{}, err
		}
		if cur.Status != "pending" && cur.Status != "returned" && cur.Status != "in_progress" {
			return TaskRead{}, ErrTransition
		}
		date, start, end := cur.Date, cur.Start, cur.End
		if in.Date != nil {
			date = *in.Date
		}
		if in.Start != nil {
			start = *in.Start
		}
		if in.End != nil {
			end = *in.End
		}
		if start >= end {
			return TaskRead{}, ErrInvalidWindow.WithField("body.end")
		}
		moved := date != cur.Date || start != cur.Start || end != cur.End
		assignee := cur.AssigneeID
		if in.AssigneeID.Sent {
			assignee = nil
			if !in.AssigneeID.Null {
				v := in.AssigneeID.Value
				assignee = &v
			}
		}
		reassigned := !sameID(assignee, cur.AssigneeID)
		if !moved && !reassigned {
			return TaskRead{}, ErrNothingToChange
		}
		if reassigned && assignee != nil {
			ok, err := canAssign(ctx, q, actor.TenantID, *assignee, cur.PropertyID)
			if err != nil {
				return TaskRead{}, err
			}
			if !ok {
				return TaskRead{}, ErrInvalidAssignee.WithField("body.assignee_id")
			}
		}
		if _, err := q.Exec(ctx, `
			UPDATE tasks SET scheduled_date = $2, window_start = $3, window_end = $4, assigned_to = $5,
			       assigned_by = CASE WHEN $6 THEN $7::uuid ELSE assigned_by END,
			       is_late = CASE WHEN $8 THEN false ELSE is_late END, updated_at = now()
			WHERE id = $1`, id, date, start, end, assignee, reassigned, actor.UserID, moved); err != nil {
			return TaskRead{}, err
		}
		if moved {
			if err := addEvent(ctx, q, id, "rescheduled", &actor.UserID, map[string]any{
				"date": date, "start": start, "end": end, "note": reason,
				"from": map[string]any{"date": cur.Date, "start": cur.Start, "end": cur.End},
			}); err != nil {
				return TaskRead{}, err
			}
		}
		if reassigned {
			if err := addEvent(ctx, q, id, "reassigned", &actor.UserID, map[string]any{
				"assignee_id": assignee, "assignee_name": userName(ctx, q, assignee), "note": reason,
				"previous": cur.AssigneeID,
			}); err != nil {
				return TaskRead{}, err
			}
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "task.rescheduled", Entity: "task", EntityID: &id, PropertyID: &cur.PropertyID,
			Before: map[string]any{"date": cur.Date, "start": cur.Start, "end": cur.End, "assigned_to": cur.AssigneeID},
			After:  map[string]any{"date": date, "start": start, "end": end, "assigned_to": assignee, "reason": reason},
		}); err != nil {
			return TaskRead{}, err
		}
		t, err := s.byID(ctx, q, id)
		if err != nil {
			return TaskRead{}, err
		}
		if reassigned {
			if err := s.bus.Publish(ctx, q, events.Event{Type: EventAssigned, TenantID: actor.TenantID, ActorID: &actor.UserID,
				Data: TaskEvent{Task: t, PreviousAssignee: cur.AssigneeID, Note: &reason}}); err != nil {
				return TaskRead{}, err
			}
		}
		if moved {
			if err := s.bus.Publish(ctx, q, events.Event{Type: EventRescheduled, TenantID: actor.TenantID, ActorID: &actor.UserID,
				Data: TaskEvent{Task: t, Note: &reason}}); err != nil {
				return TaskRead{}, err
			}
		}
		return t, nil
	})
}

func sameID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Validate aprueba una tarea finalizada.
func (s *Service) Validate(ctx context.Context, actor authz.Actor, id uuid.UUID, in TaskDecision) (TaskRead, error) {
	return s.decide(ctx, actor, id, "validated", trimmed(in.Note))
}

// Return devuelve una tarea finalizada para que la corrijan.
func (s *Service) Return(ctx context.Context, actor authz.Actor, id uuid.UUID, in TaskDecision) (TaskRead, error) {
	note := trimmed(in.Note)
	if note == nil {
		return TaskRead{}, ErrReasonRequired.WithField("body.note")
	}
	return s.decide(ctx, actor, id, "returned", note)
}

// Cancel cancela una tarea que aun no se termina.
func (s *Service) Cancel(ctx context.Context, actor authz.Actor, id uuid.UUID, in TaskDecision) (TaskRead, error) {
	return s.decide(ctx, actor, id, "cancelled", trimmed(in.Note))
}

func (s *Service) decide(ctx context.Context, actor authz.Actor, id uuid.UUID, to string, note *string) (TaskRead, error) {
	from := map[string][]string{
		"validated": {"done"},
		"returned":  {"done", "validated"},
		"cancelled": {"pending", "in_progress", "returned"},
	}[to]
	event := map[string]string{"validated": EventValidated, "returned": EventReturned, "cancelled": EventCancelled}[to]
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TaskRead, error) {
		cur, err := s.lockTask(ctx, q, actor, id)
		if err != nil {
			return TaskRead{}, err
		}
		ok := false
		for _, st := range from {
			ok = ok || cur.Status == st
		}
		if !ok {
			return TaskRead{}, ErrTransition
		}
		if _, err := q.Exec(ctx, `
			UPDATE tasks SET status = $2::text,
			       validated_by = CASE WHEN $2::text = 'validated' THEN $3::uuid WHEN $2::text = 'returned' THEN NULL ELSE validated_by END,
			       validated_at = CASE WHEN $2::text = 'validated' THEN now() WHEN $2::text = 'returned' THEN NULL ELSE validated_at END,
			       updated_at = now()
			WHERE id = $1`, id, to, actor.UserID); err != nil {
			return TaskRead{}, err
		}
		data := map[string]any{}
		if note != nil {
			data["note"] = *note
		}
		if err := addEvent(ctx, q, id, to, &actor.UserID, data); err != nil {
			return TaskRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "task." + to, Entity: "task", EntityID: &id, PropertyID: &cur.PropertyID,
			Before: map[string]any{"status": cur.Status}, After: map[string]any{"status": to, "note": note},
		}); err != nil {
			return TaskRead{}, err
		}
		t, err := s.byID(ctx, q, id)
		if err != nil {
			return TaskRead{}, err
		}
		return t, s.bus.Publish(ctx, q, events.Event{Type: event, TenantID: actor.TenantID, ActorID: &actor.UserID,
			Data: TaskEvent{Task: t, Note: note}})
	})
}

// --- Limpiezas desde reservas -------------------------------------------------------

// GenerateFromReservations crea la limpieza de salida de cada reserva viva
// que sale entre from y to y aun no la tiene.
func (s *Service) GenerateFromReservations(ctx context.Context, actor authz.Actor, in TaskGenerate) (TaskGenerated, error) {
	if err := validDate(in.From, "body.from"); err != nil {
		return TaskGenerated{}, err
	}
	if err := validDate(in.To, "body.to"); err != nil {
		return TaskGenerated{}, err
	}
	if in.To < in.From {
		return TaskGenerated{}, ErrInvalidRange.WithField("body.to")
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TaskGenerated, error) {
		scope, err := authz.ResolvePropertyScope(ctx, q, actor)
		if err != nil {
			return TaskGenerated{}, err
		}
		all, ids := scope.SQLArgs()
		rows, err := q.Query(ctx, `
			SELECT r.id FROM reservations r
			WHERE r.check_out_date BETWEEN $1 AND $2 AND r.status IN ('confirmed', 'checked_in', 'checked_out')
			  AND ($3::bool OR r.property_id = ANY($4::uuid[]))
			  AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.reservation_id = r.id AND t.kind = 'checkout_cleaning'
			                  AND t.status <> 'cancelled')
			ORDER BY r.check_out_date, r.check_out_time`, in.From, in.To, all, ids)
		if err != nil {
			return TaskGenerated{}, err
		}
		list, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return TaskGenerated{}, err
		}
		created := 0
		for _, rid := range list {
			ok, err := s.checkoutCleaning(ctx, q, actor.TenantID, &actor.UserID, rid)
			if err != nil {
				return TaskGenerated{}, err
			}
			if ok {
				created++
			}
		}
		return TaskGenerated{Created: created}, nil
	})
}

// checkoutCleaning crea la limpieza de salida de la reserva: el dia de la
// salida, desde la hora de salida hasta la siguiente entrada (o 4 horas).
// Si la propiedad tiene una sola persona de limpieza asignada, se la asigna.
func (s *Service) checkoutCleaning(ctx context.Context, q database.Querier, tenantID uuid.UUID, by *uuid.UUID, reservationID uuid.UUID) (bool, error) {
	var propertyID uuid.UUID
	var date, start, end, code string
	err := q.QueryRow(ctx, `
		SELECT r.property_id, to_char(r.check_out_date, 'YYYY-MM-DD'), r.code,
		       to_char(r.check_out_time, 'HH24:MI'),
		       to_char(least(
		           coalesce((SELECT min(n.check_in_time) FROM reservations n
		                     WHERE n.property_id = r.property_id AND n.check_in_date = r.check_out_date
		                       AND n.status NOT IN ('cancelled', 'no_show') AND n.id <> r.id), '23:59'::time),
		           CASE WHEN r.check_out_time >= '19:59'::time THEN '23:59'::time ELSE r.check_out_time + interval '4 hours' END),
		       'HH24:MI')
		FROM reservations r
		WHERE r.id = $1 AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.reservation_id = r.id
		                                  AND t.kind = 'checkout_cleaning' AND t.status <> 'cancelled')`,
		reservationID).Scan(&propertyID, &date, &code, &start, &end)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if end <= start {
		end = "23:59"
	}
	if start >= end {
		start = "08:00"
	}
	var assignee *uuid.UUID
	rows, err := q.Query(ctx, `
		SELECT pa.user_id FROM property_assignments pa
		JOIN shared.memberships m ON m.user_id = pa.user_id AND m.tenant_id = $2 AND m.status = 'active'
		WHERE pa.property_id = $1
		  AND EXISTS (SELECT 1 FROM shared.role_permissions rp WHERE rp.role_id = m.role_id AND rp.permission = $3)
		  AND NOT EXISTS (SELECT 1 FROM shared.role_permissions rp WHERE rp.role_id = m.role_id AND rp.permission = $4)`,
		propertyID, tenantID, string(authz.TasksExecute), string(authz.MaintenanceExecute))
	if err != nil {
		return false, err
	}
	cleaners, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return false, err
	}
	if len(cleaners) == 1 {
		assignee = &cleaners[0]
	}
	_, err = s.insert(ctx, q, tenantID, newTask{
		Kind: "checkout_cleaning", PropertyID: propertyID, Date: date, Start: start, End: end, AssigneeID: assignee,
		Priority: "normal", ReservationID: &reservationID, By: by,
		Source: map[string]any{"source": "reservation", "reservation_code": code},
	})
	return err == nil, err
}

// summary arma "Toallas 2, Jabon 1".
func summary(parts []string) string {
	return strings.Join(parts, ", ")
}
