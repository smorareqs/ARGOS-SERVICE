package reservations

import (
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/optional"
)

// --- Canales ---------------------------------------------------------------

// ChannelRead es un canal de venta con sus reglas (W-H6).
type ChannelRead struct {
	ID                   uuid.UUID `db:"id" json:"id" format:"uuid"`
	Name                 string    `db:"name" json:"name"`
	Kind                 string    `db:"kind" json:"kind" enum:"direct,booking,airbnb,agency,other"`
	RequiresDeposit      bool      `db:"requires_deposit" json:"requires_deposit"`
	HasCheckin           bool      `db:"has_checkin" json:"has_checkin" doc:"Registro de huespedes y check-in/out formal."`
	DefaultDepositAmount *float64  `db:"default_deposit_amount" json:"default_deposit_amount" nullable:"true"`
	IsActive             bool      `db:"is_active" json:"is_active"`
	FeedsCount           int       `db:"feeds_count" json:"feeds_count" doc:"Calendarios iCal activos del canal."`
	UpcomingCount        int       `db:"upcoming_count" json:"upcoming_count" doc:"Reservas vivas que aun no salen."`
	CreatedAt            time.Time `db:"created_at" json:"created_at"`
	UpdatedAt            time.Time `db:"updated_at" json:"updated_at"`
}

// ChannelCreate es el cuerpo de POST /channels.
type ChannelCreate struct {
	_                    struct{} `additionalProperties:"true"`
	Name                 string   `json:"name" minLength:"2" maxLength:"64"`
	Kind                 string   `json:"kind" enum:"direct,booking,airbnb,agency,other"`
	RequiresDeposit      bool     `json:"requires_deposit,omitempty"`
	HasCheckin           bool     `json:"has_checkin,omitempty"`
	DefaultDepositAmount *float64 `json:"default_deposit_amount,omitempty" minimum:"0"`
}

// ChannelUpdate es el cuerpo de PATCH /channels/{id}.
type ChannelUpdate struct {
	_                    struct{}                `additionalProperties:"true"`
	Name                 *string                 `json:"name,omitempty" minLength:"2" maxLength:"64"`
	Kind                 *string                 `json:"kind,omitempty" enum:"direct,booking,airbnb,agency,other"`
	RequiresDeposit      *bool                   `json:"requires_deposit,omitempty"`
	HasCheckin           *bool                   `json:"has_checkin,omitempty"`
	DefaultDepositAmount optional.Value[float64] `json:"default_deposit_amount,omitzero" minimum:"0"`
	IsActive             *bool                   `json:"is_active,omitempty"`
}

// --- Calendarios iCal ------------------------------------------------------

// FeedRead es la URL iCal de una propiedad en un canal, con su ultima lectura.
type FeedRead struct {
	ID            uuid.UUID  `db:"id" json:"id" format:"uuid"`
	PropertyID    uuid.UUID  `db:"property_id" json:"property_id" format:"uuid"`
	PropertyCode  string     `db:"property_code" json:"property_code"`
	PropertyName  string     `db:"property_name" json:"property_name"`
	ChannelID     uuid.UUID  `db:"channel_id" json:"channel_id" format:"uuid"`
	ChannelName   string     `db:"channel_name" json:"channel_name"`
	ChannelKind   string     `db:"channel_kind" json:"channel_kind"`
	URL           string     `db:"url" json:"url"`
	IsActive      bool       `db:"is_active" json:"is_active"`
	LastSyncedAt  *time.Time `db:"last_synced_at" json:"last_synced_at" nullable:"true" doc:"Ultimo intento."`
	LastSuccessAt *time.Time `db:"last_success_at" json:"last_success_at" nullable:"true"`
	LastError     *string    `db:"last_error" json:"last_error" nullable:"true" doc:"Vacio si el ultimo intento salio bien."`
	EventsCount   *int       `db:"events_count" json:"events_count" nullable:"true" doc:"Eventos que traia en la ultima lectura correcta."`
	CreatedAt     time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at" json:"updated_at"`
}

// FeedCreate es el cuerpo de POST /properties/{id}/ical-feeds.
type FeedCreate struct {
	_         struct{}  `additionalProperties:"true"`
	ChannelID uuid.UUID `json:"channel_id" format:"uuid"`
	URL       string    `json:"url" minLength:"10" maxLength:"2048" doc:"https://... exportado por el canal."`
}

// FeedUpdate es el cuerpo de PATCH /ical-feeds/{id}.
type FeedUpdate struct {
	_        struct{} `additionalProperties:"true"`
	URL      *string  `json:"url,omitempty" minLength:"10" maxLength:"2048"`
	IsActive *bool    `json:"is_active,omitempty"`
}

// SyncResult es lo que hizo una lectura de un calendario.
type SyncResult struct {
	FeedID       uuid.UUID `json:"feed_id" format:"uuid"`
	PropertyCode string    `json:"property_code"`
	ChannelName  string    `json:"channel_name"`
	OK           bool      `json:"ok"`
	Error        *string   `json:"error" nullable:"true"`
	Events       int       `json:"events"`
	Created      int       `json:"created"`
	Updated      int       `json:"updated"`
	Cancelled    int       `json:"cancelled" doc:"Desaparecieron del calendario."`
	Conflicts    []string  `json:"conflicts" doc:"Eventos que se cruzan con otra reserva (no se importaron)."`
}

// --- Huespedes -------------------------------------------------------------

// GuestRead es un huesped del directorio (W-H5). Sin guests.read, la reserva
// solo trae el nombre: contacto y documento vienen vacios.
type GuestRead struct {
	ID              uuid.UUID `db:"id" json:"id" format:"uuid"`
	FullName        string    `db:"full_name" json:"full_name"`
	Email           *string   `db:"email" json:"email" nullable:"true"`
	Phone           *string   `db:"phone" json:"phone" nullable:"true"`
	DocumentType    *string   `db:"document_type" json:"document_type" nullable:"true" enum:"dpi,passport"`
	DocumentNumber  *string   `db:"document_number" json:"document_number" nullable:"true"`
	Nationality     *string   `db:"nationality" json:"nationality" nullable:"true" doc:"ISO 3166-1 alfa-2."`
	Notes           *string   `db:"notes" json:"notes" nullable:"true"`
	Blacklisted     bool      `db:"blacklisted" json:"blacklisted"`
	BlacklistReason *string   `db:"blacklist_reason" json:"blacklist_reason" nullable:"true"`
	Stays           int       `db:"stays" json:"stays" doc:"Estancias terminadas."`
	LastStay        *string   `db:"last_stay" json:"last_stay" format:"date" nullable:"true" doc:"Ultima salida."`
	NextStay        *string   `db:"next_stay" json:"next_stay" format:"date" nullable:"true" doc:"Proxima llegada."`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time `db:"updated_at" json:"updated_at"`
}

// GuestDocument son los datos de una foto del documento (sin URL).
type GuestDocument struct {
	FileID      uuid.UUID  `db:"id" json:"file_id" format:"uuid"`
	Kind        string     `db:"kind" json:"kind" enum:"id_front,id_back"`
	ContentType string     `db:"content_type" json:"content_type"`
	SizeBytes   int64      `db:"size_bytes" json:"size_bytes"`
	CreatedAt   time.Time  `db:"created_at" json:"created_at"`
	ExpiresAt   *time.Time `db:"expires_at" json:"expires_at" nullable:"true" doc:"Se borra sola: salida + 90 dias."`
}

// GuestDetail es la ficha del huesped: estancias y fotos del documento.
type GuestDetail struct {
	GuestRead
	Reservations []ReservationRead `json:"reservations" doc:"Sus estancias, las mas recientes primero."`
	Documents    []GuestDocument   `json:"documents" doc:"Fotos vigentes del documento (ver con guests.documents)."`
}

// GuestCreate es el cuerpo de POST /guests y el huesped nuevo de una reserva.
type GuestCreate struct {
	_              struct{} `additionalProperties:"true"`
	FullName       string   `json:"full_name" minLength:"2" maxLength:"160"`
	Email          *string  `json:"email,omitempty" maxLength:"320" format:"email"`
	Phone          *string  `json:"phone,omitempty" maxLength:"32"`
	DocumentType   *string  `json:"document_type,omitempty" enum:"dpi,passport"`
	DocumentNumber *string  `json:"document_number,omitempty" maxLength:"32"`
	Nationality    *string  `json:"nationality,omitempty" minLength:"2" maxLength:"2"`
	Notes          *string  `json:"notes,omitempty" maxLength:"2000"`
}

// GuestUpdate es el cuerpo de PATCH /guests/{id}.
type GuestUpdate struct {
	_               struct{}               `additionalProperties:"true"`
	FullName        *string                `json:"full_name,omitempty" minLength:"2" maxLength:"160"`
	Email           optional.Value[string] `json:"email,omitzero" maxLength:"320"`
	Phone           optional.Value[string] `json:"phone,omitzero" maxLength:"32"`
	DocumentType    optional.Value[string] `json:"document_type,omitzero"`
	DocumentNumber  optional.Value[string] `json:"document_number,omitzero" maxLength:"32"`
	Nationality     optional.Value[string] `json:"nationality,omitzero" maxLength:"2"`
	Notes           optional.Value[string] `json:"notes,omitzero" maxLength:"2000"`
	Blacklisted     *bool                  `json:"blacklisted,omitempty" doc:"Requiere guests.manage."`
	BlacklistReason optional.Value[string] `json:"blacklist_reason,omitzero" maxLength:"500"`
}

// GuestDocumentAttach es el cuerpo de POST /guests/{id}/documents.
type GuestDocumentAttach struct {
	_      struct{}  `additionalProperties:"true"`
	Kind   string    `json:"kind" enum:"id_front,id_back"`
	FileID uuid.UUID `json:"file_id" format:"uuid" doc:"Archivo guests/id_front o guests/id_back ya confirmado."`
}

// DocumentURL es la descarga firmada de una foto del documento.
type DocumentURL struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// GuestFilter son los filtros del directorio.
type GuestFilter struct {
	Query       string
	Blacklisted *bool
	Cursor      string
	Limit       int
}

// --- Reservas --------------------------------------------------------------

// ReservationRead es una reserva en listas y calendario.
type ReservationRead struct {
	ID              uuid.UUID  `db:"id" json:"id" format:"uuid"`
	Code            string     `db:"code" json:"code" doc:"Codigo corto para el huesped (ARG-7K3P9Q)."`
	PropertyID      uuid.UUID  `db:"property_id" json:"property_id" format:"uuid"`
	PropertyCode    string     `db:"property_code" json:"property_code"`
	PropertyName    string     `db:"property_name" json:"property_name"`
	ChannelID       uuid.UUID  `db:"channel_id" json:"channel_id" format:"uuid"`
	ChannelName     string     `db:"channel_name" json:"channel_name"`
	ChannelKind     string     `db:"channel_kind" json:"channel_kind"`
	RequiresDeposit bool       `db:"requires_deposit" json:"requires_deposit"`
	HasCheckin      bool       `db:"has_checkin" json:"has_checkin"`
	Source          string     `db:"source" json:"source" enum:"manual,ical,api"`
	ExternalRef     *string    `db:"external_ref" json:"external_ref" nullable:"true" doc:"UID del evento iCal o id del canal."`
	ExternalSummary *string    `db:"external_summary" json:"external_summary" nullable:"true"`
	PrimaryGuestID  *uuid.UUID `db:"primary_guest_id" json:"primary_guest_id" format:"uuid" nullable:"true"`
	GuestName       *string    `db:"guest_name" json:"guest_name" nullable:"true"`
	GuestEmail      *string    `db:"guest_email" json:"guest_email" nullable:"true" doc:"Solo con guests.read."`
	GuestsCount     int        `db:"guests_count" json:"guests_count"`
	GuestsPending   int        `db:"guests_pending" json:"guests_pending" doc:"Huespedes sin registro completo (documento y fotos)."`
	Status          string     `db:"status" json:"status" enum:"pending,confirmed,checked_in,checked_out,cancelled,no_show"`
	Incomplete      bool       `db:"incomplete" json:"incomplete" doc:"Sin huesped principal: hay que completarla (importada)."`
	CheckInDate     string     `db:"check_in_date" json:"check_in_date" format:"date"`
	CheckOutDate    string     `db:"check_out_date" json:"check_out_date" format:"date" doc:"Exclusiva: noches = salida - entrada."`
	CheckInTime     string     `db:"check_in_time" json:"check_in_time" doc:"HH:MM"`
	CheckOutTime    string     `db:"check_out_time" json:"check_out_time" doc:"HH:MM"`
	Nights          int        `db:"nights" json:"nights"`
	CheckedInAt     *time.Time `db:"checked_in_at" json:"checked_in_at" nullable:"true"`
	CheckedOutAt    *time.Time `db:"checked_out_at" json:"checked_out_at" nullable:"true"`
	Adults          int        `db:"adults" json:"adults"`
	Children        int        `db:"children" json:"children"`
	TotalAmount     *float64   `db:"total_amount" json:"total_amount" nullable:"true"`
	Currency        string     `db:"currency" json:"currency"`
	DepositAmount   *float64   `db:"deposit_amount" json:"deposit_amount" nullable:"true"`
	DepositStatus   string     `db:"deposit_status" json:"deposit_status" enum:"none,pending,received,returned,retained_partial,retained_full"`
	Notes           *string    `db:"notes" json:"notes" nullable:"true"`
	CancelledAt     *time.Time `db:"cancelled_at" json:"cancelled_at" nullable:"true"`
	CancelReason    *string    `db:"cancel_reason" json:"cancel_reason" nullable:"true"`
	CreatedAt       time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time  `db:"updated_at" json:"updated_at"`
	ManualFields    []string   `db:"manual_fields" json:"-"`
}

// ReservationGuest es un huesped de la reserva con el estado de su registro.
type ReservationGuest struct {
	GuestRead
	IsPrimary  bool            `json:"is_primary"`
	Registered bool            `json:"registered" doc:"Tiene correo, documento y las dos fotos."`
	Missing    []string        `json:"missing" doc:"Lo que le falta: email, document, id_front, id_back."`
	Documents  []GuestDocument `json:"documents" doc:"Fotos vigentes del documento (vacio sin guests.read). La URL se pide aparte con guests.documents."`
}

// EventRead es un paso de la linea de tiempo.
type EventRead struct {
	ID           uuid.UUID      `db:"id" json:"id" format:"uuid"`
	Type         string         `db:"type" json:"type"`
	ActorUserID  *uuid.UUID     `db:"actor_user_id" json:"actor_user_id" format:"uuid" nullable:"true"`
	ActorName    *string        `db:"actor_name" json:"actor_name" nullable:"true" doc:"Vacio = el sistema o el huesped."`
	ActorGuestID *uuid.UUID     `db:"actor_guest_id" json:"actor_guest_id" format:"uuid" nullable:"true"`
	Data         map[string]any `db:"data" json:"data"`
	CreatedAt    time.Time      `db:"created_at" json:"created_at"`
}

// ReservationDetail es la ficha (W-H3).
type ReservationDetail struct {
	ReservationRead
	Guests      []ReservationGuest `json:"guests"`
	Events      []EventRead        `json:"events"`
	Deposit     DepositRead        `json:"deposit" doc:"Vacio (available = false) sin deposits.read."`
	Inspections []InspectionRead   `json:"inspections"`
}

// ReservationCreate es el cuerpo de POST /reservations (W-H4).
type ReservationCreate struct {
	_              struct{}     `additionalProperties:"true"`
	PropertyID     uuid.UUID    `json:"property_id" format:"uuid"`
	ChannelID      uuid.UUID    `json:"channel_id" format:"uuid"`
	CheckInDate    string       `json:"check_in_date" format:"date"`
	CheckOutDate   string       `json:"check_out_date" format:"date"`
	CheckInTime    *string      `json:"check_in_time,omitempty" maxLength:"5" doc:"HH:MM; por defecto la de la empresa."`
	CheckOutTime   *string      `json:"check_out_time,omitempty" maxLength:"5"`
	Adults         *int         `json:"adults,omitempty" minimum:"0" maximum:"50" doc:"Por defecto 1."`
	Children       int          `json:"children,omitempty" minimum:"0" maximum:"50"`
	TotalAmount    *float64     `json:"total_amount,omitempty" minimum:"0"`
	DepositAmount  *float64     `json:"deposit_amount,omitempty" minimum:"0" doc:"Por defecto el del canal si lo exige."`
	PrimaryGuestID *uuid.UUID   `json:"primary_guest_id,omitempty" format:"uuid"`
	Guest          *GuestCreate `json:"guest,omitempty" doc:"Huesped nuevo (si su documento ya existe, se usa el existente)."`
	ExternalRef    *string      `json:"external_ref,omitempty" maxLength:"255"`
	Notes          *string      `json:"notes,omitempty" maxLength:"4000"`
	SendEmail      *bool        `json:"send_email,omitempty" doc:"Correo de confirmacion al huesped; por defecto true."`
}

// ReservationUpdate es el cuerpo de PATCH /reservations/{id}.
type ReservationUpdate struct {
	_             struct{}                `additionalProperties:"true"`
	ChannelID     *uuid.UUID              `json:"channel_id,omitempty" format:"uuid"`
	CheckInDate   *string                 `json:"check_in_date,omitempty" format:"date"`
	CheckOutDate  *string                 `json:"check_out_date,omitempty" format:"date"`
	CheckInTime   *string                 `json:"check_in_time,omitempty" maxLength:"5"`
	CheckOutTime  *string                 `json:"check_out_time,omitempty" maxLength:"5"`
	Adults        *int                    `json:"adults,omitempty" minimum:"0" maximum:"50"`
	Children      *int                    `json:"children,omitempty" minimum:"0" maximum:"50"`
	TotalAmount   optional.Value[float64] `json:"total_amount,omitzero" minimum:"0"`
	DepositAmount optional.Value[float64] `json:"deposit_amount,omitzero" minimum:"0"`
	Notes         optional.Value[string]  `json:"notes,omitzero" maxLength:"4000"`
}

// ReservationConfirm es el cuerpo de POST /reservations/{id}/confirm.
type ReservationConfirm struct {
	_         struct{} `additionalProperties:"true"`
	SendEmail *bool    `json:"send_email,omitempty" doc:"Por defecto true."`
}

// Reason es el cuerpo de cancelar y no-show.
type Reason struct {
	_      struct{} `additionalProperties:"true"`
	Reason string   `json:"reason,omitempty" maxLength:"500" doc:"Obligatorio al cancelar."`
}

// GuestLink es el cuerpo de POST /reservations/{id}/guests: un huesped
// existente (guest_id) o uno nuevo (guest).
type GuestLink struct {
	_         struct{}     `additionalProperties:"true"`
	GuestID   *uuid.UUID   `json:"guest_id,omitempty" format:"uuid"`
	Guest     *GuestCreate `json:"guest,omitempty"`
	IsPrimary bool         `json:"is_primary,omitempty"`
}

// ReservationFilter son los filtros de GET /reservations.
type ReservationFilter struct {
	From, To   string // estancias que tocan [From, To)
	PropertyID *uuid.UUID
	ChannelID  *uuid.UUID
	GuestID    *uuid.UUID
	Statuses   []string
	Incomplete bool
	Query      string
	Desc       bool
	Cursor     string
	Limit      int
}

// --- Depositos ---------------------------------------------------------------

// DepositRead es el deposito de una reserva con sus movimientos.
type DepositRead struct {
	Available          bool        `json:"available" doc:"false sin deposits.read."`
	Amount             *float64    `json:"amount" nullable:"true"`
	Status             string      `json:"status" enum:"none,pending,received,returned,retained_partial,retained_full"`
	Received           float64     `json:"received"`
	Returned           float64     `json:"returned"`
	Retained           float64     `json:"retained"`
	Balance            float64     `json:"balance" doc:"Lo que se tiene del huesped: recibido - devuelto - retenido."`
	Charges            float64     `json:"charges" doc:"Costo estimado de los hallazgos con cargo al deposito."`
	InspectionRequired bool        `json:"inspection_required" doc:"Falta la inspeccion de salida para devolver o retener."`
	Transactions       []DepositTx `json:"transactions"`

	chargeFindings bool // la inspeccion de salida tiene hallazgos con cargo
}

// DepositTx es un movimiento del deposito (inmutable).
type DepositTx struct {
	ID               uuid.UUID `db:"id" json:"id" format:"uuid"`
	Type             string    `db:"type" json:"type" enum:"received,returned,retained"`
	Amount           float64   `db:"amount" json:"amount"`
	Method           string    `db:"method" json:"method" enum:"cash,transfer,card,other"`
	Reference        *string   `db:"reference" json:"reference" nullable:"true"`
	Notes            *string   `db:"notes" json:"notes" nullable:"true"`
	RegisteredByName *string   `db:"registered_by_name" json:"registered_by_name" nullable:"true"`
	CreatedAt        time.Time `db:"created_at" json:"created_at"`
}

// DepositTxCreate es el cuerpo de POST /reservations/{id}/deposit/transactions.
type DepositTxCreate struct {
	_         struct{} `additionalProperties:"true"`
	Type      string   `json:"type" enum:"received,returned,retained"`
	Amount    float64  `json:"amount" exclusiveMinimum:"0"`
	Method    string   `json:"method" enum:"cash,transfer,card,other"`
	Reference *string  `json:"reference,omitempty" maxLength:"128"`
	Notes     *string  `json:"notes,omitempty" maxLength:"1000"`
}

// --- Inspecciones ------------------------------------------------------------

// Photo es una foto con descarga firmada (1 h).
type Photo struct {
	FileID    uuid.UUID `json:"file_id" format:"uuid"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
}

// InspectionRead es una inspeccion antes de llegar o despues de salir.
type InspectionRead struct {
	ID              uuid.UUID     `db:"id" json:"id" format:"uuid"`
	Kind            string        `db:"kind" json:"kind" enum:"pre_arrival,post_departure"`
	PerformedBy     *uuid.UUID    `db:"performed_by" json:"performed_by" format:"uuid" nullable:"true"`
	PerformedByName *string       `db:"performed_by_name" json:"performed_by_name" nullable:"true"`
	PerformedAt     time.Time     `db:"performed_at" json:"performed_at"`
	TaskID          *uuid.UUID    `db:"task_id" json:"task_id" format:"uuid" nullable:"true"`
	Notes           *string       `db:"notes" json:"notes" nullable:"true"`
	Photos          []Photo       `db:"-" json:"photos"`
	Findings        []FindingRead `db:"-" json:"findings"`
}

// FindingRead es un hallazgo de la inspeccion.
type FindingRead struct {
	ID              uuid.UUID  `db:"id" json:"id" format:"uuid"`
	InspectionID    uuid.UUID  `db:"inspection_id" json:"-"`
	Area            string     `db:"area" json:"area"`
	Description     string     `db:"description" json:"description"`
	Severity        string     `db:"severity" json:"severity" enum:"low,medium,high"`
	EstimatedCost   *float64   `db:"estimated_cost" json:"estimated_cost" nullable:"true"`
	ChargeToDeposit bool       `db:"charge_to_deposit" json:"charge_to_deposit"`
	TicketID        *uuid.UUID `db:"ticket_id" json:"ticket_id" format:"uuid" nullable:"true" doc:"Incidencia creada con el hallazgo."`
	TicketNumber    *int       `db:"ticket_number" json:"ticket_number" nullable:"true"`
	CreatedAt       time.Time  `db:"created_at" json:"created_at"`
	Photos          []Photo    `db:"-" json:"photos"`
}

// InspectionCreate es el cuerpo de POST /reservations/{id}/inspections.
type InspectionCreate struct {
	_        struct{}    `additionalProperties:"true"`
	Kind     string      `json:"kind" enum:"pre_arrival,post_departure"`
	Notes    *string     `json:"notes,omitempty" maxLength:"4000"`
	TaskID   *uuid.UUID  `json:"task_id,omitempty" format:"uuid" doc:"Tarea de limpieza de la que salen las fotos (B)."`
	PhotoIDs []uuid.UUID `json:"photo_file_ids,omitempty" maxItems:"30" doc:"Archivos reservations/inspection_photo."`
}

// FindingCreate es el cuerpo de POST /inspections/{id}/findings.
type FindingCreate struct {
	_               struct{}    `additionalProperties:"true"`
	Area            string      `json:"area" minLength:"2" maxLength:"64" doc:"Sala, cocina, bano principal..."`
	Description     string      `json:"description" minLength:"3" maxLength:"2000"`
	Severity        string      `json:"severity" enum:"low,medium,high"`
	EstimatedCost   *float64    `json:"estimated_cost,omitempty" minimum:"0"`
	ChargeToDeposit bool        `json:"charge_to_deposit,omitempty"`
	PhotoIDs        []uuid.UUID `json:"photo_file_ids,omitempty" maxItems:"10"`
	CreateTicket    bool        `json:"create_ticket,omitempty" doc:"Crea una incidencia de mantenimiento con el hallazgo."`
}

// --- Inicio ------------------------------------------------------------------

// Today resume el dia para el Inicio y el encabezado de Reservas.
type Today struct {
	Available   bool   `json:"available"`
	Arrivals    int    `json:"arrivals" doc:"Llegan hoy (confirmadas o ya en casa)."`
	Departures  int    `json:"departures" doc:"Salen hoy."`
	InHouse     int    `json:"in_house" doc:"Estancias en curso."`
	Incomplete  int    `json:"incomplete" doc:"Importadas sin huesped por completar."`
	CheckinTime string `json:"checkin_time" doc:"Hora de entrada por defecto de la empresa."`
}
