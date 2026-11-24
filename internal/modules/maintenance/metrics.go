package maintenance

import (
	"context"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Metrics calcula los indicadores de mantenimiento del alcance (W-G5).
func (s *Service) Metrics(ctx context.Context, actor authz.Actor) (Metrics, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Metrics, error) {
		scope, err := scopeOf(ctx, q, actor)
		if err != nil {
			return Metrics{}, err
		}
		today, err := s.today(ctx, q)
		if err != nil {
			return Metrics{}, err
		}
		all, ids := scope.SQLArgs()
		m := Metrics{OpenByPlace: []PlaceCount{}, Resolution: []MonthHours{}, CostByCategory: []CategoryCost{}}
		if err := q.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE t.status IN ('open', 'assigned', 'in_progress'))::int,
			       count(*) FILTER (WHERE t.status IN ('open', 'assigned', 'in_progress') AND t.urgency IN ('high', 'critical'))::int,
			       count(*) FILTER (WHERE t.status IN ('open', 'assigned', 'in_progress') AND t.assigned_to IS NULL
			                          AND t.vendor_id IS NULL)::int,
			       (avg(extract(epoch FROM t.resolved_at - t.created_at) / 3600)
			          FILTER (WHERE t.resolved_at >= now() - interval '90 days'))::float8,
			       coalesce(sum(t.cost) FILTER (WHERE coalesce(t.resolved_at, t.created_at) >= date_trunc('month', now())), 0)::float8,
			       coalesce(sum(t.cost) FILTER (WHERE coalesce(t.resolved_at, t.created_at) >= date_trunc('year', now())), 0)::float8
			FROM maintenance_tickets t
			LEFT JOIN properties p ON p.id = t.property_id
			WHERE `+ticketScope, all, ids, actor.UserID).Scan(
			&m.Open, &m.Urgent, &m.Unassigned, &m.AvgHours, &m.CostMonth, &m.CostYear); err != nil {
			return Metrics{}, err
		}
		if err := q.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE mp.next_due_at < $3::date)::int,
			       count(*) FILTER (WHERE mp.next_due_at >= $3::date AND mp.next_due_at - mp.lead_days <= $3::date)::int
			FROM maintenance_plans mp JOIN assets a ON a.id = mp.asset_id
			WHERE mp.is_active AND a.is_active AND `+assetScope, all, ids, today).Scan(&m.OverduePlans, &m.DuePlans); err != nil {
			return Metrics{}, err
		}
		if m.OpenByPlace, err = openByPlace(ctx, q, all, ids, actor); err != nil {
			return Metrics{}, err
		}
		if m.Resolution, err = resolutionByMonth(ctx, q, all, ids, actor); err != nil {
			return Metrics{}, err
		}
		m.CostByCategory, err = costByCategory(ctx, q, all, ids, actor)
		return m, err
	})
}

func openByPlace(ctx context.Context, q database.Querier, all bool, ids any, actor authz.Actor) ([]PlaceCount, error) {
	rows, err := q.Query(ctx, `
		SELECT CASE WHEN t.property_id IS NULL THEN 'building' ELSE 'property' END,
		       coalesce(t.property_id, t.building_id), p.code, coalesce(p.name, b.name), count(*)::int
		FROM maintenance_tickets t
		LEFT JOIN properties p ON p.id = t.property_id
		LEFT JOIN buildings b ON b.id = t.building_id
		WHERE t.status IN ('open', 'assigned', 'in_progress') AND `+ticketScope+`
		GROUP BY 1, 2, 3, 4 ORDER BY 5 DESC, 4 LIMIT 10`, all, ids, actor.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlaceCount{}
	for rows.Next() {
		var c PlaceCount
		if err := rows.Scan(&c.Kind, &c.ID, &c.Code, &c.Name, &c.Open); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func resolutionByMonth(ctx context.Context, q database.Querier, all bool, ids any, actor authz.Actor) ([]MonthHours, error) {
	rows, err := q.Query(ctx, `
		WITH months AS (
			SELECT generate_series(date_trunc('month', now()) - interval '5 months', date_trunc('month', now()),
			                       interval '1 month') AS m)
		SELECT to_char(months.m, 'YYYY-MM'),
		       (SELECT avg(extract(epoch FROM t.resolved_at - t.created_at) / 3600)::float8
		          FROM maintenance_tickets t LEFT JOIN properties p ON p.id = t.property_id
		         WHERE t.resolved_at >= months.m AND t.resolved_at < months.m + interval '1 month' AND `+ticketScope+`),
		       (SELECT count(*)::int
		          FROM maintenance_tickets t LEFT JOIN properties p ON p.id = t.property_id
		         WHERE t.resolved_at >= months.m AND t.resolved_at < months.m + interval '1 month' AND `+ticketScope+`)
		FROM months ORDER BY months.m`, all, ids, actor.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MonthHours{}
	for rows.Next() {
		var h MonthHours
		if err := rows.Scan(&h.Month, &h.Hours, &h.Resolved); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func costByCategory(ctx context.Context, q database.Querier, all bool, ids any, actor authz.Actor) ([]CategoryCost, error) {
	rows, err := q.Query(ctx, `
		SELECT coalesce(a.category, 'none'), coalesce(sum(t.cost), 0)::float8, count(*)::int
		FROM maintenance_tickets t
		LEFT JOIN assets a ON a.id = t.asset_id
		LEFT JOIN properties p ON p.id = t.property_id
		WHERE t.cost > 0 AND coalesce(t.resolved_at, t.created_at) >= date_trunc('year', now()) AND `+ticketScope+`
		GROUP BY 1 ORDER BY 2 DESC`, all, ids, actor.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CategoryCost{}
	for rows.Next() {
		var c CategoryCost
		if err := rows.Scan(&c.Category, &c.Cost, &c.Tickets); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Upcoming devuelve los n preventivos activos que vencen en los proximos days
// dias (incluye los vencidos), con una transaccion ya abierta (Inicio).
func (s *Service) Upcoming(ctx context.Context, q database.Querier, scope authz.PropertyScope, days, n int) ([]PlanRead, error) {
	today, err := s.today(ctx, q)
	if err != nil {
		return nil, err
	}
	all, ids := scope.SQLArgs()
	plans, err := listPlans(ctx, q, today, `mp.is_active AND a.is_active AND mp.next_due_at <= $1::date + $4::int
		AND ($2::bool OR a.property_id = ANY($3::uuid[])
		     OR a.building_id IN (SELECT building_id FROM properties WHERE id = ANY($3::uuid[]) AND building_id IS NOT NULL))`,
		all, ids, days)
	if err != nil || len(plans) <= n {
		return plans, err
	}
	return plans[:n], nil
}

// IncidentCounts cuenta las incidencias abiertas del alcance (Inicio).
func IncidentCounts(ctx context.Context, q database.Querier, scope authz.PropertyScope, actor authz.Actor) (open, urgent, unassigned int, err error) {
	all, ids := scope.SQLArgs()
	err = q.QueryRow(ctx, `
		SELECT count(*)::int, count(*) FILTER (WHERE t.urgency IN ('high', 'critical'))::int,
		       count(*) FILTER (WHERE t.assigned_to IS NULL AND t.vendor_id IS NULL)::int
		FROM maintenance_tickets t
		WHERE t.status IN ('open', 'assigned', 'in_progress') AND `+ticketScope, all, ids, actor.UserID).
		Scan(&open, &urgent, &unassigned)
	return open, urgent, unassigned, err
}
