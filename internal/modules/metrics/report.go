package metrics

import (
	"context"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// ErrMonth: el mes va como AAAA-MM.
var ErrMonth = apperr.New(apperr.KindValidation, "El mes va como AAAA-MM.")

// PropertyReport es una propiedad en el reporte del mes (app E3).
type PropertyReport struct {
	Code               string  `db:"code" json:"code"`
	Name               string  `db:"name" json:"name"`
	NightsBooked       int     `db:"nights_booked" json:"nights_booked"`
	Occupancy          int     `db:"occupancy" json:"occupancy" doc:"0-100: noches ocupadas del mes."`
	Cleanings          int     `db:"cleanings" json:"cleanings"`
	Tickets            int     `db:"tickets" json:"tickets" doc:"Incidencias reportadas en el mes."`
	AvgResolutionHours float64 `db:"avg_resolution_hours" json:"avg_resolution_hours" doc:"De las resueltas en el mes."`
}

// ReportTotals resume el reporte.
type ReportTotals struct {
	Occupancy          int     `json:"occupancy"`
	Cleanings          int     `json:"cleanings"`
	Tickets            int     `json:"tickets"`
	AvgResolutionHours float64 `json:"avg_resolution_hours"`
}

// MonthReport es GET /metrics/report (propietario y admin).
type MonthReport struct {
	Month      string           `json:"month" doc:"AAAA-MM"`
	Days       int              `json:"days"`
	Totals     ReportTotals     `json:"totals"`
	Properties []PropertyReport `json:"properties"`
}

// Report arma el reporte del mes de las propiedades del alcance: ocupacion
// (reservas confirmadas o hechas), limpiezas terminadas e incidencias.
func (s *Service) Report(ctx context.Context, actor authz.Actor, month string) (MonthReport, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (MonthReport, error) {
		today, err := s.today(ctx, q)
		if err != nil {
			return MonthReport{}, err
		}
		if month == "" {
			month = today.Format("2006-01")
		}
		start, err := time.Parse("2006-01", month)
		if err != nil {
			return MonthReport{}, ErrMonth.WithField("month")
		}
		end := start.AddDate(0, 1, -1)
		days := end.Day()
		scope, err := authz.ResolvePropertyScope(ctx, q, actor)
		if err != nil {
			return MonthReport{}, err
		}
		all, ids := scope.SQLArgs()
		rows, err := q.Query(ctx, `
			WITH tz AS (SELECT `+tenantTZ+` AS name)
			SELECT p.code, p.name,
			       coalesce(occ.nights, 0)::int AS nights_booked,
			       round(100.0 * least(coalesce(occ.nights, 0), $5) / $5)::int AS occupancy,
			       coalesce(cl.n, 0)::int AS cleanings,
			       coalesce(tk.n, 0)::int AS tickets,
			       coalesce(round(tk.hours::numeric, 1), 0)::float8 AS avg_resolution_hours
			FROM properties p
			CROSS JOIN tz
			LEFT JOIN LATERAL (
				SELECT sum(greatest(0, least(r.check_out_date, $2::date + 1) - greatest(r.check_in_date, $1::date))) AS nights
				FROM reservations r
				WHERE r.property_id = p.id AND r.status IN ('confirmed', 'checked_in', 'checked_out')
				  AND r.check_in_date <= $2::date AND r.check_out_date > $1::date) occ ON true
			LEFT JOIN LATERAL (
				SELECT count(*) AS n FROM tasks t
				WHERE t.property_id = p.id AND t.type = 'cleaning' AND t.status IN ('done', 'validated')
				  AND (t.finished_at AT TIME ZONE tz.name)::date BETWEEN $1::date AND $2::date) cl ON true
			LEFT JOIN LATERAL (
				SELECT count(*) FILTER (WHERE (mt.created_at AT TIME ZONE tz.name)::date BETWEEN $1::date AND $2::date) AS n,
				       avg(extract(epoch FROM mt.resolved_at - mt.created_at) / 3600)
				         FILTER (WHERE (mt.resolved_at AT TIME ZONE tz.name)::date BETWEEN $1::date AND $2::date) AS hours
				FROM maintenance_tickets mt WHERE mt.property_id = p.id) tk ON true
			WHERE p.deleted_at IS NULL AND ($3::bool OR p.id = ANY($4::uuid[]))
			ORDER BY p.code`, start.Format(time.DateOnly), end.Format(time.DateOnly), all, ids, days)
		if err != nil {
			return MonthReport{}, err
		}
		list, err := pgx.CollectRows(rows, pgx.RowToStructByName[PropertyReport])
		if err != nil {
			return MonthReport{}, err
		}
		out := MonthReport{Month: month, Days: days, Properties: list}
		var occ, hours float64
		withHours := 0
		for _, p := range list {
			occ += float64(p.Occupancy)
			out.Totals.Cleanings += p.Cleanings
			out.Totals.Tickets += p.Tickets
			if p.AvgResolutionHours > 0 {
				hours += p.AvgResolutionHours
				withHours++
			}
		}
		if len(list) > 0 {
			out.Totals.Occupancy = int(math.Round(occ / float64(len(list))))
		}
		if withHours > 0 {
			out.Totals.AvgResolutionHours = round1(hours / float64(withHours))
		}
		return out, nil
	})
}
