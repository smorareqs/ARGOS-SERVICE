// Package payroll calcula la nomina por periodo (fase05.md §3, parte de
// Persona A): base segun el esquema de pago, bonificacion incentivo, Bono 14,
// aguinaldo e IGSS; revision con correcciones a mano e historial; cierre con
// Excel y boletas PDF guardados en S3.
//
// Asistencia, tareas e incentivos son de Persona B. El calculo los lee si sus
// tablas existen (attendance_records, tasks, incentive_awards); mientras
// tanto los dias y horas salen del horario semanal y las limpiezas quedan en
// 0 (se corrigen en revision).
package payroll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/storage"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// EventClosed se publica al cerrar un periodo (B notifica a cada colaborador).
const EventClosed = "payroll.closed"

// ClosedEvent es el Data de EventClosed.
type ClosedEvent struct {
	Period  PeriodRead
	Entries []ClosedEntry
}

// ClosedEntry identifica la boleta de cada colaborador.
type ClosedEntry struct {
	EntryID    uuid.UUID
	EmployeeID uuid.UUID
	UserID     uuid.UUID
	NetAmount  float64
}

// Errores del modulo.
var (
	ErrPeriodNotFound = apperr.New(apperr.KindNotFound, "El período no existe.")
	ErrEntryNotFound  = apperr.New(apperr.KindNotFound, "La línea no existe.")
	ErrNoEmployee     = apperr.New(apperr.KindNotFound, "No tienes ficha laboral en esta empresa.")
	ErrDates          = apperr.New(apperr.KindValidation, "Revisa las fechas: el fin debe ser igual o posterior al inicio y el período no puede pasar de dos meses.")
	ErrOverlap        = apperr.New(apperr.KindValidation, "Ya hay un período de ese tipo que se cruza con esas fechas.")
	ErrNotDraft       = apperr.New(apperr.KindAlreadyExists, "El período ya no está en borrador: vuelve a borrador para recalcularlo.")
	ErrClosed         = apperr.New(apperr.KindAlreadyExists, "El período está cerrado y no se puede cambiar.")
	ErrNotReview      = apperr.New(apperr.KindAlreadyExists, "Las líneas se corrigen con el período en revisión.")
	ErrNotComputed    = apperr.New(apperr.KindValidation, "Calcula el período antes de pasarlo a revisión.")
	ErrNothingToClose = apperr.New(apperr.KindValidation, "El período no tiene líneas.")
	ErrNoChanges      = apperr.New(apperr.KindValidation, "No hay cambios.")
)

// Service contiene los casos de uso de nomina.
type Service struct {
	db      *database.DB
	storage *storage.Service
	bus     *events.Bus
	log     *slog.Logger
	now     func() time.Time
}

// NewService crea el servicio.
func NewService(db *database.DB, st *storage.Service, bus *events.Bus, log *slog.Logger) *Service {
	return &Service{db: db, storage: st, bus: bus, log: log, now: time.Now}
}

// SetClock fija el reloj (pruebas).
func (s *Service) SetClock(now func() time.Time) {
	s.now = now
}

// tenantInfo es lo de la empresa que usan los calculos y documentos.
type tenantInfo struct {
	ID       uuid.UUID
	Name     string
	Timezone string
	Currency string
	Today    string
}

func (s *Service) tenant(ctx context.Context, q database.Querier) (tenantInfo, error) {
	var t tenantInfo
	err := q.QueryRow(ctx, `
		SELECT id, name, timezone, trim(currency),
		       to_char(($1::timestamptz AT TIME ZONE timezone)::date, 'YYYY-MM-DD')
		FROM shared.tenants WHERE schema_name = current_schema()`, s.now()).
		Scan(&t.ID, &t.Name, &t.Timezone, &t.Currency, &t.Today)
	return t, err
}

// --- Configuracion -------------------------------------------------------------

const selectSettings = `SELECT igss_employee_rate::float8, igss_employer_rate::float8, incentive_bonus_monthly::float8,
	bonus14_month, aguinaldo_month, monthly_days, daily_hours, min_minutes_for_day, updated_at
	FROM payroll_settings WHERE id = 1`

func settings(ctx context.Context, q database.Querier) (Settings, error) {
	var c Settings
	err := q.QueryRow(ctx, selectSettings).Scan(&c.IGSSEmployeeRate, &c.IGSSEmployerRate, &c.IncentiveBonusMonthly,
		&c.Bonus14Month, &c.AguinaldoMonth, &c.MonthlyDays, &c.DailyHours, &c.MinMinutesForDay, &c.UpdatedAt)
	return c, err
}

// Settings devuelve la configuracion de nomina.
func (s *Service) Settings(ctx context.Context, _ authz.Actor) (Settings, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Settings, error) {
		return settings(ctx, q)
	})
}

// UpdateSettings reemplaza la configuracion. Aplica desde el siguiente calculo.
func (s *Service) UpdateSettings(ctx context.Context, actor authz.Actor, in SettingsInput) (Settings, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Settings, error) {
		before, err := settings(ctx, q)
		if err != nil {
			return Settings{}, err
		}
		if _, err := q.Exec(ctx, `
			UPDATE payroll_settings SET igss_employee_rate = $1, igss_employer_rate = $2, incentive_bonus_monthly = $3,
			       bonus14_month = $4, aguinaldo_month = $5, monthly_days = $6, daily_hours = $7, min_minutes_for_day = $8,
			       updated_by = $9, updated_at = now()
			WHERE id = 1`,
			in.IGSSEmployeeRate, in.IGSSEmployerRate, in.IncentiveBonusMonthly, in.Bonus14Month, in.AguinaldoMonth,
			in.MonthlyDays, in.DailyHours, in.MinMinutesForDay, actor.UserID); err != nil {
			return Settings{}, err
		}
		after, err := settings(ctx, q)
		if err != nil {
			return Settings{}, err
		}
		return after, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "payroll.settings_updated", Entity: "payroll_settings",
			Before: before, After: after,
		})
	})
}

// --- Utilidades ----------------------------------------------------------------

// tableExists dice si una tabla de otro modulo ya existe en el schema de la empresa.
func tableExists(ctx context.Context, q database.Querier, name string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok)
	return ok, err
}

func sources(ctx context.Context, q database.Querier) (Sources, error) {
	var src Sources
	var err error
	if src.Attendance, err = tableExists(ctx, q, "attendance_records"); err != nil {
		return src, err
	}
	if src.Tasks, err = tableExists(ctx, q, "tasks"); err != nil {
		return src, err
	}
	src.Incentives, err = tableExists(ctx, q, "incentive_awards")
	return src, err
}

func overlapViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23P01"
}

func notFound(err error, e *apperr.Error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return e
	}
	return err
}

var monthsES = [...]string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}

// defaultName propone el nombre del periodo: "Quincena 1–15 nov 2026" o "Noviembre 2026".
func defaultName(kind string, start, end time.Time) string {
	if kind == KindMonthly && start.Day() == 1 && end.AddDate(0, 0, 1).Day() == 1 && start.Month() == end.Month() {
		m := monthsES[start.Month()-1]
		return fmt.Sprintf("%s%s %d", strings.ToUpper(m[:1]), m[1:], start.Year())
	}
	if start.Month() == end.Month() && start.Year() == end.Year() {
		label := "Quincena"
		if kind == KindMonthly {
			label = "Período"
		}
		return fmt.Sprintf("%s %d–%d %s %d", label, start.Day(), end.Day(), monthsES[start.Month()-1][:3], start.Year())
	}
	return fmt.Sprintf("Período %d %s – %d %s %d", start.Day(), monthsES[start.Month()-1][:3], end.Day(), monthsES[end.Month()-1][:3], end.Year())
}

// money formatea un monto como en Guatemala: "Q 1,234.56".
func money(currency string, v float64) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	cents := int64(round2(v)*100 + 0.5)
	whole, frac := cents/100, cents%100
	digits := fmt.Sprint(whole)
	var b strings.Builder
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	symbol := currency
	if currency == "GTQ" || currency == "" {
		symbol = "Q"
	}
	return fmt.Sprintf("%s%s %s.%02d", sign, symbol, b.String(), frac)
}

// longDate escribe "15 de noviembre de 2026".
func longDate(t time.Time) string {
	return fmt.Sprintf("%d de %s de %d", t.Day(), monthsES[t.Month()-1], t.Year())
}

func shortDate(t time.Time) string {
	return fmt.Sprintf("%d %s %d", t.Day(), monthsES[t.Month()-1][:3], t.Year())
}

// PayTypeLabel es el esquema de pago en palabras.
func PayTypeLabel(t string) string {
	switch t {
	case PayFixedMonthly:
		return "Salario mensual"
	case PayPerDay:
		return "Por día"
	case PayPerHour:
		return "Por hora"
	case PayPerCleaning:
		return "Por limpieza"
	}
	return t
}
