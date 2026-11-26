// Package notifications es la bandeja, el push (FCM) y el correo de avisos
// (APP/docs/plans/fase03.md §4, parte de Persona B).
//
// Los modulos no llaman a este paquete: publican eventos de dominio y
// Subscribe los convierte en avisos. Notify inserta las filas en la misma
// transaccion del cambio y encola el envio (send_push, send_email): un aviso
// existe solo si el cambio se confirma. Ademas avisa por tiempo real
// (realtime.Publish) para que la bandeja de la web y la app se actualice sin
// recargar.
package notifications

import (
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
)

// Tipos de aviso (los usa la web para el icono y el tono).
const (
	TypeTaskAssigned      = "task_assigned"
	TypeTaskRescheduled   = "task_rescheduled"
	TypeTaskReminder      = "task_reminder"
	TypeTaskLate          = "task_late"
	TypeTaskCompleted     = "task_completed"
	TypeTaskValidated     = "task_validated"
	TypeTaskReturned      = "task_returned"
	TypeTaskCancelled     = "task_cancelled"
	TypeDiscrepancyOpen   = "discrepancy_open"
	TypeRestockRequested  = "restock_requested"
	TypeTicketCreated     = "ticket_created"
	TypeTicketAssigned    = "ticket_assigned"
	TypeTicketResolved    = "ticket_resolved"
	TypeMaintenanceDue    = "maintenance_due"
	TypeLowStock          = "low_stock"
	TypeAlert             = "alert"
	TypeReservationNew    = "reservation_requested"
	TypeCheckoutAlarm     = "checkout_alarm"
	TypePayslip           = "payslip_available"
	TypeDisciplinary      = "disciplinary_notice"
	TypeDisciplinaryAck   = "disciplinary_acknowledged"
	TypeChatMessage       = "chat_message"
	TypeGuestCheckedIn    = "guest_checked_in"
	TypeReservationChange = "reservation_changed"
)

// Critical son los avisos que no se pueden apagar (fase03 §2).
var Critical = map[string]bool{
	TypeCheckoutAlarm: true,
	TypeTaskAssigned:  true,
}

// Canales.
const (
	ChannelPush  = "push"
	ChannelEmail = "email"
	ChannelInApp = "inapp"
)

// Trabajos del modulo.
const (
	// JobSendPush manda el push de un aviso a los dispositivos del usuario.
	JobSendPush = "send_push"
	// JobSendGuestPush manda un push al telefono de un huesped (fase 4).
	JobSendGuestPush = "send_guest_push"
	// JobTaskReminders avisa 30 min antes de cada ventana (cada 5 min).
	JobTaskReminders = "task_reminders"
)

// Errores del modulo.
var (
	ErrNotFound    = apperr.New(apperr.KindNotFound, "El aviso no existe.")
	ErrInvalidType = apperr.New(apperr.KindValidation, "Tipo de aviso desconocido.")
	ErrTokenEmpty  = apperr.New(apperr.KindValidation, "Falta el token del dispositivo.")
	// ErrDeviceNotFound: el token no es de este usuario (o ya se quito).
	ErrDeviceNotFound = apperr.New(apperr.KindNotFound, "El dispositivo no está registrado.")
)

// Notice es un aviso por mandar.
type Notice struct {
	// Recipients son usuarios; los repetidos y uuid.Nil se ignoran.
	Recipients []uuid.UUID
	// Except no lo recibe (quien hizo el cambio no se avisa a si mismo).
	Except *uuid.UUID
	Type   string
	Title  string
	Body   string
	// Route es la ruta de la web; AppRoute la de la app. Data agrega ids.
	Route    string
	AppRoute string
	Data     map[string]any
	// Channels: inapp siempre; push y email segun preferencias.
	Channels []string
}

// NotificationRead es un aviso de la bandeja.
type NotificationRead struct {
	ID        uuid.UUID      `db:"id" json:"id" format:"uuid"`
	Type      string         `db:"type" json:"type"`
	Title     string         `db:"title" json:"title"`
	Body      string         `db:"body" json:"body"`
	Data      map[string]any `db:"data" json:"data" doc:"route (web), app_route (app) y los ids del recurso."`
	ReadAt    *time.Time     `db:"read_at" json:"read_at" nullable:"true"`
	CreatedAt time.Time      `db:"created_at" json:"created_at"`
}

// NotificationPage es la respuesta de GET /notifications.
type NotificationPage struct {
	Items      []NotificationRead `json:"items"`
	NextCursor *string            `json:"next_cursor" nullable:"true"`
	Unread     int                `json:"unread" doc:"Total sin leer (punto de la campana y badge)."`
}

// Preference es lo que el usuario eligio para un tipo.
type Preference struct {
	Type     string `json:"type"`
	Label    string `json:"label"`
	Push     bool   `json:"push"`
	Email    bool   `json:"email"`
	Critical bool   `json:"critical" doc:"No se puede apagar."`
}

// PreferenceUpdate es una linea de PUT /notifications/preferences.
type PreferenceUpdate struct {
	Type  string `json:"type"`
	Push  bool   `json:"push"`
	Email bool   `json:"email"`
}

// PreferencesUpdate es el cuerpo de PUT /notifications/preferences.
type PreferencesUpdate struct {
	Items []PreferenceUpdate `json:"items" maxItems:"64"`
}

// DeviceRegister es el cuerpo de POST /devices.
type DeviceRegister struct {
	Token    string `json:"token" minLength:"1" maxLength:"512"`
	Platform string `json:"platform" enum:"android,ios,web"`
}

// Labels nombra los tipos en la pantalla de preferencias.
var Labels = map[string]string{
	TypeTaskAssigned:      "Tareas asignadas o reasignadas",
	TypeTaskRescheduled:   "Cambios de día u hora de mis tareas",
	TypeTaskReminder:      "Recordatorio 30 min antes",
	TypeTaskLate:          "Tareas retrasadas del equipo",
	TypeTaskCompleted:     "Tareas terminadas por validar",
	TypeTaskValidated:     "Mis tareas validadas",
	TypeTaskReturned:      "Mis tareas devueltas",
	TypeTaskCancelled:     "Mis tareas canceladas",
	TypeDiscrepancyOpen:   "Diferencias de inventario",
	TypeRestockRequested:  "Solicitudes de reposición",
	TypeTicketCreated:     "Incidencias nuevas",
	TypeTicketAssigned:    "Incidencias asignadas",
	TypeTicketResolved:    "Incidencias resueltas",
	TypeMaintenanceDue:    "Preventivos por hacer",
	TypeLowStock:          "Stock bajo",
	TypeAlert:             "Alertas",
	TypeReservationNew:    "Reservas del sitio web",
	TypeCheckoutAlarm:     "Alarmas de check-out",
	TypePayslip:           "Boletas de pago",
	TypeDisciplinary:      "Acciones disciplinarias",
	TypeDisciplinaryAck:   "Acuses de acciones disciplinarias",
	TypeChatMessage:       "Mensajes de chat",
	TypeGuestCheckedIn:    "Llegadas de huéspedes",
	TypeReservationChange: "Cambios en reservas",
}

// Service es la bandeja y el envio de avisos.
type Service struct {
	db      *database.DB
	log     *slog.Logger
	push    PushSender
	webBase string
	now     func() time.Time
}

// NewService crea el servicio. push nil = solo log.
func NewService(db *database.DB, push PushSender, webBase string, log *slog.Logger) *Service {
	if push == nil {
		push = LogPush{Log: log}
	}
	return &Service{db: db, log: log, push: push, webBase: webBase, now: time.Now}
}

// SetClock cambia el reloj (pruebas).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// SetPush cambia el emisor de push (pruebas).
func (s *Service) SetPush(p PushSender) { s.push = p }

// DB expone la base (pruebas y semilla).
func (s *Service) DB() *database.DB { return s.db }
