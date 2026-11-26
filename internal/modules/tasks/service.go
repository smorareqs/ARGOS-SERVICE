// Package tasks es el itinerario de cada empresa (fase02.md §2-3, Persona B):
// limpiezas, inspecciones y tareas de mantenimiento por propiedad, con su
// checklist, fotos, consumos, la conciliacion de custodia al cerrar
// (prueba de salida del colaborador) y la validacion del supervisor.
//
// Integraciones (sin que los otros modulos lo importen):
//   - properties.SetAccessGate: limpieza y mantenimiento ven el acceso de
//     una propiedad solo el dia que tienen tarea ahi.
//   - maintenance.SetTaskLinker: asignar una incidencia a un colaborador crea
//     (o reasigna) su tarea tipo maintenance; al cerrarla se resuelve la
//     incidencia (maintenance.ResolveFromTask).
//   - dashboard.SetTasksSource: el bloque de limpiezas del Inicio.
//   - reservations: reservation.confirmed crea la limpieza de salida,
//     reservation.updated la mueve y reservation.cancelled la cancela.
//   - Notificaciones (fase 3) se suscribe a los eventos Event*.
package tasks

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/inventory"
	"github.com/smorareqs/argos-service/internal/modules/maintenance"
)

// Eventos que publica el modulo. Data: TaskEvent.
const (
	// EventAssigned: se asigno o reasigno. Destinatario: el asignado.
	EventAssigned = "task.assigned"
	// EventRescheduled: cambio el dia o la ventana. Destinatario: el asignado.
	EventRescheduled = "task.rescheduled"
	// EventCompleted: el colaborador la cerro. Destinatarios: quienes validan.
	EventCompleted = "task.completed"
	// EventValidated: el supervisor la valido. Destinatario: el asignado.
	EventValidated = "task.validated"
	// EventReturned: el supervisor la devolvio con motivo. Destinatario: el asignado.
	EventReturned = "task.returned"
	// EventCancelled: se cancelo. Destinatario: el asignado.
	EventCancelled = "task.cancelled"
	// EventLate: el trabajo de retrasos la marco. Destinatarios: quienes asignan.
	EventLate = "task.late"
)

// TaskEvent es el Data de los eventos de tareas.
type TaskEvent struct {
	Task TaskRead
	// PreviousAssignee es quien la tenia antes (solo en EventAssigned).
	PreviousAssignee *uuid.UUID
	// Note es el motivo (devuelta, cancelada, reprogramada).
	Note *string
}

// JobMarkLate es el trabajo que marca las tareas retrasadas (cada 5 min).
const JobMarkLate = "mark_late_tasks"

// kinds: tipo fino (web y app) → tipo grueso (fase02.md, nomina).
var kinds = map[string]string{
	"checkout_cleaning": "cleaning",
	"deep_cleaning":     "cleaning",
	"touch_up":          "cleaning",
	"inspection":        "inspection",
	"maintenance":       "maintenance",
}

// Errores del modulo.
var (
	ErrTaskNotFound     = apperr.New(apperr.KindNotFound, "La tarea no existe.")
	ErrItemNotFound     = apperr.New(apperr.KindNotFound, "Ese punto del checklist no existe.")
	ErrPropertyNotFound = apperr.New(apperr.KindNotFound, "La propiedad no existe.")

	ErrUnknownProperty  = apperr.New(apperr.KindValidation, "Esa propiedad no existe.")
	ErrInvalidAssignee  = apperr.New(apperr.KindValidation, "Esa persona no puede atender esta propiedad. Asígnale la propiedad primero.")
	ErrInvalidDate      = apperr.New(apperr.KindValidation, "La fecha no es válida.")
	ErrInvalidWindow    = apperr.New(apperr.KindValidation, "La hora de fin debe ser después de la de inicio.")
	ErrInvalidRange     = apperr.New(apperr.KindValidation, "El rango de fechas no es válido (máximo 92 días).")
	ErrReasonRequired   = apperr.New(apperr.KindValidation, "Escribe el motivo.")
	ErrNothingToChange  = apperr.New(apperr.KindValidation, "Indica la nueva fecha, horario o persona.")
	ErrTransition       = apperr.New(apperr.KindValidation, "La tarea no está en un estado que permita esto.")
	ErrNotYourTask      = apperr.New(apperr.KindPermissionDenied, "Esta tarea está asignada a otra persona.")
	ErrChecklistPending = apperr.New(apperr.KindValidation, "Faltan puntos del checklist por marcar.")
	ErrPhotosMissing    = apperr.New(apperr.KindValidation, "Faltan fotos obligatorias del checklist.")
	ErrCustodyMissing   = apperr.New(apperr.KindValidation, "Indica qué devuelves, qué usaste y qué te quedas de cada artículo en custodia.")
	ErrCustodyUnknown   = apperr.New(apperr.KindValidation, "Ese artículo no está en tu custodia.")
	ErrCustodyConsumed  = apperr.New(apperr.KindValidation, "Los blancos y las llaves no se consumen: indica cuántos devuelves o te quedas.")
	ErrCustodyTooMuch   = apperr.New(apperr.KindValidation, "Declaras más de lo que tienes en custodia.")
)

// Service contiene los casos de uso de tareas.
type Service struct {
	db          *database.DB
	bus         *events.Bus
	inventory   *inventory.Service
	maintenance *maintenance.Service
	log         *slog.Logger
	now         func() time.Time
}

// NewService crea el servicio.
func NewService(db *database.DB, bus *events.Bus, inv *inventory.Service, m *maintenance.Service, log *slog.Logger) *Service {
	return &Service{db: db, bus: bus, inventory: inv, maintenance: m, log: log, now: time.Now}
}

// SetClock cambia el reloj (pruebas de los jobs).
func (s *Service) SetClock(now func() time.Time) {
	s.now = now
}

// today es la fecha de hoy (AAAA-MM-DD) en la zona horaria de la empresa.
func (s *Service) today(ctx context.Context, q database.Querier) (string, error) {
	var d string
	err := q.QueryRow(ctx, `
		SELECT to_char(($1::timestamptz AT TIME ZONE `+tenantTZ+`)::date, 'YYYY-MM-DD')`, s.now()).Scan(&d)
	return d, err
}

// tenantTZ es la zona horaria de la empresa del schema activo.
const tenantTZ = `coalesce((SELECT timezone FROM shared.tenants WHERE schema_name = current_schema()), 'America/Guatemala')`

// manager: puede reasignar, validar y ver todo lo del alcance.
func manager(a authz.Actor) bool {
	return a.Permissions[authz.TasksAssign] || a.Permissions[authz.TasksManage] || a.Permissions[authz.TasksValidate]
}

func trim(s string) string { return strings.TrimSpace(s) }

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

func validDate(d, field string) error {
	if _, err := time.Parse(time.DateOnly, d); err != nil {
		return ErrInvalidDate.WithField(field)
	}
	return nil
}

// initials devuelve las iniciales de un nombre (dos letras).
func initials(name string) string {
	parts := strings.Fields(name)
	out := []rune{}
	for _, p := range parts {
		r := []rune(p)
		if len(r) > 0 {
			out = append(out, r[0])
		}
		if len(out) == 2 {
			break
		}
	}
	return strings.ToUpper(string(out))
}

// DB es la base del servicio (semilla de demostracion).
func (s *Service) DB() *database.DB { return s.db }
