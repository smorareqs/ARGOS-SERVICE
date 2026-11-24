package payroll

import (
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/optional"
)

// Settings es la configuracion de nomina de la empresa (W-J5).
type Settings struct {
	IGSSEmployeeRate      float64   `json:"igss_employee_rate" doc:"Cuota laboral del IGSS (0.0483 = 4.83 %)."`
	IGSSEmployerRate      float64   `json:"igss_employer_rate" doc:"Cuota patronal (IGSS, IRTRA e INTECAP); solo informativa en el Excel."`
	IncentiveBonusMonthly float64   `json:"incentive_bonus_monthly" doc:"Bonificacion incentivo mensual (Decreto 37-2001) para quienes estan en planilla; no paga IGSS."`
	Bonus14Month          int       `json:"bonus14_month" doc:"Mes en que se paga el Bono 14 (7 = julio)."`
	AguinaldoMonth        int       `json:"aguinaldo_month" doc:"Mes en que se paga el aguinaldo (12 = diciembre)."`
	MonthlyDays           int       `json:"monthly_days"`
	DailyHours            int       `json:"daily_hours"`
	MinMinutesForDay      int       `json:"min_minutes_for_day" doc:"Minutos trabajados para que un dia cuente (esquema por dia)."`
	UpdatedAt             time.Time `json:"updated_at"`
}

// SettingsInput reemplaza la configuracion (PUT).
type SettingsInput struct {
	IGSSEmployeeRate      float64 `json:"igss_employee_rate" minimum:"0" maximum:"0.5"`
	IGSSEmployerRate      float64 `json:"igss_employer_rate" minimum:"0" maximum:"0.5"`
	IncentiveBonusMonthly float64 `json:"incentive_bonus_monthly" minimum:"0" maximum:"100000"`
	Bonus14Month          int     `json:"bonus14_month" minimum:"1" maximum:"12"`
	AguinaldoMonth        int     `json:"aguinaldo_month" minimum:"1" maximum:"12"`
	MonthlyDays           int     `json:"monthly_days" minimum:"20" maximum:"31"`
	DailyHours            int     `json:"daily_hours" minimum:"1" maximum:"24"`
	MinMinutesForDay      int     `json:"min_minutes_for_day" minimum:"0" maximum:"1440"`
}

// PeriodRead es un periodo de nomina con sus totales (W-J2).
type PeriodRead struct {
	ID             uuid.UUID  `json:"id"`
	Name           string     `json:"name"`
	Kind           string     `json:"kind" enum:"biweekly,monthly"`
	PeriodStart    string     `json:"period_start" format:"date"`
	PeriodEnd      string     `json:"period_end" format:"date"`
	Status         string     `json:"status" enum:"draft,review,closed"`
	ComputedAt     *time.Time `json:"computed_at"`
	ReviewedAt     *time.Time `json:"reviewed_at"`
	ClosedAt       *time.Time `json:"closed_at"`
	ClosedByName   *string    `json:"closed_by_name"`
	EmployeesCount int        `json:"employees_count"`
	GrossTotal     float64    `json:"gross_total"`
	IGSSTotal      float64    `json:"igss_total"`
	NetTotal       float64    `json:"net_total"`
	Bonus14        bool       `json:"bonus14" doc:"El periodo paga Bono 14."`
	Aguinaldo      bool       `json:"aguinaldo" doc:"El periodo paga aguinaldo."`
	HasExport      bool       `json:"has_export" doc:"Al cerrar se guardo el Excel en S3."`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// PeriodCreate da de alta un periodo en borrador.
type PeriodCreate struct {
	Name        *string `json:"name,omitempty" maxLength:"64" doc:"Por defecto, \"Quincena 1-15 nov 2026\" o \"Noviembre 2026\"."`
	Kind        string  `json:"kind" enum:"biweekly,monthly"`
	PeriodStart string  `json:"period_start" format:"date"`
	PeriodEnd   string  `json:"period_end" format:"date"`
}

// Totals suma las lineas del periodo.
type Totals struct {
	Days            float64 `json:"days"`
	Hours           float64 `json:"hours"`
	Cleanings       int     `json:"cleanings"`
	Base            float64 `json:"base"`
	Overtime        float64 `json:"overtime"`
	Incentives      float64 `json:"incentives"`
	OtherEarnings   float64 `json:"other_earnings"`
	IncentiveBonus  float64 `json:"incentive_bonus"`
	Bonus14         float64 `json:"bonus14"`
	Aguinaldo       float64 `json:"aguinaldo"`
	Gross           float64 `json:"gross"`
	IGSS            float64 `json:"igss"`
	OtherDeductions float64 `json:"other_deductions"`
	Net             float64 `json:"net"`
	IGSSEmployer    float64 `json:"igss_employer" doc:"Cuota patronal estimada (informativa)."`
}

// PeriodDetail es el periodo con sus lineas (W-J3).
type PeriodDetail struct {
	PeriodRead
	Entries  []Line  `json:"entries"`
	Totals   Totals  `json:"totals"`
	Currency string  `json:"currency"`
	Sources  Sources `json:"sources" doc:"De donde salen los datos que aun dependen de otros modulos."`
}

// Sources dice si asistencia, tareas e incentivos ya existen en la empresa.
type Sources struct {
	Attendance bool `json:"attendance" doc:"false: dias y horas salen del horario semanal."`
	Tasks      bool `json:"tasks" doc:"false: las limpiezas quedan en 0 hasta que exista el itinerario."`
	Incentives bool `json:"incentives"`
}

// BonusDetail explica el calculo de Bono 14 o aguinaldo de una linea.
type BonusDetail struct {
	Kind            string  `json:"kind" enum:"bonus14,aguinaldo"`
	WindowStart     string  `json:"window_start" format:"date"`
	WindowEnd       string  `json:"window_end" format:"date"`
	MonthsHistory   int     `json:"months_history" doc:"Meses (aprox.) cubiertos por nomina cerrada en ARGOS."`
	MonthsEstimated int     `json:"months_estimated" doc:"Meses (aprox.) trabajados sin nomina en ARGOS: se estiman con el salario diario del historial o, sin historial, con el salario actual."`
	Earnings        float64 `json:"earnings" doc:"Salario ordinario devengado en la ventana (base + extras)."`
	Amount          float64 `json:"amount" doc:"Devengado / 12: salario diario × dias trabajados ÷ 365 × 12."`
}

// HistoryItem es un cambio manual en una linea.
type HistoryItem struct {
	Field     string    `json:"field"`
	From      float64   `json:"from"`
	To        float64   `json:"to"`
	ActorID   uuid.UUID `json:"actor_id"`
	ActorName string    `json:"actor_name"`
	At        time.Time `json:"at"`
	Note      *string   `json:"note"`
}

// Line es la linea de un colaborador en un periodo.
type Line struct {
	ID                   uuid.UUID     `json:"id"`
	PeriodID             uuid.UUID     `json:"period_id"`
	EmployeeID           uuid.UUID     `json:"employee_id"`
	UserID               uuid.UUID     `json:"user_id"`
	EmployeeCode         string        `json:"employee_code"`
	FullName             string        `json:"full_name"`
	Position             string        `json:"position"`
	PayType              string        `json:"pay_type" enum:"fixed_monthly,per_day,per_hour,per_cleaning"`
	PayRate              float64       `json:"pay_rate"`
	OnPayroll            bool          `json:"on_payroll"`
	AttendanceSource     string        `json:"attendance_source" enum:"attendance,schedule"`
	DaysWorked           float64       `json:"days_worked"`
	HoursWorked          float64       `json:"hours_worked"`
	CleaningsCount       int           `json:"cleanings_count"`
	BaseAmount           float64       `json:"base_amount"`
	OvertimeAmount       float64       `json:"overtime_amount"`
	IncentivesAmount     float64       `json:"incentives_amount"`
	OtherEarnings        float64       `json:"other_earnings"`
	IncentiveBonusAmount float64       `json:"incentive_bonus_amount"`
	Bonus14Amount        float64       `json:"bonus14_amount"`
	AguinaldoAmount      float64       `json:"aguinaldo_amount"`
	IGSSEmployee         float64       `json:"igss_employee"`
	OtherDeductions      float64       `json:"other_deductions"`
	GrossAmount          float64       `json:"gross_amount"`
	NetAmount            float64       `json:"net_amount"`
	BonusDetail          []BonusDetail `json:"bonus_detail"`
	ManualFields         []string      `json:"manual_fields"`
	History              []HistoryItem `json:"history"`
	Notes                *string       `json:"notes"`
	HasPayslip           bool          `json:"has_payslip"`
	UpdatedAt            time.Time     `json:"updated_at"`
}

// EntryUpdate corrige una linea en revision. Los dias, horas y limpiezas
// recalculan la base (salvo que la base se haya fijado a mano); IGSS, total y
// liquido se recalculan siempre.
type EntryUpdate struct {
	DaysWorked           optional.Value[float64] `json:"days_worked,omitzero" minimum:"0" maximum:"31"`
	HoursWorked          optional.Value[float64] `json:"hours_worked,omitzero" minimum:"0" maximum:"744"`
	CleaningsCount       optional.Value[int]     `json:"cleanings_count,omitzero" minimum:"0" maximum:"10000"`
	BaseAmount           optional.Value[float64] `json:"base_amount,omitzero" minimum:"0"`
	OvertimeAmount       optional.Value[float64] `json:"overtime_amount,omitzero" minimum:"0"`
	IncentivesAmount     optional.Value[float64] `json:"incentives_amount,omitzero" minimum:"0"`
	OtherEarnings        optional.Value[float64] `json:"other_earnings,omitzero" minimum:"0"`
	IncentiveBonusAmount optional.Value[float64] `json:"incentive_bonus_amount,omitzero" minimum:"0"`
	Bonus14Amount        optional.Value[float64] `json:"bonus14_amount,omitzero" minimum:"0"`
	AguinaldoAmount      optional.Value[float64] `json:"aguinaldo_amount,omitzero" minimum:"0"`
	OtherDeductions      optional.Value[float64] `json:"other_deductions,omitzero" minimum:"0"`
	Notes                optional.Value[string]  `json:"notes,omitzero" maxLength:"2000" doc:"null o vacio borra la nota."`
	Reason               *string                 `json:"reason,omitempty" maxLength:"500" doc:"Por que se corrige (queda en el historial)."`
}

// EmployeeEntry es una linea con su periodo (ficha del colaborador, W-D2).
type EmployeeEntry struct {
	Line
	PeriodName   string `json:"period_name"`
	PeriodStart  string `json:"period_start" format:"date"`
	PeriodEnd    string `json:"period_end" format:"date"`
	PeriodStatus string `json:"period_status" enum:"draft,review,closed"`
}

// MyPayslip es una boleta del colaborador (app, B20).
type MyPayslip struct {
	EmployeeEntry
	PayslipURL *string    `json:"payslip_url" doc:"URL firmada del PDF guardado al cerrar; si es null, usar GET /payroll/entries/{id}/payslip.pdf."`
	ExpiresAt  *time.Time `json:"expires_at"`
}

// File es un archivo generado (Excel o PDF).
type File struct {
	Name        string
	ContentType string
	Body        []byte
}
