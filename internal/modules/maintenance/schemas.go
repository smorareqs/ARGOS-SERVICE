package maintenance

import (
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/optional"
)

// --- Proveedores --------------------------------------------------------------

// VendorRead es un proveedor con lo que ha atendido.
type VendorRead struct {
	ID          uuid.UUID `db:"id" json:"id" format:"uuid"`
	Name        string    `db:"name" json:"name"`
	ServiceType *string   `db:"service_type" json:"service_type" nullable:"true" doc:"Especialidad: elevadores, plomeria..."`
	ContactName *string   `db:"contact_name" json:"contact_name" nullable:"true"`
	Phone       *string   `db:"phone" json:"phone" nullable:"true"`
	Email       *string   `db:"email" json:"email" nullable:"true"`
	Notes       *string   `db:"notes" json:"notes" nullable:"true"`
	IsActive    bool      `db:"is_active" json:"is_active"`

	AssetsCount int       `db:"assets_count" json:"assets_count" doc:"Activos que tiene a cargo."`
	OpenTickets int       `db:"open_tickets" json:"open_tickets"`
	TicketsDone int       `db:"tickets_done" json:"tickets_done" doc:"Incidencias resueltas o cerradas."`
	SpendYear   float64   `db:"spend_year" json:"spend_year" doc:"Costo de sus incidencias de este año (GTQ)."`
	AvgHours    *float64  `db:"avg_hours" json:"avg_hours" nullable:"true" doc:"Horas promedio de reporte a resuelta."`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

// VendorCreate es el cuerpo de POST /vendors.
type VendorCreate struct {
	_           struct{} `additionalProperties:"true"`
	Name        string   `json:"name" minLength:"2" maxLength:"128"`
	ServiceType *string  `json:"service_type,omitempty" maxLength:"64"`
	ContactName *string  `json:"contact_name,omitempty" maxLength:"128"`
	Phone       *string  `json:"phone,omitempty" maxLength:"32"`
	Email       *string  `json:"email,omitempty" maxLength:"254" format:"email"`
	Notes       *string  `json:"notes,omitempty" maxLength:"2000"`
}

// VendorUpdate es el cuerpo de PATCH /vendors/{id}: solo cambia lo que viene
// (null borra los opcionales).
type VendorUpdate struct {
	_           struct{}               `additionalProperties:"true"`
	Name        *string                `json:"name,omitempty" minLength:"2" maxLength:"128"`
	ServiceType optional.Value[string] `json:"service_type,omitzero" maxLength:"64"`
	ContactName optional.Value[string] `json:"contact_name,omitzero" maxLength:"128"`
	Phone       optional.Value[string] `json:"phone,omitzero" maxLength:"32"`
	Email       optional.Value[string] `json:"email,omitzero" maxLength:"254"`
	Notes       optional.Value[string] `json:"notes,omitzero" maxLength:"2000"`
	IsActive    *bool                  `json:"is_active,omitempty"`
}

// --- Activos ------------------------------------------------------------------

// AssetRead es un activo (elevador, A/C, calentador...) de una propiedad o de un edificio.
type AssetRead struct {
	ID            uuid.UUID  `db:"id" json:"id" format:"uuid"`
	PropertyID    *uuid.UUID `db:"property_id" json:"property_id" format:"uuid" nullable:"true"`
	PropertyCode  *string    `db:"property_code" json:"property_code" nullable:"true"`
	PropertyName  *string    `db:"property_name" json:"property_name" nullable:"true"`
	BuildingID    *uuid.UUID `db:"building_id" json:"building_id" format:"uuid" nullable:"true" doc:"El del activo o el de su propiedad."`
	BuildingName  *string    `db:"building_name" json:"building_name" nullable:"true"`
	Category      string     `db:"category" json:"category" enum:"elevator,camera,water,paint,appliance,plumbing,electrical,hvac,hospitality,furniture,other"`
	Name          string     `db:"name" json:"name"`
	Brand         *string    `db:"brand" json:"brand" nullable:"true"`
	Model         *string    `db:"model" json:"model" nullable:"true"`
	Serial        *string    `db:"serial" json:"serial" nullable:"true"`
	Location      *string    `db:"location" json:"location" nullable:"true" doc:"Donde esta: cocina, azotea..."`
	InstalledAt   *string    `db:"installed_at" json:"installed_at" format:"date" nullable:"true"`
	WarrantyUntil *string    `db:"warranty_until" json:"warranty_until" format:"date" nullable:"true"`
	VendorID      *uuid.UUID `db:"vendor_id" json:"vendor_id" format:"uuid" nullable:"true"`
	VendorName    *string    `db:"vendor_name" json:"vendor_name" nullable:"true"`
	Status        string     `db:"status" json:"status" enum:"operational,needs_attention,in_repair,out_of_service"`
	Notes         *string    `db:"notes" json:"notes" nullable:"true"`
	IsActive      bool       `db:"is_active" json:"is_active"`

	OpenTickets  int       `db:"open_tickets" json:"open_tickets"`
	TicketsCount int       `db:"tickets_count" json:"tickets_count"`
	CostYear     float64   `db:"cost_year" json:"cost_year" doc:"Costo de sus incidencias de este año (GTQ)."`
	NextDueAt    *string   `db:"next_due_at" json:"next_due_at" format:"date" nullable:"true" doc:"Proximo preventivo activo."`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`
}

// DocumentRead es un documento del activo (manual, garantia, factura).
type DocumentRead struct {
	FileID      uuid.UUID `json:"file_id" format:"uuid"`
	Name        string    `json:"name" doc:"Nombre original del archivo."`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	URL         string    `json:"url" doc:"Descarga firmada (1 h)."`
	CreatedAt   time.Time `json:"created_at"`
}

// AssetDetail es la ficha del activo (W-G2).
type AssetDetail struct {
	AssetRead
	Plans     []PlanRead     `json:"plans"`
	Documents []DocumentRead `json:"documents"`
	Tickets   []TicketRead   `json:"tickets" doc:"Las 20 incidencias mas recientes."`
}

// AssetCreate es el cuerpo de POST /assets. Lleva property_id o building_id (uno).
type AssetCreate struct {
	_             struct{}   `additionalProperties:"true"`
	PropertyID    *uuid.UUID `json:"property_id,omitempty" format:"uuid"`
	BuildingID    *uuid.UUID `json:"building_id,omitempty" format:"uuid"`
	Category      string     `json:"category" enum:"elevator,camera,water,paint,appliance,plumbing,electrical,hvac,hospitality,furniture,other"`
	Name          string     `json:"name" minLength:"2" maxLength:"128"`
	Brand         *string    `json:"brand,omitempty" maxLength:"128"`
	Model         *string    `json:"model,omitempty" maxLength:"128"`
	Serial        *string    `json:"serial,omitempty" maxLength:"128"`
	Location      *string    `json:"location,omitempty" maxLength:"128"`
	InstalledAt   *string    `json:"installed_at,omitempty" format:"date"`
	WarrantyUntil *string    `json:"warranty_until,omitempty" format:"date"`
	VendorID      *uuid.UUID `json:"vendor_id,omitempty" format:"uuid"`
	Status        string     `json:"status,omitempty" enum:"operational,needs_attention,in_repair,out_of_service" doc:"Por defecto operational."`
	Notes         *string    `json:"notes,omitempty" maxLength:"2000"`
}

// AssetUpdate es el cuerpo de PATCH /assets/{id}. Cambiar el estado a
// needs_attention u out_of_service levanta la alerta asset_failure; volver a
// operational la resuelve.
type AssetUpdate struct {
	_             struct{}                  `additionalProperties:"true"`
	PropertyID    optional.Value[uuid.UUID] `json:"property_id,omitzero"`
	BuildingID    optional.Value[uuid.UUID] `json:"building_id,omitzero"`
	Category      *string                   `json:"category,omitempty" enum:"elevator,camera,water,paint,appliance,plumbing,electrical,hvac,hospitality,furniture,other"`
	Name          *string                   `json:"name,omitempty" minLength:"2" maxLength:"128"`
	Brand         optional.Value[string]    `json:"brand,omitzero" maxLength:"128"`
	Model         optional.Value[string]    `json:"model,omitzero" maxLength:"128"`
	Serial        optional.Value[string]    `json:"serial,omitzero" maxLength:"128"`
	Location      optional.Value[string]    `json:"location,omitzero" maxLength:"128"`
	InstalledAt   optional.Value[string]    `json:"installed_at,omitzero" format:"date"`
	WarrantyUntil optional.Value[string]    `json:"warranty_until,omitzero" format:"date"`
	VendorID      optional.Value[uuid.UUID] `json:"vendor_id,omitzero"`
	Status        *string                   `json:"status,omitempty" enum:"operational,needs_attention,in_repair,out_of_service"`
	Notes         optional.Value[string]    `json:"notes,omitzero" maxLength:"2000"`
	IsActive      *bool                     `json:"is_active,omitempty"`
}

// DocumentAttach es el cuerpo de POST /assets/{id}/documents.
type DocumentAttach struct {
	_      struct{}  `additionalProperties:"true"`
	FileID uuid.UUID `json:"file_id" format:"uuid" doc:"Archivo maintenance/asset_document ya confirmado."`
}

// AssetFilter son los filtros de GET /assets.
type AssetFilter struct {
	Query      string
	PropertyID *uuid.UUID
	BuildingID *uuid.UUID
	VendorID   *uuid.UUID
	Category   string
	Status     string
	Active     *bool
	Cursor     string
	Limit      int
}

// --- Preventivos --------------------------------------------------------------

// PlanRead es un plan preventivo.
type PlanRead struct {
	ID              uuid.UUID  `db:"id" json:"id" format:"uuid"`
	AssetID         uuid.UUID  `db:"asset_id" json:"asset_id" format:"uuid"`
	AssetName       string     `db:"asset_name" json:"asset_name"`
	AssetCategory   string     `db:"asset_category" json:"asset_category"`
	PropertyID      *uuid.UUID `db:"property_id" json:"property_id" format:"uuid" nullable:"true"`
	PropertyCode    *string    `db:"property_code" json:"property_code" nullable:"true"`
	BuildingName    *string    `db:"building_name" json:"building_name" nullable:"true"`
	Name            string     `db:"name" json:"name"`
	FrequencyDays   int        `db:"frequency_days" json:"frequency_days"`
	ResponsibleID   *uuid.UUID `db:"responsible_user_id" json:"responsible_user_id" format:"uuid" nullable:"true"`
	ResponsibleName *string    `db:"responsible_name" json:"responsible_name" nullable:"true"`
	VendorID        *uuid.UUID `db:"vendor_id" json:"vendor_id" format:"uuid" nullable:"true"`
	VendorName      *string    `db:"vendor_name" json:"vendor_name" nullable:"true"`
	LastDoneAt      *string    `db:"last_done_at" json:"last_done_at" format:"date" nullable:"true"`
	NextDueAt       string     `db:"next_due_at" json:"next_due_at" format:"date"`
	LeadDays        int        `db:"lead_days" json:"lead_days" doc:"Dias antes del vencimiento en que se genera el ticket."`
	IsActive        bool       `db:"is_active" json:"is_active"`
	DueState        string     `db:"due_state" json:"due_state" enum:"ok,upcoming,overdue,inactive" doc:"upcoming = dentro de lead_days."`
	OpenTicketID    *uuid.UUID `db:"open_ticket_id" json:"open_ticket_id" format:"uuid" nullable:"true"`
	OpenTicketNo    *int       `db:"open_ticket_number" json:"open_ticket_number" nullable:"true"`
	CreatedAt       time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time  `db:"updated_at" json:"updated_at"`
}

// PlanCreate es el cuerpo de POST /assets/{id}/plans.
type PlanCreate struct {
	_             struct{}   `additionalProperties:"true"`
	Name          string     `json:"name" minLength:"2" maxLength:"128"`
	FrequencyDays int        `json:"frequency_days" minimum:"1" maximum:"3650"`
	ResponsibleID *uuid.UUID `json:"responsible_user_id,omitempty" format:"uuid"`
	VendorID      *uuid.UUID `json:"vendor_id,omitempty" format:"uuid"`
	LastDoneAt    *string    `json:"last_done_at,omitempty" format:"date"`
	NextDueAt     *string    `json:"next_due_at,omitempty" format:"date" doc:"Por defecto last_done_at + frecuencia, o hoy + frecuencia."`
	LeadDays      *int       `json:"lead_days,omitempty" minimum:"0" maximum:"365" doc:"Por defecto 7."`
}

// PlanUpdate es el cuerpo de PATCH /assets/{id}/plans/{plan_id}.
type PlanUpdate struct {
	_             struct{}                  `additionalProperties:"true"`
	Name          *string                   `json:"name,omitempty" minLength:"2" maxLength:"128"`
	FrequencyDays *int                      `json:"frequency_days,omitempty" minimum:"1" maximum:"3650"`
	ResponsibleID optional.Value[uuid.UUID] `json:"responsible_user_id,omitzero"`
	VendorID      optional.Value[uuid.UUID] `json:"vendor_id,omitzero"`
	NextDueAt     *string                   `json:"next_due_at,omitempty" format:"date"`
	LeadDays      *int                      `json:"lead_days,omitempty" minimum:"0" maximum:"365"`
	IsActive      *bool                     `json:"is_active,omitempty"`
}

// PlanFilter son los filtros de GET /maintenance/plans (calendario W-G3).
type PlanFilter struct {
	From, To string // next_due_at entre (inclusive); vacio = sin limite
	AssetID  *uuid.UUID
	Active   *bool
}

// --- Incidencias --------------------------------------------------------------

// TicketRead es una incidencia.
type TicketRead struct {
	ID            uuid.UUID  `db:"id" json:"id" format:"uuid"`
	Number        int        `db:"number" json:"number" doc:"Folio corto de la empresa (#42)."`
	PropertyID    *uuid.UUID `db:"property_id" json:"property_id" format:"uuid" nullable:"true" doc:"Vacio en preventivos de activos del edificio."`
	PropertyCode  *string    `db:"property_code" json:"property_code" nullable:"true"`
	PropertyName  *string    `db:"property_name" json:"property_name" nullable:"true"`
	BuildingID    *uuid.UUID `db:"building_id" json:"building_id" format:"uuid" nullable:"true" doc:"El del ticket o el de su propiedad."`
	BuildingName  *string    `db:"building_name" json:"building_name" nullable:"true"`
	AssetID       *uuid.UUID `db:"asset_id" json:"asset_id" format:"uuid" nullable:"true"`
	AssetName     *string    `db:"asset_name" json:"asset_name" nullable:"true"`
	AssetCategory *string    `db:"asset_category" json:"asset_category" nullable:"true"`
	PlanID        *uuid.UUID `db:"plan_id" json:"plan_id" format:"uuid" nullable:"true" doc:"Si nacio de un preventivo."`
	PlanName      *string    `db:"plan_name" json:"plan_name" nullable:"true"`
	Title         string     `db:"title" json:"title"`
	Description   string     `db:"description" json:"description"`
	Urgency       string     `db:"urgency" json:"urgency" enum:"low,medium,high,critical"`
	Status        string     `db:"status" json:"status" enum:"open,assigned,in_progress,resolved,closed,rejected"`
	ReporterID    *uuid.UUID `db:"reported_by_user_id" json:"reported_by_user_id" format:"uuid" nullable:"true"`
	ReporterName  *string    `db:"reporter_name" json:"reporter_name" nullable:"true" doc:"Vacio si lo genero el sistema (preventivo)."`
	AssigneeID    *uuid.UUID `db:"assigned_to" json:"assigned_to" format:"uuid" nullable:"true"`
	AssigneeName  *string    `db:"assignee_name" json:"assignee_name" nullable:"true"`
	VendorID      *uuid.UUID `db:"vendor_id" json:"vendor_id" format:"uuid" nullable:"true"`
	VendorName    *string    `db:"vendor_name" json:"vendor_name" nullable:"true"`
	TaskID        *uuid.UUID `db:"task_id" json:"task_id" format:"uuid" nullable:"true" doc:"Tarea de mantenimiento generada al asignar a un colaborador."`
	Resolution    *string    `db:"resolution" json:"resolution" nullable:"true"`
	Cost          *float64   `db:"cost" json:"cost" nullable:"true" doc:"GTQ."`
	PhotosCount   int        `db:"photos_count" json:"photos_count"`
	ResolvedAt    *time.Time `db:"resolved_at" json:"resolved_at" nullable:"true"`
	ClosedAt      *time.Time `db:"closed_at" json:"closed_at" nullable:"true"`
	CreatedAt     time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at" json:"updated_at"`
}

// TicketEventRead es un paso de la linea de tiempo.
type TicketEventRead struct {
	ID        uuid.UUID      `db:"id" json:"id" format:"uuid"`
	Type      string         `db:"type" json:"type" enum:"created,updated,assigned,started,resolved,closed,rejected,reopened"`
	ActorID   *uuid.UUID     `db:"actor_user_id" json:"actor_user_id" format:"uuid" nullable:"true"`
	ActorName *string        `db:"actor_name" json:"actor_name" nullable:"true" doc:"Vacio = el sistema."`
	Data      map[string]any `db:"data" json:"data" doc:"Segun el tipo: assignee_name, vendor_name, note, reason, resolution, cost, fields."`
	CreatedAt time.Time      `db:"created_at" json:"created_at"`
}

// PhotoRead es una foto del ticket.
type PhotoRead struct {
	FileID    uuid.UUID `json:"file_id" format:"uuid"`
	Stage     string    `json:"stage" enum:"report,resolution" doc:"Del reporte o de la solucion."`
	URL       string    `json:"url" doc:"Descarga firmada (1 h)."`
	CreatedAt time.Time `json:"created_at"`
}

// TicketDetail es la incidencia con su linea de tiempo y fotos (drawer de W-G1).
type TicketDetail struct {
	TicketRead
	Events []TicketEventRead `json:"events"`
	Photos []PhotoRead       `json:"photos"`
}

// TicketCreate es el cuerpo de POST /tickets (app B12 y web).
type TicketCreate struct {
	_           struct{}    `additionalProperties:"true"`
	PropertyID  uuid.UUID   `json:"property_id" format:"uuid"`
	AssetID     *uuid.UUID  `json:"asset_id,omitempty" format:"uuid"`
	Title       string      `json:"title" minLength:"3" maxLength:"160"`
	Description string      `json:"description,omitempty" maxLength:"4000"`
	Urgency     string      `json:"urgency" enum:"low,medium,high,critical"`
	PhotoIDs    []uuid.UUID `json:"photo_file_ids,omitempty" maxItems:"10" doc:"Archivos maintenance/ticket_photo ya confirmados por quien reporta."`
}

// TicketUpdate es el cuerpo de PATCH /tickets/{id}.
type TicketUpdate struct {
	_           struct{}                  `additionalProperties:"true"`
	Title       *string                   `json:"title,omitempty" minLength:"3" maxLength:"160"`
	Description *string                   `json:"description,omitempty" maxLength:"4000"`
	Urgency     *string                   `json:"urgency,omitempty" enum:"low,medium,high,critical"`
	AssetID     optional.Value[uuid.UUID] `json:"asset_id,omitzero"`
	Cost        optional.Value[float64]   `json:"cost,omitzero" minimum:"0"`
}

// TicketAssign es el cuerpo de POST /tickets/{id}/assign: a un colaborador, a
// un proveedor o a ambos (el colaborador acompana al proveedor).
type TicketAssign struct {
	_          struct{}   `additionalProperties:"true"`
	AssigneeID *uuid.UUID `json:"assignee_id,omitempty" format:"uuid" doc:"Miembro con maintenance.execute."`
	VendorID   *uuid.UUID `json:"vendor_id,omitempty" format:"uuid"`
	Note       string     `json:"note,omitempty" maxLength:"500"`
}

// TicketResolve es el cuerpo de POST /tickets/{id}/resolve.
type TicketResolve struct {
	_                struct{}    `additionalProperties:"true"`
	Resolution       string      `json:"resolution" minLength:"3" maxLength:"2000" doc:"Que se hizo."`
	Cost             *float64    `json:"cost,omitempty" minimum:"0"`
	PhotoIDs         []uuid.UUID `json:"photo_file_ids,omitempty" maxItems:"10" doc:"Archivos maintenance/ticket_resolution."`
	AssetOperational bool        `json:"asset_operational,omitempty" doc:"Deja el activo en operational (resuelve su alerta)."`
}

// TicketReason es el cuerpo de reject, reopen y close.
type TicketReason struct {
	_      struct{} `additionalProperties:"true"`
	Reason string   `json:"reason,omitempty" maxLength:"500" doc:"Obligatorio al rechazar y al reabrir."`
}

// TicketFilter son los filtros de GET /tickets.
type TicketFilter struct {
	Query      string
	Statuses   []string // vacio = todos; "active" = open, assigned, in_progress
	Urgency    string
	PropertyID *uuid.UUID
	AssetID    *uuid.UUID
	VendorID   *uuid.UUID
	AssigneeID *uuid.UUID
	Mine       bool // asignadas a quien consulta
	Cursor     string
	Limit      int
}

// Assignable es una persona que puede atender incidencias.
type Assignable struct {
	UserID      uuid.UUID `db:"user_id" json:"user_id" format:"uuid"`
	FullName    string    `db:"full_name" json:"full_name"`
	RoleName    string    `db:"role_name" json:"role_name"`
	OpenTickets int       `db:"open_tickets" json:"open_tickets" doc:"Asignadas y en curso: ayuda a repartir."`
}

// --- Indicadores (W-G5) -------------------------------------------------------

// PlaceCount son las incidencias abiertas de una propiedad o de un edificio.
type PlaceCount struct {
	Kind string    `json:"kind" enum:"property,building"`
	ID   uuid.UUID `json:"id" format:"uuid"`
	Code *string   `json:"code" nullable:"true" doc:"Codigo de la propiedad; vacio en edificios."`
	Name string    `json:"name"`
	Open int       `json:"open"`
}

// MonthHours es el tiempo promedio de resolucion de un mes.
type MonthHours struct {
	Month    string   `json:"month" doc:"AAAA-MM."`
	Hours    *float64 `json:"hours" nullable:"true" doc:"Vacio si no se resolvio ninguna."`
	Resolved int      `json:"resolved"`
}

// CategoryCost es el gasto de una categoria de activo.
type CategoryCost struct {
	Category string  `json:"category" doc:"Categoria del activo; none = sin activo."`
	Cost     float64 `json:"cost"`
	Tickets  int     `json:"tickets"`
}

// Metrics son los indicadores de mantenimiento del alcance.
type Metrics struct {
	Open         int      `json:"open" doc:"Abiertas, asignadas y en curso."`
	Urgent       int      `json:"urgent" doc:"Abiertas de urgencia alta o critica."`
	Unassigned   int      `json:"unassigned" doc:"Abiertas sin colaborador ni proveedor."`
	OverduePlans int      `json:"overdue_plans"`
	DuePlans     int      `json:"due_plans" doc:"Preventivos dentro de su anticipacion."`
	AvgHours     *float64 `json:"avg_hours" nullable:"true" doc:"Horas promedio de reporte a resuelta (ultimos 90 dias)."`
	CostMonth    float64  `json:"cost_month"`
	CostYear     float64  `json:"cost_year"`

	OpenByPlace    []PlaceCount   `json:"open_by_place" doc:"Las 10 propiedades o edificios con mas abiertas."`
	Resolution     []MonthHours   `json:"resolution" doc:"Ultimos 6 meses, del mas antiguo al actual."`
	CostByCategory []CategoryCost `json:"cost_by_category" doc:"Este año, de mayor a menor."`
}
