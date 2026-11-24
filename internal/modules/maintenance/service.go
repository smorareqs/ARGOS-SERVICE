// Package maintenance gestiona el mantenimiento de cada empresa (fase03.md):
// proveedores, activos por propiedad o edificio con sus documentos, planes
// preventivos e incidencias (tickets) con su linea de tiempo. Todo se filtra
// por el alcance de quien consulta (authz.PropertyScope): fuera de alcance
// responde 404.
//
// Integraciones con otros modulos, sin importarlos:
//   - Alertas: un activo con falla o un preventivo vencido levantan alertas
//     (alerts.Service.Raise) y se resuelven solas al corregirse.
//   - Tareas (Persona B): al asignar un ticket a un colaborador se llama al
//     TaskLinker registrado (crea la tarea tipo maintenance); al cerrar esa
//     tarea, el modulo de tareas llama a ResolveFromTask.
//   - Notificaciones (Persona B): se suscribe a los eventos Event* del bus.
package maintenance

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/storage"
	"github.com/smorareqs/argos-service/internal/modules/alerts"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Eventos que publica el modulo (fase03.md §4). Data va en cada constante.
const (
	// EventTicketCreated: alguien reporto una incidencia o la genero un
	// preventivo. Data: TicketEvent. Destinatarios: admin y supervisores.
	EventTicketCreated = "ticket.created"
	// EventTicketAssigned: cambio el colaborador o el proveedor. Data:
	// TicketEvent (con PreviousAssignee). Destinatario: el asignado.
	EventTicketAssigned = "ticket.assigned"
	// EventTicketResolved: se marco resuelta. Data: TicketEvent.
	// Destinatario: quien la reporto.
	EventTicketResolved = "ticket.resolved"
	// EventMaintenanceDue: un preventivo entro en su anticipacion y genero
	// su ticket. Data: PlanDueEvent. Destinatario: el responsable del plan.
	EventMaintenanceDue = "maintenance.due"
)

// TicketEvent es el Data de los eventos de tickets.
type TicketEvent struct {
	Ticket TicketRead
	// PreviousAssignee es quien la tenia antes (solo en EventTicketAssigned).
	PreviousAssignee *uuid.UUID
}

// PlanDueEvent es el Data de EventMaintenanceDue.
type PlanDueEvent struct {
	Plan   PlanRead
	Ticket TicketRead
}

// TaskLinker crea (o reasigna) la tarea de mantenimiento de un ticket
// asignado a un colaborador y devuelve su id, que se guarda en task_id. Lo
// registra el modulo de tareas (Persona B) con SetTaskLinker. Corre en la
// transaccion de la asignacion. Sin linker, asignar no crea tarea.
type TaskLinker func(ctx context.Context, q database.Querier, actor authz.Actor, t TicketRead) (*uuid.UUID, error)

// Errores del modulo.
var (
	ErrVendorNotFound = apperr.New(apperr.KindNotFound, "El proveedor no existe.")
	ErrAssetNotFound  = apperr.New(apperr.KindNotFound, "El activo no existe.")
	ErrPlanNotFound   = apperr.New(apperr.KindNotFound, "El preventivo no existe.")
	ErrTicketNotFound = apperr.New(apperr.KindNotFound, "La incidencia no existe.")
	ErrFileNotFound   = apperr.New(apperr.KindNotFound, "El documento no existe.")
	// ErrPropertyNotFound: reportar sobre una propiedad fuera de alcance responde 404 (fase03.md §8).
	ErrPropertyNotFound = apperr.New(apperr.KindNotFound, "La propiedad no existe.")

	ErrVendorNameTaken  = apperr.New(apperr.KindValidation, "Ya hay un proveedor con ese nombre.")
	ErrUnknownVendor    = apperr.New(apperr.KindValidation, "Ese proveedor no existe o está inactivo.")
	ErrUnknownProperty  = apperr.New(apperr.KindValidation, "Esa propiedad no existe.")
	ErrUnknownBuilding  = apperr.New(apperr.KindValidation, "Ese edificio no existe.")
	ErrUnknownAsset     = apperr.New(apperr.KindValidation, "Ese activo no es de esta propiedad.")
	ErrAssetOwner       = apperr.New(apperr.KindValidation, "Indica la propiedad o el edificio del activo (solo uno).")
	ErrInvalidDate      = apperr.New(apperr.KindValidation, "La fecha no es válida.")
	ErrWarrantyBefore   = apperr.New(apperr.KindValidation, "La garantía no puede terminar antes de la instalación.")
	ErrInvalidAssignee  = apperr.New(apperr.KindValidation, "Esa persona no puede atender incidencias en esta empresa.")
	ErrNoAssignee       = apperr.New(apperr.KindValidation, "Elige un colaborador o un proveedor.")
	ErrInvalidFiles     = apperr.New(apperr.KindValidation, "Alguna foto no se subió bien. Vuelve a adjuntarla.")
	ErrInvalidDocument  = apperr.New(apperr.KindValidation, "El documento no se subió bien. Vuelve a adjuntarlo.")
	ErrReasonRequired   = apperr.New(apperr.KindValidation, "Escribe el motivo.")
	ErrTextTooLong      = apperr.New(apperr.KindValidation, "El texto es demasiado largo.")
	ErrNotYourTicket    = apperr.New(apperr.KindPermissionDenied, "Esta incidencia está asignada a otra persona.")
	ErrTicketTransition = apperr.New(apperr.KindValidation, "La incidencia no está en un estado que permita esto.")
	ErrPlanTicketOpen   = apperr.New(apperr.KindValidation, "Este preventivo ya tiene otra incidencia abierta.")
)

// Service contiene los casos de uso de mantenimiento.
type Service struct {
	db      *database.DB
	storage *storage.Service
	alerts  *alerts.Service
	bus     *events.Bus
	log     *slog.Logger
	linker  TaskLinker
	now     func() time.Time
}

// NewService crea el servicio.
func NewService(db *database.DB, st *storage.Service, al *alerts.Service, bus *events.Bus, log *slog.Logger) *Service {
	return &Service{db: db, storage: st, alerts: al, bus: bus, log: log, now: time.Now}
}

// SetTaskLinker registra la creacion de tareas del modulo de tareas.
func (s *Service) SetTaskLinker(l TaskLinker) {
	s.linker = l
}

// SetClock cambia el reloj (pruebas de los jobs).
func (s *Service) SetClock(now func() time.Time) {
	s.now = now
}

// today es la fecha de hoy (AAAA-MM-DD) en la zona horaria de la empresa del
// schema activo.
func (s *Service) today(ctx context.Context, q database.Querier) (string, error) {
	var d string
	err := q.QueryRow(ctx, `
		SELECT to_char(($1::timestamptz AT TIME ZONE coalesce(
		         (SELECT timezone FROM shared.tenants WHERE schema_name = current_schema()), 'America/Guatemala'))::date,
		       'YYYY-MM-DD')`, s.now()).Scan(&d)
	return d, err
}

// scopeOf resuelve el alcance del actor.
func scopeOf(ctx context.Context, q database.Querier, actor authz.Actor) (authz.PropertyScope, error) {
	return authz.ResolvePropertyScope(ctx, q, actor)
}

// manager indica si el actor administra mantenimiento (asigna, cierra, edita).
func manager(actor authz.Actor) bool {
	return actor.Permissions[authz.MaintenanceManage] || actor.Permissions[authz.MaintenanceAssign]
}

// validDate valida una fecha AAAA-MM-DD opcional.
func validDate(d *string, field string) error {
	if d == nil {
		return nil
	}
	if _, err := time.Parse(time.DateOnly, *d); err != nil {
		return ErrInvalidDate.WithField(field)
	}
	return nil
}

// addDays suma dias a una fecha AAAA-MM-DD valida.
func addDays(d string, n int) string {
	t, _ := time.Parse(time.DateOnly, d)
	return t.AddDate(0, 0, n).Format(time.DateOnly)
}

// trimmed devuelve el texto sin espacios o nil si queda vacio.
func trimmed(s *string) *string {
	if s == nil {
		return nil
	}
	v := trim(*s)
	if v == "" {
		return nil
	}
	return &v
}
