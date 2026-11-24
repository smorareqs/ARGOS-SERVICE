package payroll

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// staffMember es un colaborador contratado en el periodo.
type staffMember struct {
	ID       uuid.UUID
	UserID   uuid.UUID
	Code     string
	Name     string
	Position string
	worker
}

// workload son los dias, horas y limpiezas de una persona en el periodo.
type workload struct {
	Days, Hours float64
	Cleanings   int
	Source      string
}

// computeInput reune lo que el calculo lee de la base.
type computeInput struct {
	period     span
	cfg        Settings
	staff      []staffMember
	work       map[uuid.UUID]workload
	cleanings  map[uuid.UUID]int
	incentives map[uuid.UUID]float64
	bonuses    []bonusPlan
}

// bonusPlan es un bono que paga el periodo con lo que ya se sabe de cada persona.
type bonusPlan struct {
	kind    string
	year    int
	month   int
	history map[uuid.UUID][]paidPeriod
	paid    map[uuid.UUID]bool
}

// Compute (re)calcula las lineas de un periodo en borrador con la asistencia
// (o el horario), las limpiezas validadas y los incentivos aprobados.
func (s *Service) Compute(ctx context.Context, actor authz.Actor, id uuid.UUID) (PeriodDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (PeriodDetail, error) {
		sp, status, err := lockPeriod(ctx, q, id)
		if err != nil {
			return PeriodDetail{}, err
		}
		switch status {
		case StatusClosed:
			return PeriodDetail{}, ErrClosed
		case StatusReview:
			return PeriodDetail{}, ErrNotDraft
		}
		in, err := s.gather(ctx, q, id, sp)
		if err != nil {
			return PeriodDetail{}, err
		}
		if _, err := q.Exec(ctx, `DELETE FROM payroll_entries WHERE period_id = $1`, id); err != nil {
			return PeriodDetail{}, err
		}
		for _, m := range in.staff {
			if err := insertEntry(ctx, q, id, m, in.line(m)); err != nil {
				return PeriodDetail{}, err
			}
		}
		if _, err := q.Exec(ctx, `UPDATE payroll_periods SET computed_at = now(), updated_at = now() WHERE id = $1`, id); err != nil {
			return PeriodDetail{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "payroll.period_computed", Entity: "payroll_period", EntityID: &id,
			After: map[string]any{"employees": len(in.staff)},
		}); err != nil {
			return PeriodDetail{}, err
		}
		return s.detail(ctx, q, id)
	})
}

func (s *Service) gather(ctx context.Context, q database.Querier, id uuid.UUID, sp span) (computeInput, error) {
	in := computeInput{period: sp}
	var err error
	if in.cfg, err = settings(ctx, q); err != nil {
		return in, err
	}
	t, err := s.tenant(ctx, q)
	if err != nil {
		return in, err
	}
	src, err := sources(ctx, q)
	if err != nil {
		return in, err
	}
	if in.staff, err = activeStaff(ctx, q, sp); err != nil {
		return in, err
	}
	if src.Attendance {
		in.work, err = attendanceWork(ctx, q, sp, t.Timezone, in.cfg.MinMinutesForDay)
	} else {
		in.work, err = scheduleWork(ctx, q, sp, in.staff, in.cfg)
	}
	if err != nil {
		return in, err
	}
	if in.cleanings, err = cleaningsDone(ctx, q, sp, t.Timezone, src.Tasks); err != nil {
		return in, err
	}
	if in.incentives, err = approvedIncentives(ctx, q, sp, src.Incentives); err != nil {
		return in, err
	}
	for _, b := range []struct {
		kind  string
		month int
	}{{"bonus14", in.cfg.Bonus14Month}, {"aguinaldo", in.cfg.AguinaldoMonth}} {
		year, ok := bonusYear(sp, b.month)
		if !ok {
			continue
		}
		plan, err := planBonus(ctx, q, id, b.kind, year, b.month)
		if err != nil {
			return in, err
		}
		in.bonuses = append(in.bonuses, plan)
	}
	return in, nil
}

// line calcula la linea de una persona.
func (in computeInput) line(m staffMember) lineValues {
	w, ok := in.work[m.ID]
	if !ok {
		w = workload{Source: "attendance"}
	}
	w.Cleanings = in.cleanings[m.ID]
	a := amounts{Incentives: round2(in.incentives[m.ID])}
	a.Base = baseAmount(in.period, m.worker, w.Days, w.Hours, w.Cleanings)
	a.IncentiveBonus = incentiveBonus(in.period, m.worker, in.cfg.IncentiveBonusMonthly)
	var details []BonusDetail
	monthly := monthlyEquivalent(in.period, m.worker, a.Base, a.Overtime, in.cfg.MonthlyDays)
	for _, b := range in.bonuses {
		if b.paid[m.ID] || !m.OnPayroll {
			continue
		}
		det := bonusAmount(b.kind, b.year, b.month, m.worker, b.history[m.ID], monthly)
		if det.Amount <= 0 {
			continue
		}
		details = append(details, det)
		if b.kind == "bonus14" {
			a.Bonus14 = det.Amount
		} else {
			a.Aguinaldo = det.Amount
		}
	}
	igss, gross, net := settle(a, m.OnPayroll, in.cfg.IGSSEmployeeRate)
	return lineValues{work: w, amounts: a, igss: igss, gross: gross, net: net, bonuses: details}
}

type lineValues struct {
	work             workload
	amounts          amounts
	igss, gross, net float64
	bonuses          []BonusDetail
}

func insertEntry(ctx context.Context, q database.Querier, periodID uuid.UUID, m staffMember, v lineValues) error {
	var detail any
	if len(v.bonuses) > 0 {
		detail = v.bonuses
	}
	_, err := q.Exec(ctx, `
		INSERT INTO payroll_entries (period_id, employee_id, employee_code, full_name, position, pay_type, pay_rate,
		       on_payroll, attendance_source, days_worked, hours_worked, cleanings_count, base_amount, overtime_amount,
		       incentives_amount, other_earnings, incentive_bonus_amount, bonus14_amount, aguinaldo_amount, igss_employee,
		       other_deductions, gross_amount, net_amount, bonus_detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)`,
		periodID, m.ID, m.Code, m.Name, m.Position, m.PayType, m.PayRate, m.OnPayroll, v.work.Source,
		round2(v.work.Days), round2(v.work.Hours), v.work.Cleanings, v.amounts.Base, v.amounts.Overtime,
		v.amounts.Incentives, v.amounts.OtherEarnings, v.amounts.IncentiveBonus, v.amounts.Bonus14, v.amounts.Aguinaldo,
		v.igss, v.amounts.OtherDeductions, v.gross, v.net, detail)
	return err
}

// activeStaff son las fichas laborales con contrato en el periodo.
func activeStaff(ctx context.Context, q database.Querier, sp span) ([]staffMember, error) {
	rows, err := q.Query(ctx, `
		SELECT em.id, em.user_id, em.employee_code, u.full_name, em.position, em.pay_type, em.pay_rate::float8,
		       em.on_payroll, em.hire_date, em.termination_date
		FROM employees em JOIN shared.users u ON u.id = em.user_id
		WHERE em.hire_date <= $2 AND (em.termination_date IS NULL OR em.termination_date >= $1)
		ORDER BY u.full_name, em.employee_code`, sp.Start, sp.End)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []staffMember
	for rows.Next() {
		var m staffMember
		if err := rows.Scan(&m.ID, &m.UserID, &m.Code, &m.Name, &m.Position, &m.PayType, &m.PayRate, &m.OnPayroll,
			&m.Hire, &m.Termination); err != nil {
			return nil, err
		}
		m.Hire = day(m.Hire)
		if m.Termination != nil {
			t := day(*m.Termination)
			m.Termination = &t
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// attendanceWork lee los marcajes cerrados (tabla de Persona B): un dia cuenta
// si suma al menos minMinutes; el marcaje es del dia en que empezo.
func attendanceWork(ctx context.Context, q database.Querier, sp span, tz string, minMinutes int) (map[uuid.UUID]workload, error) {
	rows, err := q.Query(ctx, `
		SELECT employee_id, sum(worked_minutes)::int
		FROM attendance_records
		WHERE clock_out_at IS NOT NULL AND (clock_in_at AT TIME ZONE $3)::date BETWEEN $1 AND $2
		GROUP BY employee_id, (clock_in_at AT TIME ZONE $3)::date`, sp.Start, sp.End, tz)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]workload{}
	for rows.Next() {
		var id uuid.UUID
		var minutes int
		if err := rows.Scan(&id, &minutes); err != nil {
			return nil, err
		}
		w := out[id]
		w.Source = "attendance"
		w.Hours += float64(minutes) / 60
		if minutes >= minMinutes {
			w.Days++
		}
		out[id] = w
	}
	return out, rows.Err()
}

// scheduleWork estima dias y horas con el horario semanal mientras no exista
// la asistencia: cada dia programado del contrato cuenta con sus minutos. Sin
// horario se asume lunes a viernes con las horas por dia de la configuracion.
func scheduleWork(ctx context.Context, q database.Querier, sp span, staff []staffMember, cfg Settings) (map[uuid.UUID]workload, error) {
	minutes, err := scheduleMinutes(ctx, q)
	if err != nil {
		return nil, err
	}
	week := map[int]int{1: cfg.DailyHours * 60, 2: cfg.DailyHours * 60, 3: cfg.DailyHours * 60, 4: cfg.DailyHours * 60, 5: cfg.DailyHours * 60}
	out := map[uuid.UUID]workload{}
	for _, m := range staff {
		plan := minutes[m.ID]
		if len(plan) == 0 {
			plan = week
		}
		out[m.ID] = plannedWork(sp, m, plan, cfg.MinMinutesForDay)
	}
	return out, nil
}

// plannedWork suma los dias y horas programados del contrato en el periodo.
func plannedWork(sp span, m staffMember, plan map[int]int, minMinutes int) workload {
	w := workload{Source: "schedule"}
	for d := sp.Start; !d.After(sp.End); d = d.AddDate(0, 0, 1) {
		if d.Before(m.Hire) || (m.Termination != nil && d.After(*m.Termination)) {
			continue
		}
		mins := plan[int(d.Weekday())]
		w.Hours += float64(mins) / 60
		if mins > 0 && mins >= minMinutes {
			w.Days++
		}
	}
	return w
}

// scheduleMinutes son los minutos programados por colaborador y dia de la semana.
func scheduleMinutes(ctx context.Context, q database.Querier) (map[uuid.UUID]map[int]int, error) {
	rows, err := q.Query(ctx, `
		SELECT employee_id, weekday, sum(extract(epoch FROM ends_at - starts_at) / 60)::int
		FROM employee_schedules GROUP BY employee_id, weekday`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	minutes := map[uuid.UUID]map[int]int{}
	for rows.Next() {
		var id uuid.UUID
		var wd, m int
		if err := rows.Scan(&id, &wd, &m); err != nil {
			return nil, err
		}
		if minutes[id] == nil {
			minutes[id] = map[int]int{}
		}
		minutes[id][wd] = m
	}
	return minutes, rows.Err()
}

// cleaningsDone cuenta las limpiezas validadas con fin dentro del periodo
// (tabla tasks de Persona B; 0 si aun no existe).
func cleaningsDone(ctx context.Context, q database.Querier, sp span, tz string, exists bool) (map[uuid.UUID]int, error) {
	out := map[uuid.UUID]int{}
	if !exists {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT em.id, count(*)::int
		FROM tasks t JOIN employees em ON em.user_id = t.assigned_to
		WHERE t.type = 'cleaning' AND t.status = 'validated' AND t.finished_at IS NOT NULL
		  AND (t.finished_at AT TIME ZONE $3)::date BETWEEN $1 AND $2
		GROUP BY em.id`, sp.Start, sp.End, tz)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// approvedIncentives suma los incentivos aprobados cuyo periodo termina dentro
// del de nomina (tabla incentive_awards de Persona B).
func approvedIncentives(ctx context.Context, q database.Querier, sp span, exists bool) (map[uuid.UUID]float64, error) {
	out := map[uuid.UUID]float64{}
	if !exists {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT employee_id, sum(amount)::float8 FROM incentive_awards
		WHERE status = 'approved' AND period_end BETWEEN $1 AND $2 GROUP BY employee_id`, sp.Start, sp.End)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var v float64
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

// planBonus lee el historial cerrado de la ventana del bono y quien ya lo
// cobro en otro periodo del mismo año.
func planBonus(ctx context.Context, q database.Querier, periodID uuid.UUID, kind string, year, month int) (bonusPlan, error) {
	plan := bonusPlan{kind: kind, year: year, month: month, history: map[uuid.UUID][]paidPeriod{}, paid: map[uuid.UUID]bool{}}
	start, end := bonusWindow(year, month)
	rows, err := q.Query(ctx, `
		SELECT e.employee_id, p.period_start, p.period_end, (e.base_amount + e.overtime_amount)::float8
		FROM payroll_entries e JOIN payroll_periods p ON p.id = e.period_id
		WHERE p.status = 'closed' AND p.period_start BETWEEN $1 AND $2`, start, end)
	if err != nil {
		return plan, err
	}
	for rows.Next() {
		var id uuid.UUID
		var h paidPeriod
		if err := rows.Scan(&id, &h.Start, &h.End, &h.Earnings); err != nil {
			rows.Close()
			return plan, err
		}
		h.Start, h.End = day(h.Start), day(h.End)
		plan.history[id] = append(plan.history[id], h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return plan, err
	}
	from := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	paid, err := q.Query(ctx, `
		SELECT DISTINCT e.employee_id FROM payroll_entries e JOIN payroll_periods p ON p.id = e.period_id
		WHERE p.id <> $1 AND p.period_start <= $3 AND p.period_end >= $2
		  AND (CASE WHEN $4::text = 'bonus14' THEN e.bonus14_amount ELSE e.aguinaldo_amount END) > 0`,
		periodID, from, from.AddDate(0, 0, 14), kind)
	if err != nil {
		return plan, err
	}
	defer paid.Close()
	for paid.Next() {
		var id uuid.UUID
		if err := paid.Scan(&id); err != nil {
			return plan, err
		}
		plan.paid[id] = true
	}
	return plan, paid.Err()
}
