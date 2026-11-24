package payroll

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Estados del periodo.
const (
	StatusDraft  = "draft"
	StatusReview = "review"
	StatusClosed = "closed"
)

const selectPeriod = `
	SELECT p.id, p.name, p.kind, p.period_start, p.period_end, p.status, p.computed_at, p.reviewed_at,
	       p.closed_at, u.full_name, coalesce(t.n, 0), coalesce(t.gross, 0)::float8, coalesce(t.igss, 0)::float8,
	       coalesce(t.net, 0)::float8, p.export_file_id IS NOT NULL, p.created_at, p.updated_at
	FROM payroll_periods p
	LEFT JOIN shared.users u ON u.id = p.closed_by
	LEFT JOIN LATERAL (
		SELECT count(*) AS n, sum(gross_amount) AS gross, sum(igss_employee) AS igss, sum(net_amount) AS net
		FROM payroll_entries e WHERE e.period_id = p.id
	) t ON true`

func scanPeriod(row pgx.Row, cfg Settings) (PeriodRead, error) {
	var p PeriodRead
	var start, end time.Time
	err := row.Scan(&p.ID, &p.Name, &p.Kind, &start, &end, &p.Status, &p.ComputedAt, &p.ReviewedAt, &p.ClosedAt,
		&p.ClosedByName, &p.EmployeesCount, &p.GrossTotal, &p.IGSSTotal, &p.NetTotal, &p.HasExport, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	p.PeriodStart, p.PeriodEnd = start.Format(time.DateOnly), end.Format(time.DateOnly)
	sp := span{Kind: p.Kind, Start: day(start), End: day(end)}
	_, p.Bonus14 = bonusYear(sp, cfg.Bonus14Month)
	_, p.Aguinaldo = bonusYear(sp, cfg.AguinaldoMonth)
	return p, nil
}

func periodByID(ctx context.Context, q database.Querier, id uuid.UUID, cfg Settings) (PeriodRead, error) {
	p, err := scanPeriod(q.QueryRow(ctx, selectPeriod+` WHERE p.id = $1`, id), cfg)
	return p, notFound(err, ErrPeriodNotFound)
}

// lockPeriod bloquea el periodo para cambiarlo y devuelve su estado.
func lockPeriod(ctx context.Context, q database.Querier, id uuid.UUID) (span, string, error) {
	var sp span
	var status string
	err := q.QueryRow(ctx, `SELECT kind, period_start, period_end, status FROM payroll_periods WHERE id = $1 FOR UPDATE`, id).
		Scan(&sp.Kind, &sp.Start, &sp.End, &status)
	sp.Start, sp.End = day(sp.Start), day(sp.End)
	return sp, status, notFound(err, ErrPeriodNotFound)
}

// Periods lista los periodos, el mas reciente primero (W-J2).
func (s *Service) Periods(ctx context.Context, _ authz.Actor) ([]PeriodRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]PeriodRead, error) {
		cfg, err := settings(ctx, q)
		if err != nil {
			return nil, err
		}
		rows, err := q.Query(ctx, selectPeriod+` ORDER BY p.period_start DESC, p.kind LIMIT 200`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []PeriodRead{}
		for rows.Next() {
			p, err := scanPeriod(rows, cfg)
			if err != nil {
				return nil, err
			}
			out = append(out, p)
		}
		return out, rows.Err()
	})
}

// Period devuelve el periodo con sus lineas y totales (W-J3).
func (s *Service) Period(ctx context.Context, _ authz.Actor, id uuid.UUID) (PeriodDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (PeriodDetail, error) {
		return s.detail(ctx, q, id)
	})
}

func (s *Service) detail(ctx context.Context, q database.Querier, id uuid.UUID) (PeriodDetail, error) {
	cfg, err := settings(ctx, q)
	if err != nil {
		return PeriodDetail{}, err
	}
	p, err := periodByID(ctx, q, id, cfg)
	if err != nil {
		return PeriodDetail{}, err
	}
	t, err := s.tenant(ctx, q)
	if err != nil {
		return PeriodDetail{}, err
	}
	src, err := sources(ctx, q)
	if err != nil {
		return PeriodDetail{}, err
	}
	entries, err := entriesOf(ctx, q, `e.period_id = $1`, id)
	if err != nil {
		return PeriodDetail{}, err
	}
	return PeriodDetail{PeriodRead: p, Entries: entries, Totals: totalsOf(entries, cfg), Currency: t.Currency, Sources: src}, nil
}

func totalsOf(list []Line, cfg Settings) Totals {
	var t Totals
	employerBase := 0.0
	for _, e := range list {
		t.Days += e.DaysWorked
		t.Hours += e.HoursWorked
		t.Cleanings += e.CleaningsCount
		t.Base += e.BaseAmount
		t.Overtime += e.OvertimeAmount
		t.Incentives += e.IncentivesAmount
		t.OtherEarnings += e.OtherEarnings
		t.IncentiveBonus += e.IncentiveBonusAmount
		t.Bonus14 += e.Bonus14Amount
		t.Aguinaldo += e.AguinaldoAmount
		t.Gross += e.GrossAmount
		t.IGSS += e.IGSSEmployee
		t.OtherDeductions += e.OtherDeductions
		t.Net += e.NetAmount
		if e.OnPayroll {
			employerBase += e.BaseAmount + e.OvertimeAmount + e.OtherEarnings
		}
	}
	for _, v := range []*float64{
		&t.Days, &t.Hours, &t.Base, &t.Overtime, &t.Incentives, &t.OtherEarnings, &t.IncentiveBonus,
		&t.Bonus14, &t.Aguinaldo, &t.Gross, &t.IGSS, &t.OtherDeductions, &t.Net,
	} {
		*v = round2(*v)
	}
	t.IGSSEmployer = round2(employerBase * cfg.IGSSEmployerRate)
	return t
}

// CreatePeriod da de alta un periodo en borrador.
func (s *Service) CreatePeriod(ctx context.Context, actor authz.Actor, in PeriodCreate) (PeriodRead, error) {
	start, err1 := time.Parse(time.DateOnly, in.PeriodStart)
	end, err2 := time.Parse(time.DateOnly, in.PeriodEnd)
	if err1 != nil || err2 != nil || end.Before(start) || end.Sub(start) > 61*24*time.Hour {
		return PeriodRead{}, ErrDates.WithField("body.period_end")
	}
	name := defaultName(in.Kind, start, end)
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		name = strings.TrimSpace(*in.Name)
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (PeriodRead, error) {
		var id uuid.UUID
		err := q.QueryRow(ctx, `
			INSERT INTO payroll_periods (name, kind, period_start, period_end, created_by)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, name, in.Kind, start, end, actor.UserID).Scan(&id)
		if overlapViolation(err) {
			return PeriodRead{}, ErrOverlap.WithField("body.period_start")
		}
		if err != nil {
			return PeriodRead{}, err
		}
		cfg, err := settings(ctx, q)
		if err != nil {
			return PeriodRead{}, err
		}
		p, err := periodByID(ctx, q, id, cfg)
		if err != nil {
			return PeriodRead{}, err
		}
		return p, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "payroll.period_created", Entity: "payroll_period", EntityID: &id,
			After: map[string]any{"name": p.Name, "kind": p.Kind, "period_start": p.PeriodStart, "period_end": p.PeriodEnd},
		})
	})
}

// DeletePeriod borra un periodo en borrador (con sus lineas).
func (s *Service) DeletePeriod(ctx context.Context, actor authz.Actor, id uuid.UUID) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		_, status, err := lockPeriod(ctx, q, id)
		if err != nil {
			return err
		}
		if status != StatusDraft {
			return ErrNotDraft
		}
		var name string
		if err := q.QueryRow(ctx, `DELETE FROM payroll_periods WHERE id = $1 RETURNING name`, id).Scan(&name); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "payroll.period_deleted", Entity: "payroll_period", EntityID: &id,
			Before: map[string]any{"name": name},
		})
	})
}

// Review pasa el periodo calculado a revision: se pueden corregir las lineas.
func (s *Service) Review(ctx context.Context, actor authz.Actor, id uuid.UUID) (PeriodDetail, error) {
	return s.transition(ctx, actor, id, StatusDraft, StatusReview, "payroll.period_reviewed", func(q database.Querier) error {
		var n int
		var computed bool
		if err := q.QueryRow(ctx, `SELECT computed_at IS NOT NULL, (SELECT count(*) FROM payroll_entries WHERE period_id = $1)
			FROM payroll_periods WHERE id = $1`, id).Scan(&computed, &n); err != nil {
			return err
		}
		if !computed || n == 0 {
			return ErrNotComputed
		}
		_, err := q.Exec(ctx, `UPDATE payroll_periods SET reviewed_at = now() WHERE id = $1`, id)
		return err
	})
}

// Reopen devuelve un periodo en revision a borrador para recalcularlo (el
// calculo nuevo reemplaza las correcciones).
func (s *Service) Reopen(ctx context.Context, actor authz.Actor, id uuid.UUID) (PeriodDetail, error) {
	return s.transition(ctx, actor, id, StatusReview, StatusDraft, "payroll.period_reopened", nil)
}

func (s *Service) transition(ctx context.Context, actor authz.Actor, id uuid.UUID, from, to, action string, check func(database.Querier) error) (PeriodDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (PeriodDetail, error) {
		_, status, err := lockPeriod(ctx, q, id)
		if err != nil {
			return PeriodDetail{}, err
		}
		switch {
		case status == StatusClosed:
			return PeriodDetail{}, ErrClosed
		case status != from && from == StatusDraft:
			return PeriodDetail{}, ErrNotDraft
		case status != from:
			return PeriodDetail{}, ErrNotReview
		}
		if check != nil {
			if err := check(q); err != nil {
				return PeriodDetail{}, err
			}
		}
		if _, err := q.Exec(ctx, `UPDATE payroll_periods SET status = $2, updated_at = now() WHERE id = $1`, id, to); err != nil {
			return PeriodDetail{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: action, Entity: "payroll_period", EntityID: &id,
			Before: map[string]any{"status": from}, After: map[string]any{"status": to},
		}); err != nil {
			return PeriodDetail{}, err
		}
		return s.detail(ctx, q, id)
	})
}
