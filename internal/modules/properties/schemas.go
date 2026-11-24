package properties

import (
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/optional"
)

// Contrato HTTP de propiedades y edificios (fase02.md §4). Lo consumen la web
// (W-C1 a W-C3) y la app (B1, ficha de propiedad y sync).

// PersonRef es una persona asignada o propietaria.
type PersonRef struct {
	UserID   uuid.UUID `json:"user_id" format:"uuid"`
	FullName string    `json:"full_name"`
}

// PropertyRead es una propiedad. Nunca trae la informacion de acceso: se pide
// aparte con GET /properties/{id}/access-info y queda en la bitacora.
type PropertyRead struct {
	ID                    uuid.UUID   `db:"id" json:"id" format:"uuid"`
	Code                  string      `db:"code" json:"code"`
	Name                  string      `db:"name" json:"name"`
	Type                  string      `db:"type" json:"type" enum:"apartment,house,room,other"`
	BuildingID            *uuid.UUID  `db:"building_id" json:"building_id" format:"uuid" nullable:"true"`
	BuildingName          *string     `db:"building_name" json:"building_name" nullable:"true"`
	BuildingZone          *string     `db:"building_zone" json:"building_zone" nullable:"true" doc:"Zona del edificio (si la propiedad no tiene la suya)."`
	Address               *string     `db:"address" json:"address" nullable:"true"`
	Zone                  *string     `db:"zone" json:"zone" nullable:"true" doc:"Barrio o ciudad (p. ej. Zona 10, Antigua Guatemala)."`
	Lat                   *float64    `db:"lat" json:"lat" nullable:"true"`
	Lng                   *float64    `db:"lng" json:"lng" nullable:"true"`
	Bedrooms              *int        `db:"bedrooms" json:"bedrooms" nullable:"true"`
	Bathrooms             *int        `db:"bathrooms" json:"bathrooms" nullable:"true"`
	MaxGuests             *int        `db:"max_guests" json:"max_guests" nullable:"true"`
	ChecklistTemplateID   *uuid.UUID  `db:"checklist_template_id" json:"checklist_template_id" format:"uuid" nullable:"true"`
	ChecklistTemplateName *string     `db:"checklist_template_name" json:"checklist_template_name" nullable:"true"`
	HasAccessInfo         bool        `db:"has_access_info" json:"has_access_info"`
	AccessInfoUpdatedAt   *time.Time  `db:"access_info_updated_at" json:"access_info_updated_at" nullable:"true"`
	CoverFileID           *uuid.UUID  `db:"cover_file_id" json:"cover_file_id" format:"uuid" nullable:"true"`
	CoverKey              *string     `db:"cover_key" json:"-"`
	CoverURL              *string     `db:"-" json:"cover_url" nullable:"true" doc:"Foto de portada firmada por 12 h."`
	Notes                 *string     `db:"notes" json:"notes" nullable:"true"`
	IsActive              bool        `db:"is_active" json:"is_active"`
	Assigned              []PersonRef `db:"assigned" json:"assigned" doc:"Colaboradores asignados (alcance de limpieza y mantenimiento)."`
	Owners                []PersonRef `db:"owners" json:"owners"`
	CreatedAt             time.Time   `db:"created_at" json:"created_at"`
	UpdatedAt             time.Time   `db:"updated_at" json:"updated_at"`
}

// PropertyCreate es el cuerpo de POST /properties.
type PropertyCreate struct {
	_                   struct{}   `additionalProperties:"true"`
	Code                string     `json:"code" minLength:"1" maxLength:"32" doc:"Corto y unico, visible en la app (p. ej. TB-504)."`
	Name                string     `json:"name" minLength:"1" maxLength:"128"`
	Type                string     `json:"type" enum:"apartment,house,room,other"`
	BuildingID          *uuid.UUID `json:"building_id,omitempty" format:"uuid" nullable:"true"`
	Address             *string    `json:"address,omitempty" maxLength:"500" nullable:"true"`
	Zone                *string    `json:"zone,omitempty" maxLength:"128" nullable:"true"`
	Lat                 *float64   `json:"lat,omitempty" minimum:"-90" maximum:"90" nullable:"true"`
	Lng                 *float64   `json:"lng,omitempty" minimum:"-180" maximum:"180" nullable:"true"`
	Bedrooms            *int       `json:"bedrooms,omitempty" minimum:"0" maximum:"99" nullable:"true"`
	Bathrooms           *int       `json:"bathrooms,omitempty" minimum:"0" maximum:"99" nullable:"true"`
	MaxGuests           *int       `json:"max_guests,omitempty" minimum:"0" maximum:"999" nullable:"true"`
	ChecklistTemplateID *uuid.UUID `json:"checklist_template_id,omitempty" format:"uuid" nullable:"true" doc:"Sin valor usa la plantilla por defecto."`
	AccessInfo          *string    `json:"access_info,omitempty" maxLength:"2000" nullable:"true" doc:"Llaves, codigos, wifi. Se guarda cifrada."`
	CoverFileID         *uuid.UUID `json:"cover_file_id,omitempty" format:"uuid" nullable:"true" doc:"Archivo properties/cover ya confirmado."`
	Notes               *string    `json:"notes,omitempty" maxLength:"2000" nullable:"true"`
}

// PropertyUpdate es el cuerpo de PATCH /properties/{id}: solo cambia lo que
// viene; null borra los campos opcionales.
type PropertyUpdate struct {
	_                   struct{}                  `additionalProperties:"true"`
	Code                *string                   `json:"code,omitempty" minLength:"1" maxLength:"32"`
	Name                *string                   `json:"name,omitempty" minLength:"1" maxLength:"128"`
	Type                *string                   `json:"type,omitempty" enum:"apartment,house,room,other"`
	BuildingID          optional.Value[uuid.UUID] `json:"building_id,omitzero" format:"uuid"`
	Address             optional.Value[string]    `json:"address,omitzero" maxLength:"500"`
	Zone                optional.Value[string]    `json:"zone,omitzero" maxLength:"128"`
	Lat                 optional.Value[float64]   `json:"lat,omitzero" minimum:"-90" maximum:"90"`
	Lng                 optional.Value[float64]   `json:"lng,omitzero" minimum:"-180" maximum:"180"`
	Bedrooms            optional.Value[int]       `json:"bedrooms,omitzero" minimum:"0" maximum:"99"`
	Bathrooms           optional.Value[int]       `json:"bathrooms,omitzero" minimum:"0" maximum:"99"`
	MaxGuests           optional.Value[int]       `json:"max_guests,omitzero" minimum:"0" maximum:"999"`
	ChecklistTemplateID optional.Value[uuid.UUID] `json:"checklist_template_id,omitzero" format:"uuid"`
	AccessInfo          optional.Value[string]    `json:"access_info,omitzero" maxLength:"2000"`
	CoverFileID         optional.Value[uuid.UUID] `json:"cover_file_id,omitzero" format:"uuid"`
	Notes               optional.Value[string]    `json:"notes,omitzero" maxLength:"2000"`
	IsActive            *bool                     `json:"is_active,omitempty"`
}

// AccessInfoRead es la informacion de acceso descifrada.
type AccessInfoRead struct {
	PropertyID uuid.UUID  `json:"property_id" format:"uuid"`
	Text       string     `json:"text"`
	UpdatedAt  *time.Time `json:"updated_at" nullable:"true"`
	ViewedAt   time.Time  `json:"viewed_at" doc:"Momento de esta consulta (queda en la bitacora)."`
}

// PeopleSet es el cuerpo de PUT /properties/{id}/assignments y /owners.
type PeopleSet struct {
	_       struct{}    `additionalProperties:"true"`
	UserIDs []uuid.UUID `json:"user_ids" maxItems:"200"`
}

// PropertySet es el cuerpo de PUT /property-assignments/{user_id} y
// /property-owners/{user_id}.
type PropertySet struct {
	_           struct{}    `additionalProperties:"true"`
	PropertyIDs []uuid.UUID `json:"property_ids" maxItems:"1000"`
}

// UserProperties es el conjunto de propiedades de una persona.
type UserProperties struct {
	UserID      uuid.UUID   `json:"user_id" format:"uuid"`
	PropertyIDs []uuid.UUID `json:"property_ids"`
}

// ListFilter filtra GET /properties.
type ListFilter struct {
	Query      string
	BuildingID *uuid.UUID
	Active     *bool
	AssigneeID *uuid.UUID
	OwnerID    *uuid.UUID
	Cursor     string
	Limit      int
}

// --- Edificios -------------------------------------------------------------

// BuildingRead es un edificio.
type BuildingRead struct {
	ID              uuid.UUID `db:"id" json:"id" format:"uuid"`
	Name            string    `db:"name" json:"name"`
	Address         *string   `db:"address" json:"address" nullable:"true"`
	Zone            *string   `db:"zone" json:"zone" nullable:"true"`
	Lat             *float64  `db:"lat" json:"lat" nullable:"true"`
	Lng             *float64  `db:"lng" json:"lng" nullable:"true"`
	Notes           *string   `db:"notes" json:"notes" nullable:"true"`
	IsActive        bool      `db:"is_active" json:"is_active"`
	PropertiesCount int       `db:"properties_count" json:"properties_count" doc:"Propiedades activas o inactivas (no borradas)."`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time `db:"updated_at" json:"updated_at"`
}

// BuildingCreate es el cuerpo de POST /buildings.
type BuildingCreate struct {
	_       struct{} `additionalProperties:"true"`
	Name    string   `json:"name" minLength:"1" maxLength:"128"`
	Address *string  `json:"address,omitempty" maxLength:"500" nullable:"true"`
	Zone    *string  `json:"zone,omitempty" maxLength:"128" nullable:"true"`
	Lat     *float64 `json:"lat,omitempty" minimum:"-90" maximum:"90" nullable:"true"`
	Lng     *float64 `json:"lng,omitempty" minimum:"-180" maximum:"180" nullable:"true"`
	Notes   *string  `json:"notes,omitempty" maxLength:"2000" nullable:"true"`
}

// BuildingUpdate es el cuerpo de PATCH /buildings/{id}.
type BuildingUpdate struct {
	_        struct{}                `additionalProperties:"true"`
	Name     *string                 `json:"name,omitempty" minLength:"1" maxLength:"128"`
	Address  optional.Value[string]  `json:"address,omitzero" maxLength:"500"`
	Zone     optional.Value[string]  `json:"zone,omitzero" maxLength:"128"`
	Lat      optional.Value[float64] `json:"lat,omitzero" minimum:"-90" maximum:"90"`
	Lng      optional.Value[float64] `json:"lng,omitzero" minimum:"-180" maximum:"180"`
	Notes    optional.Value[string]  `json:"notes,omitzero" maxLength:"2000"`
	IsActive *bool                   `json:"is_active,omitempty"`
}
