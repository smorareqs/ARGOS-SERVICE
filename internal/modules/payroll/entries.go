package payroll

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/optional"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

const entryColumns = `
	e.id, e.period_id, e.employee_id, em.user_id, e.employee_code, e.full_name, e.position, e.pay_type,
	e.pay_rate::float8 AS pay_rate, e.on_payroll, e.attendance_source, e.days_worked::float8 AS days_worked,
	e.hours_worked::float8 AS hours_worked, e.cleanings_count, e.base_amount::float8 AS base_amount,
	e.overtime_amount::float8 AS overtime_amount, e.incentives_amount::float8 AS incentives_amount,
	e.other_earnings::float8 AS other_earnings, e.incentive_bonus_amount::float8 AS incentive_bonus_amount,
	e.bonus14_amount::float8 AS bonus14_amount, e.aguinaldo_amount::float8 AS aguinaldo_amount,
	e.igss_employee::float8 AS igss_employee, e.other_deductions::float8 AS other_deductions,
	e.gross_amount::float8 AS gross_amount, e.net_amount::float8 AS net_amount,
	coalesce(e.bonus_detail, '[]'::jsonb) AS bonus_detail, e.manual_fields, e.history, e.notes,
	e.payslip_file_id IS NOT NULL AS has_payslip, e.updated_at`

func entriesOf(ctx context.Context, q database.Querier, where string, args ...any) ([]Line, error) {
	rows, err := q.Query(ctx, `SELECT `+entryColumns+`
		FROM payroll_entries e JOIN employees em ON em.id = e.employee_id
		WHERE `+where+` ORDER BY e.full_name, e.employee_code`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Line])
}

func entryByID(ctx context.Context, q database.Querier, periodID, id uuid.UUID) (Line, error) {
	list, err := entriesOf(ctx, q, `e.id = $1 AND e.period_id = $2`, id, periodID)
	if err != nil {
		return Line{}, err
	}
	if len(list) == 0 {
		return Line{}, ErrEntryNotFound
	}
	return list[0], nil
}

// editable son los campos que se corrigen a mano, en el orden del historial.
type editable struct {
	name  string
	value optional.Value[float64]
	field *float64
}

// UpdateEntry corrige una linea de un periodo en revision y deja historial.
func (s *Service) UpdateEntry(ctx context.Context, actor authz.Actor, periodID, id uuid.UUID, in EntryUpdate) (Line, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Line, error) {
		if err := requireReview(ctx, q, periodID); err != nil {
			return Line{}, err
		}
		e, err := entryByID(ctx, q, periodID, id)
		if err != nil {
			return Line{}, err
		}
		cfg, err := settings(ctx, q)
		if err != nil {
			return Line{}, err
		}
		var actorName string
		if err := q.QueryRow(ctx, `SELECT full_name FROM shared.users WHERE id = $1`, actor.UserID).Scan(&actorName); err != nil {
			return Line{}, err
		}
		before := e
		changes := applyEdits(&e, in, actor.UserID, actorName, s.now())
		notesChanged := applyNotes(&e, in.Notes)
		if len(changes) == 0 && !notesChanged {
			return Line{}, ErrNoChanges
		}
		a := amounts{
			Base: e.BaseAmount, Overtime: e.OvertimeAmount, Incentives: e.IncentivesAmount, OtherEarnings: e.OtherEarnings,
			IncentiveBonus: e.IncentiveBonusAmount, Bonus14: e.Bonus14Amount, Aguinaldo: e.AguinaldoAmount, OtherDeductions: e.OtherDeductions,
		}
		e.IGSSEmployee, e.GrossAmount, e.NetAmount = settle(a, e.OnPayroll, cfg.IGSSEmployeeRate)
		e.History = append(e.History, changes...)
		if err := saveEntry(ctx, q, e); err != nil {
			return Line{}, err
		}
		changed := map[string]any{"net_amount": e.NetAmount}
		for _, c := range changes {
			changed[c.Field] = c.To
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "payroll.entry_updated", Entity: "payroll_entry", EntityID: &id,
			Before: map[string]any{"employee": before.FullName, "net_amount": before.NetAmount}, After: changed,
		}); err != nil {
			return Line{}, err
		}
		return entryByID(ctx, q, periodID, id)
	})
}

// requireReview bloquea el periodo y exige que este en revision.
func requireReview(ctx context.Context, q database.Querier, periodID uuid.UUID) error {
	_, status, err := lockPeriod(ctx, q, periodID)
	switch {
	case err != nil:
		return err
	case status == StatusClosed:
		return ErrClosed
	case status == StatusDraft:
		return ErrNotReview
	}
	return nil
}

// applyNotes cambia la nota si viene distinta (null o vacio la borra).
func applyNotes(e *Line, in optional.Value[string]) bool {
	if !in.Sent {
		return false
	}
	var next *string
	if !in.Null {
		next = trimmedPtr(&in.Value)
	}
	if equalPtr(next, e.Notes) {
		return false
	}
	e.Notes = next
	return true
}

func saveEntry(ctx context.Context, q database.Querier, e Line) error {
	_, err := q.Exec(ctx, `
		UPDATE payroll_entries SET days_worked = $2, hours_worked = $3, cleanings_count = $4, base_amount = $5,
		       overtime_amount = $6, incentives_amount = $7, other_earnings = $8, incentive_bonus_amount = $9,
		       bonus14_amount = $10, aguinaldo_amount = $11, igss_employee = $12, other_deductions = $13,
		       gross_amount = $14, net_amount = $15, manual_fields = $16, history = $17, notes = $18, updated_at = now()
		WHERE id = $1`,
		e.ID, e.DaysWorked, e.HoursWorked, e.CleaningsCount, e.BaseAmount, e.OvertimeAmount, e.IncentivesAmount,
		e.OtherEarnings, e.IncentiveBonusAmount, e.Bonus14Amount, e.AguinaldoAmount, e.IGSSEmployee, e.OtherDeductions,
		e.GrossAmount, e.NetAmount, e.ManualFields, e.History, e.Notes)
	return err
}

// applyEdits aplica los cambios numericos y devuelve el historial. Si cambian
// dias, horas o limpiezas y la base no se fijo a mano, la base se recalcula
// (salvo en salario fijo, que no depende de ellos).
func applyEdits(e *Line, in EntryUpdate, actorID uuid.UUID, actorName string, now time.Time) []HistoryItem {
	cleanings := float64(e.CleaningsCount)
	fields := []editable{
		{"days_worked", in.DaysWorked, &e.DaysWorked},
		{"hours_worked", in.HoursWorked, &e.HoursWorked},
		{"cleanings_count", optional.Value[float64]{Sent: in.CleaningsCount.Sent, Null: in.CleaningsCount.Null, Value: float64(in.CleaningsCount.Value)}, &cleanings},
		{"base_amount", in.BaseAmount, &e.BaseAmount},
		{"overtime_amount", in.OvertimeAmount, &e.OvertimeAmount},
		{"incentives_amount", in.IncentivesAmount, &e.IncentivesAmount},
		{"other_earnings", in.OtherEarnings, &e.OtherEarnings},
		{"incentive_bonus_amount", in.IncentiveBonusAmount, &e.IncentiveBonusAmount},
		{"bonus14_amount", in.Bonus14Amount, &e.Bonus14Amount},
		{"aguinaldo_amount", in.AguinaldoAmount, &e.AguinaldoAmount},
		{"other_deductions", in.OtherDeductions, &e.OtherDeductions},
	}
	change := func(field string, from, to float64) HistoryItem {
		return HistoryItem{Field: field, From: from, To: to, ActorID: actorID, ActorName: actorName, At: now, Note: trimmedPtr(in.Reason)}
	}
	var out []HistoryItem
	quantity := false
	for _, f := range fields {
		v, ok := f.next()
		if !ok {
			continue
		}
		out = append(out, change(f.name, *f.field, v))
		*f.field = v
		if !slices.Contains(e.ManualFields, f.name) {
			e.ManualFields = append(e.ManualFields, f.name)
		}
		quantity = quantity || f.name == "days_worked" || f.name == "hours_worked" || f.name == "cleanings_count"
	}
	e.CleaningsCount = int(cleanings)
	if base, ok := rebase(*e, quantity); ok {
		out = append(out, change("base_amount", e.BaseAmount, base))
		e.BaseAmount = base
	}
	return out
}

// next devuelve el valor nuevo si vino y es distinto del actual.
func (f editable) next() (float64, bool) {
	if !f.value.Sent || f.value.Null {
		return 0, false
	}
	v := round2(f.value.Value)
	return v, v != *f.field
}

// rebase recalcula la base si cambiaron dias, horas o limpiezas, el esquema
// depende de ellos y nadie fijo la base a mano.
func rebase(e Line, quantity bool) (float64, bool) {
	if !quantity || e.PayType == PayFixedMonthly || slices.Contains(e.ManualFields, "base_amount") {
		return 0, false
	}
	base := baseAmount(span{}, worker{PayType: e.PayType, PayRate: e.PayRate}, e.DaysWorked, e.HoursWorked, e.CleaningsCount)
	return base, base != e.BaseAmount
}

// EmployeeEntries son las lineas de un colaborador en todos los periodos (W-D2).
func (s *Service) EmployeeEntries(ctx context.Context, _ authz.Actor, employeeID uuid.UUID) ([]EmployeeEntry, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]EmployeeEntry, error) {
		return employeeEntries(ctx, q, `e.employee_id = $1`, employeeID)
	})
}

func employeeEntries(ctx context.Context, q database.Querier, where string, args ...any) ([]EmployeeEntry, error) {
	rows, err := q.Query(ctx, `SELECT `+entryColumns+`, p.name AS period_name, to_char(p.period_start, 'YYYY-MM-DD') AS period_start,
		       to_char(p.period_end, 'YYYY-MM-DD') AS period_end, p.status AS period_status
		FROM payroll_entries e JOIN employees em ON em.id = e.employee_id JOIN payroll_periods p ON p.id = e.period_id
		WHERE `+where+` ORDER BY p.period_start DESC LIMIT 120`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[EmployeeEntry])
}

// Me son las boletas del colaborador en periodos cerrados (app, B20).
func (s *Service) Me(ctx context.Context, actor authz.Actor) ([]MyPayslip, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]MyPayslip, error) {
		var employeeID uuid.UUID
		if err := q.QueryRow(ctx, `SELECT id FROM employees WHERE user_id = $1`, actor.UserID).Scan(&employeeID); err != nil {
			return nil, notFound(err, ErrNoEmployee)
		}
		list, err := employeeEntries(ctx, q, `e.employee_id = $1 AND p.status = 'closed'`, employeeID)
		if err != nil {
			return nil, err
		}
		out := make([]MyPayslip, len(list))
		for i, e := range list {
			out[i] = MyPayslip{EmployeeEntry: e}
			if !e.HasPayslip {
				continue
			}
			var key string
			if err := q.QueryRow(ctx, `SELECT f.s3_key FROM payroll_entries e JOIN files f ON f.id = e.payslip_file_id
				WHERE e.id = $1 AND f.status = 'ready'`, e.ID).Scan(&key); err != nil {
				continue
			}
			url, err := s.storage.PresignDownload(ctx, actor.TenantID, key, 15*time.Minute)
			if err != nil {
				return nil, err
			}
			exp := s.now().Add(15 * time.Minute)
			out[i].PayslipURL, out[i].ExpiresAt = &url, &exp
		}
		return out, nil
	})
}

func trimmedPtr(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	return &t
}

func equalPtr(a, b *string) bool {
	a, b = trimmedPtr(a), trimmedPtr(b)
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
