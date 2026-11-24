package employees

import (
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/optional"
)

// Contrato HTTP del perfil laboral (fase02.md §4, W-D2 sin RRHH). Lo lee y
// escribe quien tiene users.manage. Los datos de nomina y asistencia llegan
// en la fase 5.

// ScheduleBlock es un bloque del horario semanal.
type ScheduleBlock struct {
	_        struct{} `additionalProperties:"true"`
	Weekday  int      `json:"weekday" minimum:"0" maximum:"6" doc:"0 = domingo, 1 = lunes ... 6 = sabado."`
	StartsAt string   `json:"starts_at" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" doc:"HH:MM en la zona de la empresa."`
	EndsAt   string   `json:"ends_at" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$"`
}

// EmployeeRead es la ficha laboral de una persona de la empresa.
type EmployeeRead struct {
	ID                uuid.UUID       `db:"id" json:"id" format:"uuid"`
	UserID            uuid.UUID       `db:"user_id" json:"user_id" format:"uuid"`
	FullName          string          `db:"full_name" json:"full_name"`
	Email             string          `db:"email" json:"email"`
	EmployeeCode      string          `db:"employee_code" json:"employee_code"`
	Position          string          `db:"position" json:"position"`
	HireDate          string          `db:"hire_date" json:"hire_date" format:"date"`
	TerminationDate   *string         `db:"termination_date" json:"termination_date" format:"date" nullable:"true"`
	DocumentType      *string         `db:"document_type" json:"document_type" enum:"dpi,passport" nullable:"true"`
	DocumentNumber    *string         `db:"document_number" json:"document_number" nullable:"true"`
	IGSSNumber        *string         `db:"igss_number" json:"igss_number" nullable:"true"`
	NIT               *string         `db:"nit" json:"nit" nullable:"true"`
	PayType           string          `db:"pay_type" json:"pay_type" enum:"fixed_monthly,per_day,per_hour,per_cleaning"`
	PayRate           float64         `db:"pay_rate" json:"pay_rate" doc:"Monto por unidad del esquema, en la moneda de la empresa."`
	OnPayroll         bool            `db:"on_payroll" json:"on_payroll" doc:"En planilla (IGSS, prestaciones)."`
	EmergencyName     *string         `db:"emergency_name" json:"emergency_name" nullable:"true" doc:"Contacto de emergencia."`
	EmergencyPhone    *string         `db:"emergency_phone" json:"emergency_phone" nullable:"true"`
	EmergencyRelation *string         `db:"emergency_relation" json:"emergency_relation" nullable:"true" doc:"Parentesco o relacion."`
	Notes             *string         `db:"notes" json:"notes" nullable:"true"`
	Schedule          []ScheduleBlock `db:"schedule" json:"schedule"`
	CreatedAt         time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time       `db:"updated_at" json:"updated_at"`
}

// EmployeeCreate es el cuerpo de POST /employees.
type EmployeeCreate struct {
	_                 struct{}        `additionalProperties:"true"`
	UserID            uuid.UUID       `json:"user_id" format:"uuid" doc:"Persona de la empresa (miembro)."`
	EmployeeCode      *string         `json:"employee_code,omitempty" maxLength:"32" nullable:"true" doc:"Sin valor se genera (EMP-0001...)."`
	Position          string          `json:"position" minLength:"1" maxLength:"64"`
	HireDate          string          `json:"hire_date" format:"date"`
	TerminationDate   *string         `json:"termination_date,omitempty" format:"date" nullable:"true"`
	DocumentType      *string         `json:"document_type,omitempty" enum:"dpi,passport" nullable:"true"`
	DocumentNumber    *string         `json:"document_number,omitempty" maxLength:"32" nullable:"true" doc:"DPI: 13 digitos."`
	IGSSNumber        *string         `json:"igss_number,omitempty" maxLength:"32" nullable:"true"`
	NIT               *string         `json:"nit,omitempty" maxLength:"16" nullable:"true"`
	PayType           string          `json:"pay_type" enum:"fixed_monthly,per_day,per_hour,per_cleaning"`
	PayRate           float64         `json:"pay_rate" minimum:"0" maximum:"9999999"`
	OnPayroll         bool            `json:"on_payroll,omitempty"`
	EmergencyName     *string         `json:"emergency_name,omitempty" maxLength:"128" nullable:"true"`
	EmergencyPhone    *string         `json:"emergency_phone,omitempty" maxLength:"32" nullable:"true"`
	EmergencyRelation *string         `json:"emergency_relation,omitempty" maxLength:"64" nullable:"true"`
	Notes             *string         `json:"notes,omitempty" maxLength:"2000" nullable:"true"`
	Schedule          []ScheduleBlock `json:"schedule,omitempty" maxItems:"42"`
}

// EmployeeUpdate es el cuerpo de PATCH /employees/{id}: solo cambia lo que
// viene; null borra los opcionales.
type EmployeeUpdate struct {
	_                 struct{}               `additionalProperties:"true"`
	EmployeeCode      *string                `json:"employee_code,omitempty" minLength:"1" maxLength:"32"`
	Position          *string                `json:"position,omitempty" minLength:"1" maxLength:"64"`
	HireDate          *string                `json:"hire_date,omitempty" format:"date"`
	TerminationDate   optional.Value[string] `json:"termination_date,omitzero" format:"date"`
	DocumentType      optional.Value[string] `json:"document_type,omitzero" enum:"dpi,passport"`
	DocumentNumber    optional.Value[string] `json:"document_number,omitzero" maxLength:"32"`
	IGSSNumber        optional.Value[string] `json:"igss_number,omitzero" maxLength:"32"`
	NIT               optional.Value[string] `json:"nit,omitzero" maxLength:"16"`
	PayType           *string                `json:"pay_type,omitempty" enum:"fixed_monthly,per_day,per_hour,per_cleaning"`
	PayRate           *float64               `json:"pay_rate,omitempty" minimum:"0" maximum:"9999999"`
	OnPayroll         *bool                  `json:"on_payroll,omitempty"`
	EmergencyName     optional.Value[string] `json:"emergency_name,omitzero" maxLength:"128"`
	EmergencyPhone    optional.Value[string] `json:"emergency_phone,omitzero" maxLength:"32"`
	EmergencyRelation optional.Value[string] `json:"emergency_relation,omitzero" maxLength:"64"`
	Notes             optional.Value[string] `json:"notes,omitzero" maxLength:"2000"`
}

// ScheduleSet es el cuerpo de PUT /employees/{id}/schedule.
type ScheduleSet struct {
	_      struct{}        `additionalProperties:"true"`
	Blocks []ScheduleBlock `json:"blocks" maxItems:"42"`
}
