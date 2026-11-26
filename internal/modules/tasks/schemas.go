package tasks

import (
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/optional"
)

// TaskChecklistSection es el avance de una seccion del checklist.
type TaskChecklistSection struct {
	Name  string `json:"name"`
	Total int    `json:"total"`
	Done  int    `json:"done"`
}

// TaskConsumption es un insumo usado en la tarea.
type TaskConsumption struct {
	ItemID uuid.UUID `json:"item_id" format:"uuid"`
	Item   string    `json:"item"`
	Qty    float64   `json:"qty"`
	Unit   string    `json:"unit"`
}

// TaskTimelineEvent es una linea de la historia de la tarea, ya en texto.
type TaskTimelineEvent struct {
	At     time.Time `json:"at"`
	Type   string    `json:"type"`
	Text   string    `json:"text"`
	Actor  string    `json:"actor" doc:"Nombre de quien lo hizo; \"Sistema\" si fue un trabajo."`
	Detail *string   `json:"detail,omitempty" nullable:"true"`
}

// TaskRead es una tarea como la muestran la web (itinerario) y la app.
type TaskRead struct {
	ID              uuid.UUID              `db:"id" json:"id" format:"uuid"`
	Number          int                    `db:"number" json:"number" doc:"Folio corto de la empresa (#128)."`
	Type            string                 `db:"kind" json:"type" enum:"checkout_cleaning,deep_cleaning,touch_up,inspection,maintenance"`
	Status          string                 `db:"status" json:"status" enum:"pending,in_progress,done,validated,returned,cancelled"`
	Priority        string                 `db:"priority" json:"priority" enum:"normal,high"`
	PropertyID      uuid.UUID              `db:"property_id" json:"property_id" format:"uuid"`
	PropertyCode    string                 `db:"property_code" json:"property_code"`
	PropertyName    string                 `db:"property_name" json:"property_name"`
	PropertyAddress *string                `db:"property_address" json:"property_address" nullable:"true"`
	Lat             *float64               `db:"lat" json:"lat" nullable:"true" doc:"La de la propiedad o la de su edificio."`
	Lng             *float64               `db:"lng" json:"lng" nullable:"true"`
	BuildingID      *uuid.UUID             `db:"building_id" json:"building_id" format:"uuid" nullable:"true"`
	BuildingName    *string                `db:"building_name" json:"building_name" nullable:"true"`
	AssigneeID      *uuid.UUID             `db:"assignee_id" json:"assignee_id" format:"uuid" nullable:"true"`
	AssigneeName    *string                `db:"assignee_name" json:"assignee_name" nullable:"true"`
	Date            string                 `db:"date" json:"date" format:"date" doc:"Dia local AAAA-MM-DD."`
	Start           string                 `db:"start" json:"start" doc:"HH:MM"`
	End             string                 `db:"end" json:"end" doc:"HH:MM"`
	Late            bool                   `db:"is_late" json:"late" doc:"La marco el trabajo de retrasos."`
	Checklist       []TaskChecklistSection `db:"checklist" json:"checklist"`
	Photos          int                    `db:"photos" json:"photos"`
	StartedAt       *time.Time             `db:"started_at" json:"started_at" nullable:"true"`
	FinishedAt      *time.Time             `db:"finished_at" json:"finished_at" nullable:"true"`
	ValidatedAt     *time.Time             `db:"validated_at" json:"validated_at" nullable:"true"`
	ReservationID   *uuid.UUID             `db:"reservation_id" json:"reservation_id" format:"uuid" nullable:"true"`
	ReservationCode *string                `db:"reservation_code" json:"reservation_code" nullable:"true"`
	TicketID        *uuid.UUID             `db:"maintenance_ticket_id" json:"ticket_id" format:"uuid" nullable:"true"`
	Notes           *string                `db:"notes" json:"notes" nullable:"true"`
	CompletionNotes *string                `db:"completion_notes" json:"completion_notes" nullable:"true"`
	Consumption     []TaskConsumption      `db:"consumption" json:"consumption"`
	Discrepancy     *string                `db:"discrepancy" json:"discrepancy" nullable:"true" doc:"Resumen de las diferencias de inventario abiertas."`
	Timeline        []TaskTimelineEvent    `db:"-" json:"timeline"`
	CreatedAt       time.Time              `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time              `db:"updated_at" json:"updated_at"`
}

// TaskPhoto es una foto subida a un punto del checklist.
type TaskPhoto struct {
	FileID    uuid.UUID `db:"id" json:"file_id" format:"uuid"`
	Kind      string    `db:"kind" json:"kind" enum:"before,after,evidence"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

// TaskChecklistItem es un punto del checklist de la tarea.
type TaskChecklistItem struct {
	ID                  uuid.UUID   `db:"id" json:"id" format:"uuid"`
	Section             string      `db:"section" json:"section"`
	Position            int         `db:"position" json:"position"`
	Text                string      `db:"text" json:"text"`
	RequiresPhoto       bool        `db:"requires_photo" json:"requires_photo"`
	RequiresBeforeAfter bool        `db:"requires_before_after" json:"requires_before_after"`
	IsDone              bool        `db:"is_done" json:"is_done"`
	DoneAt              *time.Time  `db:"done_at" json:"done_at" nullable:"true"`
	Notes               *string     `db:"notes" json:"notes" nullable:"true"`
	Photos              []TaskPhoto `db:"photos" json:"photos"`
}

// TaskCustodyLine es lo que la persona tiene en custodia al cerrar.
type TaskCustodyLine struct {
	ItemID     uuid.UUID `db:"item_id" json:"item_id" format:"uuid"`
	Name       string    `db:"name" json:"name"`
	Unit       string    `db:"unit" json:"unit"`
	Category   string    `db:"category" json:"category"`
	Returnable bool      `db:"returnable" json:"returnable" doc:"Blancos y llaves: se devuelven completos."`
	Quantity   float64   `db:"quantity" json:"quantity"`
}

// TaskDetail es la tarea con su checklist completo (app B3/B4, web detalle).
type TaskDetail struct {
	TaskRead
	Items      []TaskChecklistItem `json:"items"`
	Evidence   []TaskPhoto         `json:"evidence" doc:"Fotos de la tarea que no son de un punto del checklist."`
	Custody    []TaskCustodyLine   `json:"custody" doc:"La custodia de quien consulta, para la conciliacion del cierre."`
	CanExecute bool                `json:"can_execute" doc:"Quien consulta puede iniciarla y cerrarla."`
	CanManage  bool                `json:"can_manage" doc:"Quien consulta puede reasignarla o validarla."`
}

// TaskAssignee es una persona a la que se le pueden asignar tareas.
type TaskAssignee struct {
	ID          uuid.UUID   `db:"id" json:"id" format:"uuid"`
	Name        string      `db:"name" json:"name"`
	Initials    string      `db:"-" json:"initials"`
	Role        string      `db:"role" json:"role"`
	Scoped      bool        `db:"scoped" json:"scoped" doc:"Solo puede atender sus propiedades asignadas."`
	PropertyIDs []uuid.UUID `db:"property_ids" json:"property_ids" doc:"Sus propiedades asignadas (vacio si no tiene alcance restringido)."`
}

// Filter son los filtros de GET /tasks.
type Filter struct {
	Date         string
	From, To     string
	PropertyID   *uuid.UUID
	AssigneeID   *uuid.UUID
	Statuses     []string
	Mine         bool
	UpdatedSince *time.Time
	Cursor       string
	Limit        int
}

// TaskCreate es el cuerpo de POST /tasks (web: TaskDraft).
type TaskCreate struct {
	Type          string     `json:"type" enum:"checkout_cleaning,deep_cleaning,touch_up,inspection,maintenance"`
	PropertyID    uuid.UUID  `json:"property_id" format:"uuid"`
	Date          string     `json:"date" format:"date"`
	Start         string     `json:"start" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" doc:"HH:MM"`
	End           string     `json:"end" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" doc:"HH:MM"`
	AssigneeID    *uuid.UUID `json:"assignee_id,omitempty" format:"uuid" nullable:"true"`
	Priority      string     `json:"priority,omitempty" enum:"normal,high" default:"normal"`
	Notes         *string    `json:"notes,omitempty" maxLength:"2000" nullable:"true"`
	ReservationID *uuid.UUID `json:"reservation_id,omitempty" format:"uuid" nullable:"true"`
}

// TaskReschedule es el cuerpo de POST /tasks/{id}/reschedule (web: TaskMove).
type TaskReschedule struct {
	AssigneeID optional.Value[uuid.UUID] `json:"assignee_id,omitzero" format:"uuid" doc:"null la deja sin asignar."`
	Date       *string                   `json:"date,omitempty" format:"date"`
	Start      *string                   `json:"start,omitempty" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$"`
	End        *string                   `json:"end,omitempty" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$"`
	Reason     string                    `json:"reason" maxLength:"500"`
}

// TaskDecision es el cuerpo de validate, return y cancel.
type TaskDecision struct {
	Note *string `json:"note,omitempty" maxLength:"1000" nullable:"true"`
}

// TaskGenerate es el cuerpo de POST /tasks/generate-from-reservations.
type TaskGenerate struct {
	From string `json:"from" format:"date"`
	To   string `json:"to" format:"date"`
}

// TaskGenerated es la respuesta de generate-from-reservations.
type TaskGenerated struct {
	Created int `json:"created"`
}

// TaskStart es el cuerpo de POST /tasks/{id}/start (el GPS es obligatorio).
type TaskStart struct {
	Lat float64 `json:"lat" minimum:"-90" maximum:"90"`
	Lng float64 `json:"lng" minimum:"-180" maximum:"180"`
}

// TaskItemUpdate es el cuerpo de PATCH /tasks/{id}/checklist/{item_id}.
type TaskItemUpdate struct {
	IsDone *bool                  `json:"is_done,omitempty"`
	Notes  optional.Value[string] `json:"notes,omitzero" maxLength:"1000"`
}

// TaskConsumptionLine es un insumo usado (de la custodia propia).
type TaskConsumptionLine struct {
	ItemID   uuid.UUID `json:"item_id" format:"uuid"`
	Quantity float64   `json:"quantity" exclusiveMinimum:"0"`
}

// TaskConsumptions es el cuerpo de POST /tasks/{id}/consumptions.
type TaskConsumptions struct {
	Items []TaskConsumptionLine `json:"items" minItems:"1" maxItems:"50"`
}

// TaskCustodyReturn es la declaracion de un articulo en custodia al cerrar.
type TaskCustodyReturn struct {
	ItemID   uuid.UUID `json:"item_id" format:"uuid"`
	Returned float64   `json:"returned" minimum:"0" doc:"Lo que devuelve a la bodega."`
	Consumed float64   `json:"consumed" minimum:"0" doc:"Lo que gasto (solo consumibles)."`
	Kept     float64   `json:"kept" minimum:"0" doc:"Lo que se queda para su siguiente tarea."`
}

// TaskComplete es el cuerpo de POST /tasks/{id}/complete (prueba de salida).
type TaskComplete struct {
	Lat   float64 `json:"lat" minimum:"-90" maximum:"90"`
	Lng   float64 `json:"lng" minimum:"-180" maximum:"180"`
	Notes *string `json:"notes,omitempty" maxLength:"2000" nullable:"true"`
	// Custody es la conciliacion: debe traer cada articulo que tiene en
	// custodia. Vacio si no tiene nada.
	Custody []TaskCustodyReturn `json:"custody,omitempty" maxItems:"100"`
}
