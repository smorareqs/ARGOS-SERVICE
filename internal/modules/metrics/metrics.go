// Package metrics son las metricas de desempeno, las metas y los incentivos
// (fase05.md §2-3, Persona B, web W-K y app B21 y E3).
//
// Las metricas salen de las limpiezas terminadas (tasks de tipo cleaning en
// done o validated, del dia en que terminaron en la hora de la empresa). El
// job diario performance_snapshot guarda cada dia por colaborador. El job
// diario incentive_evaluation revisa las metas de la semana o el mes que
// acaba de cerrar y crea los incentivos pendientes; el admin los aprueba y la
// nomina (Persona A) los suma y los marca pagados al cerrar.
//
// Los rankings solo comparan colaboradores del mismo rol.
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/notifications"
)

// Trabajos del modulo (diarios, uno por empresa).
const (
	JobSnapshot = "performance_snapshot"
	JobEvaluate = "incentive_evaluation"
)

// Errores del modulo.
var (
	ErrRange      = apperr.New(apperr.KindValidation, "El rango va como 7d, 30d, week o month.")
	ErrNoEmployee = apperr.New(apperr.KindValidation, "Tu usuario no tiene ficha de colaborador.")
)

// PersonMetrics son las cifras de una persona en el rango.
type PersonMetrics struct {
	ID                  uuid.UUID  `db:"id" json:"id" format:"uuid" doc:"Usuario."`
	EmployeeID          *uuid.UUID `db:"employee_id" json:"employee_id" format:"uuid" nullable:"true"`
	Name                string     `db:"name" json:"name"`
	Role                string     `db:"role" json:"role"`
	RoleKey             string     `db:"role_key" json:"role_key"`
	Cleanings           int        `db:"cleanings" json:"cleanings"`
	OnTime              int        `db:"on_time" json:"on_time" doc:"0-100: terminadas sin retraso."`
	AvgMinutes          int        `db:"avg_minutes" json:"avg_minutes"`
	Late                int        `db:"late" json:"late"`
	Returned            int        `db:"returned" json:"returned" doc:"Veces que el supervisor la devolvio."`
	ChecklistCompliance float64    `db:"checklist_compliance" json:"checklist_compliance" doc:"0-100: puntos del checklist hechos."`
	TicketsReported     int        `db:"tickets_reported" json:"tickets_reported"`
	Rank                int        `db:"-" json:"rank" doc:"Puesto entre los de su rol (1 = mejor)."`
	RankOf              int        `db:"-" json:"rank_of" doc:"Cuantos hay en su rol."`
}

// DayCount es un dia de la serie.
type DayCount struct {
	Date  string `db:"date" json:"date" format:"date"`
	Count int    `db:"count" json:"count"`
}

// PropertyMetrics son las limpiezas de una propiedad.
type PropertyMetrics struct {
	Code       string `db:"code" json:"code"`
	Name       string `db:"name" json:"name"`
	Cleanings  int    `db:"cleanings" json:"cleanings"`
	AvgMinutes int    `db:"avg_minutes" json:"avg_minutes"`
}

// Board es GET /metrics/board (web W-K1 y app E3).
type Board struct {
	Range      string            `json:"range" enum:"7d,30d,week,month"`
	From       string            `json:"from" format:"date"`
	To         string            `json:"to" format:"date"`
	PerDay     []DayCount        `json:"per_day"`
	People     []PersonMetrics   `json:"people"`
	Properties []PropertyMetrics `json:"properties"`
	Totals     BoardTotals       `json:"totals"`
}

// BoardTotals resume el rango.
type BoardTotals struct {
	Cleanings  int `json:"cleanings"`
	OnTime     int `json:"on_time"`
	AvgMinutes int `json:"avg_minutes"`
	Late       int `json:"late"`
	Returned   int `json:"returned"`
}

// MyMetrics es GET /metrics/me (app B21).
type MyMetrics struct {
	Range    string         `json:"range"`
	From     string         `json:"from" format:"date"`
	To       string         `json:"to" format:"date"`
	Me       PersonMetrics  `json:"me"`
	Previous PersonMetrics  `json:"previous" doc:"El rango anterior del mismo largo (tendencia)."`
	PerDay   []DayCount     `json:"per_day"`
	Goals    []GoalProgress `json:"goals"`
}

// Team es GET /metrics/team (app del supervisor): ranking por rol.
type Team struct {
	Range  string          `json:"range"`
	From   string          `json:"from" format:"date"`
	To     string          `json:"to" format:"date"`
	People []PersonMetrics `json:"people"`
	Totals BoardTotals     `json:"totals"`
}

// Service contiene los casos de uso de metricas.
type Service struct {
	db    *database.DB
	notif *notifications.Service
	log   *slog.Logger
	now   func() time.Time
}

// NewService crea el servicio.
func NewService(db *database.DB, notif *notifications.Service, log *slog.Logger) *Service {
	return &Service{db: db, notif: notif, log: log, now: time.Now}
}

// SetClock cambia el reloj (pruebas).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// DB es la base del servicio (semilla).
func (s *Service) DB() *database.DB { return s.db }

const tenantTZ = `coalesce((SELECT timezone FROM shared.tenants WHERE schema_name = current_schema()), 'America/Guatemala')`

// today es la fecha de hoy en la hora de la empresa.
func (s *Service) today(ctx context.Context, q database.Querier) (time.Time, error) {
	var d time.Time
	err := q.QueryRow(ctx, `SELECT ($1::timestamptz AT TIME ZONE `+tenantTZ+`)::date`, s.now()).Scan(&d)
	return d, err
}

// span convierte el rango en dias (inclusive).
func span(r string, today time.Time) (time.Time, time.Time, error) {
	switch r {
	case "", "7d":
		return today.AddDate(0, 0, -6), today, nil
	case "30d":
		return today.AddDate(0, 0, -29), today, nil
	case "week":
		wd := (int(today.Weekday()) + 6) % 7 // lunes = 0
		return today.AddDate(0, 0, -wd), today, nil
	case "month":
		return time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC), today, nil
	}
	return time.Time{}, time.Time{}, ErrRange.WithField("range")
}

func day(t time.Time) string { return t.Format(time.DateOnly) }

// finished son las limpiezas terminadas del rango dentro del alcance. Usa
// $1 desde, $2 hasta, $3 todas las propiedades y $4 sus ids.
const finished = `
	tz AS (SELECT ` + tenantTZ + ` AS name),
	fin AS (
		SELECT t.id, t.assigned_to AS user_id, t.property_id, t.is_late, t.started_at, t.finished_at,
		       (t.finished_at AT TIME ZONE tz.name)::date AS day
		FROM tasks t CROSS JOIN tz
		WHERE t.type = 'cleaning' AND t.status IN ('done', 'validated') AND t.assigned_to IS NOT NULL
		  AND t.finished_at IS NOT NULL AND (t.finished_at AT TIME ZONE tz.name)::date BETWEEN $1::date AND $2::date
		  AND ($3::bool OR t.property_id = ANY($4::uuid[]))
	)`

// people calcula las cifras por persona. Con all incluye a los colaboradores
// activos sin limpiezas (el equipo completo); con user, solo esa persona.
func people(ctx context.Context, q database.Querier, from, to time.Time, scope authz.PropertyScope, user *uuid.UUID) ([]PersonMetrics, error) {
	all, ids := scope.SQLArgs()
	rows, err := q.Query(ctx, `
		WITH `+finished+`,
		who AS (
			SELECT user_id FROM fin
			UNION
			SELECT e.user_id FROM employees e WHERE $3::bool AND (e.termination_date IS NULL OR e.termination_date >= $1)
		)
		SELECT w.user_id AS id, e.id AS employee_id, coalesce(u.full_name, u.email) AS name,
		       coalesce(r.name, '') AS role, coalesce(r.key, '') AS role_key,
		       s.cleanings, s.on_time, s.avg_minutes, s.late, ret.n AS returned,
		       cl.pct AS checklist_compliance, tk.n AS tickets_reported
		FROM who w
		CROSS JOIN tz
		JOIN shared.users u ON u.id = w.user_id
		LEFT JOIN employees e ON e.user_id = w.user_id
		LEFT JOIN LATERAL (
			SELECT ro.name, ro.key FROM shared.memberships m
			JOIN shared.roles ro ON ro.id = m.role_id
			JOIN shared.tenants tn ON tn.id = m.tenant_id AND tn.schema_name = current_schema()
			WHERE m.user_id = w.user_id LIMIT 1) r ON true
		CROSS JOIN LATERAL (
			SELECT count(*)::int AS cleanings,
			       coalesce(round(100.0 * count(*) FILTER (WHERE NOT f.is_late) / nullif(count(*), 0)), 0)::int AS on_time,
			       coalesce(round(avg(extract(epoch FROM f.finished_at - f.started_at) / 60)
			                 FILTER (WHERE f.started_at IS NOT NULL)), 0)::int AS avg_minutes,
			       count(*) FILTER (WHERE f.is_late)::int AS late
			FROM fin f WHERE f.user_id = w.user_id) s
		CROSS JOIN LATERAL (
			SELECT count(*)::int AS n FROM task_events ev JOIN tasks t2 ON t2.id = ev.task_id
			WHERE ev.type = 'returned' AND t2.assigned_to = w.user_id
			  AND (ev.created_at AT TIME ZONE tz.name)::date BETWEEN $1 AND $2
			  AND ($3::bool OR t2.property_id = ANY($4::uuid[]))) ret
		CROSS JOIN LATERAL (
			SELECT coalesce(round(100.0 * count(*) FILTER (WHERE ci.is_done) / nullif(count(*), 0), 1), 0)::float8 AS pct
			FROM task_checklist_items ci JOIN fin f ON f.id = ci.task_id WHERE f.user_id = w.user_id) cl
		CROSS JOIN LATERAL (
			SELECT count(*)::int AS n FROM maintenance_tickets mt
			WHERE mt.reported_by_user_id = w.user_id
			  AND (mt.created_at AT TIME ZONE tz.name)::date BETWEEN $1 AND $2) tk
		WHERE ($5::uuid IS NULL OR w.user_id = $5)
		ORDER BY s.cleanings DESC, s.on_time DESC, s.late, 3`, day(from), day(to), all, ids, user)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[PersonMetrics])
	if err != nil {
		return nil, err
	}
	rank(list)
	return list, nil
}

// rank pone el puesto dentro de cada rol (la lista ya viene ordenada).
func rank(list []PersonMetrics) {
	seen, total := map[string]int{}, map[string]int{}
	for _, p := range list {
		total[p.RoleKey]++
	}
	for i := range list {
		seen[list[i].RoleKey]++
		list[i].Rank, list[i].RankOf = seen[list[i].RoleKey], total[list[i].RoleKey]
	}
}

func totals(list []PersonMetrics) BoardTotals {
	var t BoardTotals
	var onTime, minutes float64
	var timed int
	for _, p := range list {
		t.Cleanings += p.Cleanings
		t.Late += p.Late
		t.Returned += p.Returned
		onTime += float64(p.OnTime * p.Cleanings)
		if p.AvgMinutes > 0 {
			minutes += float64(p.AvgMinutes * p.Cleanings)
			timed += p.Cleanings
		}
	}
	if t.Cleanings > 0 {
		t.OnTime = int(math.Round(onTime / float64(t.Cleanings)))
	}
	if timed > 0 {
		t.AvgMinutes = int(math.Round(minutes / float64(timed)))
	}
	return t
}

func perDay(ctx context.Context, q database.Querier, from, to time.Time, scope authz.PropertyScope, user *uuid.UUID) ([]DayCount, error) {
	all, ids := scope.SQLArgs()
	rows, err := q.Query(ctx, `
		WITH `+finished+`
		SELECT to_char(d, 'YYYY-MM-DD') AS date, count(f.id)::int AS count
		FROM generate_series($1::date, $2::date, interval '1 day') d
		LEFT JOIN fin f ON f.day = d::date AND ($5::uuid IS NULL OR f.user_id = $5)
		GROUP BY d ORDER BY d`, day(from), day(to), all, ids, user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[DayCount])
}

func properties(ctx context.Context, q database.Querier, from, to time.Time, scope authz.PropertyScope) ([]PropertyMetrics, error) {
	all, ids := scope.SQLArgs()
	rows, err := q.Query(ctx, `
		WITH `+finished+`
		SELECT p.code, p.name, count(*)::int AS cleanings,
		       coalesce(round(avg(extract(epoch FROM f.finished_at - f.started_at) / 60)
		                 FILTER (WHERE f.started_at IS NOT NULL)), 0)::int AS avg_minutes
		FROM fin f JOIN properties p ON p.id = f.property_id
		GROUP BY p.code, p.name ORDER BY 3 DESC, 1`, day(from), day(to), all, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[PropertyMetrics])
}

// Board arma el tablero del rango dentro del alcance del actor.
func (s *Service) Board(ctx context.Context, actor authz.Actor, r string) (Board, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Board, error) {
		today, err := s.today(ctx, q)
		if err != nil {
			return Board{}, err
		}
		from, to, err := span(r, today)
		if err != nil {
			return Board{}, err
		}
		if r == "" {
			r = "7d"
		}
		scope, err := authz.ResolvePropertyScope(ctx, q, actor)
		if err != nil {
			return Board{}, err
		}
		out := Board{Range: r, From: day(from), To: day(to)}
		if out.PerDay, err = perDay(ctx, q, from, to, scope, nil); err != nil {
			return Board{}, err
		}
		if out.People, err = people(ctx, q, from, to, scope, nil); err != nil {
			return Board{}, err
		}
		if out.Properties, err = properties(ctx, q, from, to, scope); err != nil {
			return Board{}, err
		}
		out.Totals = totals(out.People)
		return out, nil
	})
}

// Team es el ranking del equipo por rol (supervisor).
func (s *Service) Team(ctx context.Context, actor authz.Actor, r string) (Team, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Team, error) {
		today, err := s.today(ctx, q)
		if err != nil {
			return Team{}, err
		}
		from, to, err := span(r, today)
		if err != nil {
			return Team{}, err
		}
		if r == "" {
			r = "7d"
		}
		scope, err := authz.ResolvePropertyScope(ctx, q, actor)
		if err != nil {
			return Team{}, err
		}
		list, err := people(ctx, q, from, to, scope, nil)
		if err != nil {
			return Team{}, err
		}
		// Solo colaboradores con ficha (el equipo, sin admins ni propietarios).
		team := make([]PersonMetrics, 0, len(list))
		for _, p := range list {
			if p.EmployeeID != nil {
				team = append(team, p)
			}
		}
		rank(team)
		return Team{Range: r, From: day(from), To: day(to), People: team, Totals: totals(team)}, nil
	})
}

// Me son las cifras del colaborador, su tendencia, su puesto y sus metas.
func (s *Service) Me(ctx context.Context, actor authz.Actor, r string) (MyMetrics, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (MyMetrics, error) {
		today, err := s.today(ctx, q)
		if err != nil {
			return MyMetrics{}, err
		}
		from, to, err := span(r, today)
		if err != nil {
			return MyMetrics{}, err
		}
		if r == "" {
			r = "7d"
		}
		all := authz.PropertyScope{All: true}
		list, err := people(ctx, q, from, to, all, nil)
		if err != nil {
			return MyMetrics{}, err
		}
		out := MyMetrics{Range: r, From: day(from), To: day(to), Goals: []GoalProgress{}}
		found := false
		for _, p := range list {
			if p.ID == actor.UserID {
				out.Me, found = p, true
			}
		}
		if !found {
			var emp *uuid.UUID
			var name string
			if err := q.QueryRow(ctx, `SELECT (SELECT id FROM employees WHERE user_id = $1), coalesce(full_name, email) FROM shared.users WHERE id = $1`,
				actor.UserID).Scan(&emp, &name); err != nil {
				return MyMetrics{}, err
			}
			out.Me = PersonMetrics{ID: actor.UserID, EmployeeID: emp, Name: name}
		}
		// Tendencia: el rango anterior del mismo largo.
		length := int(to.Sub(from).Hours()/24) + 1
		pFrom, pTo := from.AddDate(0, 0, -length), from.AddDate(0, 0, -1)
		prev, err := people(ctx, q, pFrom, pTo, all, &actor.UserID)
		if err != nil {
			return MyMetrics{}, err
		}
		if len(prev) > 0 {
			out.Previous = prev[0]
		} else {
			out.Previous = PersonMetrics{ID: actor.UserID, Name: out.Me.Name}
		}
		if out.PerDay, err = perDay(ctx, q, from, to, all, &actor.UserID); err != nil {
			return MyMetrics{}, err
		}
		if out.Me.EmployeeID != nil {
			if out.Goals, err = s.progressFor(ctx, q, today, *out.Me.EmployeeID, out.Me.RoleKey); err != nil {
				return MyMetrics{}, err
			}
		}
		return out, nil
	})
}

// --- Foto diaria ----------------------------------------------------------------

// Snapshot guarda las cifras de ese dia por colaborador (idempotente).
func (s *Service) Snapshot(ctx context.Context, q database.Querier, d time.Time) (int, error) {
	list, err := people(ctx, q, d, d, authz.PropertyScope{All: true}, nil)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range list {
		if p.EmployeeID == nil {
			continue
		}
		var open int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM inventory_discrepancies WHERE user_id = $1 AND status = 'open'`, p.ID).Scan(&open); err != nil {
			return 0, err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO performance_snapshots (employee_id, date, cleanings_done, avg_minutes, checklist_compliance,
			                                   late_count, returned_count, tickets_reported, discrepancies_open)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (employee_id, date) DO UPDATE SET cleanings_done = EXCLUDED.cleanings_done,
			    avg_minutes = EXCLUDED.avg_minutes, checklist_compliance = EXCLUDED.checklist_compliance,
			    late_count = EXCLUDED.late_count, returned_count = EXCLUDED.returned_count,
			    tickets_reported = EXCLUDED.tickets_reported, discrepancies_open = EXCLUDED.discrepancies_open`,
			*p.EmployeeID, day(d), p.Cleanings, p.AvgMinutes, p.ChecklistCompliance, p.Late, p.Returned, p.TicketsReported, open); err != nil {
			return 0, fmt.Errorf("guardando la foto del dia: %w", err)
		}
		n++
	}
	return n, nil
}
