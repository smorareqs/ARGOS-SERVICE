package notifications

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/modules/alerts"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/disciplinary"
	"github.com/smorareqs/argos-service/internal/modules/inventory"
	"github.com/smorareqs/argos-service/internal/modules/maintenance"
	"github.com/smorareqs/argos-service/internal/modules/payroll"
	"github.com/smorareqs/argos-service/internal/modules/reservations"
	"github.com/smorareqs/argos-service/internal/modules/tasks"
)

// Subscribe convierte los eventos de dominio en avisos (fase03.md §4). Los
// handlers corren en la transaccion del cambio y solo insertan y encolan.
func (s *Service) Subscribe(bus *events.Bus) {
	on := func(typ string, fn func(ctx context.Context, q database.Querier, e events.Event) error) {
		bus.Subscribe(typ, fn)
	}
	// Tareas.
	on(tasks.EventAssigned, s.onTask)
	on(tasks.EventRescheduled, s.onTask)
	on(tasks.EventCompleted, s.onTask)
	on(tasks.EventValidated, s.onTask)
	on(tasks.EventReturned, s.onTask)
	on(tasks.EventCancelled, s.onTask)
	on(tasks.EventLate, s.onTask)
	// Inventario.
	on(inventory.EventDiscrepancyOpened, s.onDiscrepancy)
	on(inventory.EventRestockRequested, s.onRestock)
	// Mantenimiento.
	on(maintenance.EventTicketCreated, s.onTicket)
	on(maintenance.EventTicketAssigned, s.onTicket)
	on(maintenance.EventTicketResolved, s.onTicket)
	on(maintenance.EventMaintenanceDue, s.onPlanDue)
	on(alerts.EventRaised, s.onAlert)
	// Reservas del sitio, nomina y disciplina.
	on(reservations.EventRequested, s.onReservationRequested)
	on(payroll.EventClosed, s.onPayroll)
	on(disciplinary.EventCreated, s.onDisciplinary)
	on(disciplinary.EventAcknowledged, s.onDisciplinary)
}

// as lee el Data del evento sea valor o puntero.
func as[T any](d any) (T, bool) {
	switch v := d.(type) {
	case T:
		return v, true
	case *T:
		if v != nil {
			return *v, true
		}
	}
	var zero T
	return zero, false
}

var kindLabels = map[string]string{
	"checkout_cleaning": "Limpieza de salida",
	"deep_cleaning":     "Limpieza profunda",
	"touch_up":          "Repaso antes de llegada",
	"inspection":        "Inspección",
	"maintenance":       "Mantenimiento",
}

func taskLabel(kind string) string {
	if l, ok := kindLabels[kind]; ok {
		return l
	}
	return "Tarea"
}

func one(id *uuid.UUID) []uuid.UUID {
	if id == nil {
		return nil
	}
	return []uuid.UUID{*id}
}

func (s *Service) onTask(ctx context.Context, q database.Querier, e events.Event) error {
	ev, ok := as[tasks.TaskEvent](e.Data)
	if !ok {
		return nil
	}
	t := ev.Task
	where := fmt.Sprintf("%s · %s", t.PropertyCode, t.PropertyName)
	when := fmt.Sprintf("%s %s–%s", humanDate(t.Date), t.Start, t.End)
	base := Notice{
		Except:   e.ActorID,
		Route:    "/itinerario?tarea=" + t.ID.String(),
		AppRoute: "/tasks/" + t.ID.String(),
		Data:     map[string]any{"task_id": t.ID.String(), "property_id": t.PropertyID.String()},
	}
	note := ""
	if ev.Note != nil && strings.TrimSpace(*ev.Note) != "" {
		note = ": " + strings.TrimSpace(*ev.Note)
	}
	label := taskLabel(t.Type)
	var notices []Notice
	switch e.Type {
	case tasks.EventAssigned:
		n := base
		n.Recipients, n.Type = one(t.AssigneeID), TypeTaskAssigned
		n.Title = "Nueva tarea: " + strings.ToLower(label)
		n.Body = where + " · " + when
		n.Channels = []string{ChannelPush, ChannelEmail}
		notices = append(notices, n)
		if ev.PreviousAssignee != nil && (t.AssigneeID == nil || *ev.PreviousAssignee != *t.AssigneeID) {
			p := base
			p.Recipients, p.Type = one(ev.PreviousAssignee), TypeTaskAssigned
			p.Title = "Te reasignaron una tarea"
			p.Body = label + " en " + where + " ya no está en tu agenda"
			p.AppRoute, p.Route = "/home/today", "/itinerario"
			p.Channels = []string{ChannelPush}
			notices = append(notices, p)
		}
	case tasks.EventRescheduled:
		n := base
		n.Recipients, n.Type = one(t.AssigneeID), TypeTaskRescheduled
		n.Title = "Cambió tu tarea"
		n.Body = label + " en " + where + " ahora es el " + when + note
		n.Channels = []string{ChannelPush, ChannelEmail}
		notices = append(notices, n)
	case tasks.EventCompleted:
		people, err := Holders(ctx, q, e.TenantID, &t.PropertyID, authz.TasksValidate)
		if err != nil {
			return err
		}
		n := base
		n.Recipients, n.Type = people, TypeTaskCompleted
		n.Title = label + " terminada"
		who := "Alguien"
		if t.AssigneeName != nil {
			who = *t.AssigneeName
		}
		n.Body = where + " · " + who + " la cerró; está lista para validar"
		if t.Discrepancy != nil {
			n.Body += " · " + *t.Discrepancy
		}
		n.Channels = []string{ChannelInApp}
		notices = append(notices, n)
	case tasks.EventValidated:
		n := base
		n.Recipients, n.Type = one(t.AssigneeID), TypeTaskValidated
		n.Title = "Tu " + strings.ToLower(label) + " fue validada"
		n.Body = where + note
		n.Channels = []string{ChannelPush}
		notices = append(notices, n)
	case tasks.EventReturned:
		n := base
		n.Recipients, n.Type = one(t.AssigneeID), TypeTaskReturned
		n.Title = "Devolvieron tu tarea para corregir"
		n.Body = where + note
		n.Channels = []string{ChannelPush}
		notices = append(notices, n)
	case tasks.EventCancelled:
		n := base
		n.Recipients, n.Type = one(t.AssigneeID), TypeTaskCancelled
		n.Title = "Se canceló una tarea"
		n.Body = label + " en " + where + " del " + humanDate(t.Date) + note
		n.Channels = []string{ChannelPush}
		notices = append(notices, n)
	case tasks.EventLate:
		people, err := Holders(ctx, q, e.TenantID, &t.PropertyID, authz.TasksAssign)
		if err != nil {
			return err
		}
		n := base
		n.Recipients, n.Type = people, TypeTaskLate
		n.Title = label + " retrasada"
		if t.Status == "pending" {
			n.Body = where + " debía iniciar a las " + t.Start
		} else {
			n.Body = where + " debía terminar a las " + t.End
		}
		n.Channels = []string{ChannelPush, ChannelInApp}
		notices = append(notices, n)
	}
	for _, n := range notices {
		if err := s.Notify(ctx, q, e.TenantID, n); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) onDiscrepancy(ctx context.Context, q database.Querier, e events.Event) error {
	ev, ok := as[inventory.DiscrepancyEvent](e.Data)
	if !ok {
		return nil
	}
	people, err := Holders(ctx, q, e.TenantID, nil, authz.InventoryManage, authz.InventoryReconcile)
	if err != nil {
		return err
	}
	var name string
	_ = q.QueryRow(ctx, `SELECT full_name FROM shared.users WHERE id = $1`, ev.UserID).Scan(&name)
	body := fmt.Sprintf("%s: faltan %s (esperado %s, devolvió %s)", ev.ItemName,
		num(ev.Expected-ev.Returned), num(ev.Expected), num(ev.Returned))
	if name != "" {
		body = name + " · " + body
	}
	if ev.TaskNumber != nil && *ev.TaskNumber > 0 {
		body += fmt.Sprintf(" · tarea #%d", *ev.TaskNumber)
	}
	return s.Notify(ctx, q, e.TenantID, Notice{
		Recipients: people, Type: TypeDiscrepancyOpen,
		Title: "Diferencia de inventario", Body: body,
		Route: "/inventario?tab=diferencias", AppRoute: "/inventory",
		Data:     map[string]any{"discrepancy_id": ev.ID.String()},
		Channels: []string{ChannelPush, ChannelEmail},
	})
}

func (s *Service) onRestock(ctx context.Context, q database.Querier, e events.Event) error {
	ev, ok := as[inventory.RestockEvent](e.Data)
	if !ok {
		return nil
	}
	r := ev.Request
	people, err := Holders(ctx, q, e.TenantID, r.PropertyID, authz.InventoryManage)
	if err != nil {
		return err
	}
	parts := make([]string, 0, len(r.Items))
	for _, i := range r.Items {
		parts = append(parts, fmt.Sprintf("%s × %s", i.ItemName, num(i.Qty)))
	}
	return s.Notify(ctx, q, e.TenantID, Notice{
		Recipients: people, Except: e.ActorID, Type: TypeRestockRequested,
		Title: "Solicitud de reposición", Body: r.PersonName + " pide " + strings.Join(parts, ", "),
		Route: "/inventario?tab=solicitudes", AppRoute: "/inventory",
		Data:     map[string]any{"request_id": r.ID.String()},
		Channels: []string{ChannelPush, ChannelInApp},
	})
}

func (s *Service) onTicket(ctx context.Context, q database.Querier, e events.Event) error {
	ev, ok := as[maintenance.TicketEvent](e.Data)
	if !ok {
		return nil
	}
	t := ev.Ticket
	where := ""
	if t.PropertyCode != nil {
		where = *t.PropertyCode
	} else if t.BuildingName != nil {
		where = *t.BuildingName
	}
	base := Notice{
		Except:   e.ActorID,
		Route:    "/mantenimiento?ver=" + t.ID.String(),
		AppRoute: "/tickets/" + t.ID.String(),
		Data:     map[string]any{"ticket_id": t.ID.String()},
	}
	title := fmt.Sprintf("#%d %s", t.Number, t.Title)
	switch e.Type {
	case maintenance.EventTicketCreated:
		people, err := Holders(ctx, q, e.TenantID, t.PropertyID, authz.MaintenanceManage, authz.MaintenanceAssign)
		if err != nil {
			return err
		}
		n := base
		n.Recipients, n.Type = people, TypeTicketCreated
		n.Title = "Incidencia nueva · " + urgencyLabel(t.Urgency)
		n.Body = joinNonEmpty(" · ", title, where, deref(t.ReporterName, ""))
		n.Channels = []string{ChannelPush, ChannelInApp}
		if t.Urgency == "critical" || t.Urgency == "high" {
			n.Channels = append(n.Channels, ChannelEmail)
		}
		return s.Notify(ctx, q, e.TenantID, n)
	case maintenance.EventTicketAssigned:
		n := base
		n.Recipients, n.Type = one(t.AssigneeID), TypeTicketAssigned
		n.Title = "Te asignaron una incidencia"
		n.Body = joinNonEmpty(" · ", title, where, urgencyLabel(t.Urgency))
		n.Channels = []string{ChannelPush, ChannelEmail}
		return s.Notify(ctx, q, e.TenantID, n)
	case maintenance.EventTicketResolved:
		n := base
		n.Recipients, n.Type = one(t.ReporterID), TypeTicketResolved
		n.Title = "Incidencia resuelta"
		n.Body = joinNonEmpty(" · ", title, where, deref(t.Resolution, ""))
		n.Channels = []string{ChannelPush}
		return s.Notify(ctx, q, e.TenantID, n)
	}
	return nil
}

func (s *Service) onPlanDue(ctx context.Context, q database.Querier, e events.Event) error {
	ev, ok := as[maintenance.PlanDueEvent](e.Data)
	if !ok {
		return nil
	}
	return s.Notify(ctx, q, e.TenantID, Notice{
		Recipients: one(ev.Plan.ResponsibleID), Type: TypeMaintenanceDue,
		Title: "Preventivo por hacer",
		Body:  fmt.Sprintf("%s de %s toca el %s", ev.Plan.Name, ev.Plan.AssetName, humanDate(ev.Plan.NextDueAt)),
		Route: "/mantenimiento?tab=preventivos", AppRoute: "/tickets/" + ev.Ticket.ID.String(),
		Data:     map[string]any{"plan_id": ev.Plan.ID.String(), "ticket_id": ev.Ticket.ID.String()},
		Channels: []string{ChannelPush, ChannelEmail},
	})
}

func (s *Service) onAlert(ctx context.Context, q database.Querier, e events.Event) error {
	a, ok := as[alerts.AlertRead](e.Data)
	if !ok {
		return nil
	}
	n := Notice{
		Route: "/alertas?tipo=" + a.Type, AppRoute: "/alerts",
		Data: map[string]any{"alert_id": a.ID.String(), "alert_type": a.Type},
	}
	var perms []authz.Permission
	switch a.Type {
	case "task_late", "discrepancy_open":
		return nil // ya avisan task.late e inventory.discrepancy_opened
	case "low_stock":
		n.Type, n.Title = TypeLowStock, "Stock bajo"
		n.Route, n.AppRoute = "/inventario", "/inventory"
		n.Channels = []string{ChannelEmail, ChannelInApp}
		perms = []authz.Permission{authz.InventoryManage}
	default:
		n.Type, n.Title = TypeAlert, alertTitle(a.Type)
		n.Channels = []string{ChannelInApp}
		if a.Severity == "high" {
			n.Channels = append(n.Channels, ChannelPush)
		}
		perms = []authz.Permission{authz.AlertsManage}
	}
	people, err := Holders(ctx, q, e.TenantID, a.PropertyID, perms...)
	if err != nil {
		return err
	}
	n.Recipients, n.Body = people, a.Message
	return s.Notify(ctx, q, e.TenantID, n)
}

func (s *Service) onReservationRequested(ctx context.Context, q database.Querier, e events.Event) error {
	ev, ok := as[reservations.ReservationEvent](e.Data)
	if !ok {
		return nil
	}
	r := ev.Reservation
	people, err := Holders(ctx, q, e.TenantID, &r.PropertyID, authz.ReservationsManage)
	if err != nil {
		return err
	}
	return s.Notify(ctx, q, e.TenantID, Notice{
		Recipients: people, Type: TypeReservationNew,
		Title: "Reserva nueva del sitio web",
		Body: joinNonEmpty(" · ", deref(r.GuestName, "Huésped"), r.PropertyCode,
			humanDate(r.CheckInDate)+" al "+humanDate(r.CheckOutDate)),
		Route: "/reservas/" + r.Code, AppRoute: "/reservations/" + r.ID.String(),
		Data:     map[string]any{"reservation_id": r.ID.String()},
		Channels: []string{ChannelPush, ChannelEmail},
	})
}

func (s *Service) onPayroll(ctx context.Context, q database.Querier, e events.Event) error {
	ev, ok := as[payroll.ClosedEvent](e.Data)
	if !ok {
		return nil
	}
	for _, entry := range ev.Entries {
		// El correo ya lo manda nomina (payslip_available).
		if err := s.Notify(ctx, q, e.TenantID, Notice{
			Recipients: []uuid.UUID{entry.UserID}, Type: TypePayslip,
			Title: "Tu boleta está lista", Body: ev.Period.Name,
			Route: "/perfil", AppRoute: "/payslips",
			Data:     map[string]any{"entry_id": entry.EntryID.String()},
			Channels: []string{ChannelPush},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) onDisciplinary(ctx context.Context, q database.Querier, e events.Event) error {
	a, ok := as[disciplinary.ActionRead](e.Data)
	if !ok {
		return nil
	}
	if e.Type == disciplinary.EventCreated {
		// El correo ya lo manda disciplina (disciplinary_notice).
		return s.Notify(ctx, q, e.TenantID, Notice{
			Recipients: []uuid.UUID{a.UserID}, Type: TypeDisciplinary,
			Title: "Tienes una acción disciplinaria por leer", Body: "Léela y confirma que la recibiste.",
			Route: "/perfil", AppRoute: "/disciplinary/" + a.ID.String(),
			Data:     map[string]any{"action_id": a.ID.String()},
			Channels: []string{ChannelPush},
		})
	}
	return s.Notify(ctx, q, e.TenantID, Notice{
		Recipients: one(a.IssuedBy), Type: TypeDisciplinaryAck,
		Title: a.EmployeeName + " dio su acuse", Body: deref(a.AcknowledgementNote, "Sin nota."),
		Route: "/rrhh?empleado=" + a.EmployeeID.String(), AppRoute: "/alerts",
		Data:     map[string]any{"action_id": a.ID.String()},
		Channels: []string{ChannelInApp},
	})
}

// ── Textos ────────────────────────────────────────────────

var months = []string{"ene", "feb", "mar", "abr", "may", "jun", "jul", "ago", "sep", "oct", "nov", "dic"}

// humanDate: "2026-11-26" → "26 nov".
func humanDate(d string) string {
	if len(d) < 10 {
		return d
	}
	var y, m, day int
	if _, err := fmt.Sscanf(d[:10], "%d-%d-%d", &y, &m, &day); err != nil || m < 1 || m > 12 {
		return d
	}
	return fmt.Sprintf("%d %s", day, months[m-1])
}

func num(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", v), "0"), ".")
}

func deref(s *string, def string) string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return def
	}
	return *s
}

func joinNonEmpty(sep string, parts ...string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

func urgencyLabel(u string) string {
	switch u {
	case "critical":
		return "crítica"
	case "high":
		return "alta"
	case "medium":
		return "media"
	default:
		return "baja"
	}
}

func alertTitle(t string) string {
	switch t {
	case "asset_failure":
		return "Activo con falla"
	case "maintenance_overdue":
		return "Preventivo vencido"
	case "checkout_alarm_unattended":
		return "Alarma de salida sin atender"
	default:
		return "Alerta nueva"
	}
}
