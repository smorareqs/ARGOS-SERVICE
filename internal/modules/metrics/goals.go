package metrics

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/notifications"
)

// Errores de metas e incentivos.
var (
	ErrGoalNotFound = apperr.New(apperr.KindNotFound, "La meta no existe.")
	ErrApplies      = apperr.New(apperr.KindValidation, "Elige a quién aplica la meta.")
	ErrDecide       = apperr.New(apperr.KindValidation, "Elige incentivos pendientes.")
)

// Metricas que puede medir una meta y como se muestran.
var units = map[string]string{
	"cleanings_done":       "limpiezas",
	"checklist_compliance": "%",
	"on_time_rate":         "%",
	"late_count":           "retrasos",
	"returned_count":       "devueltas",
	"avg_minutes":          "min",
	"tickets_reported":     "reportes",
}

// GoalRead es una meta. title, scope, current, unit y reward son lo que
// muestra la web (W-K2); el resto, lo que se edita.
type GoalRead struct {
	ID              uuid.UUID  `json:"id" format:"uuid"`
	Title           string     `json:"title"`
	Scope           string     `json:"scope" doc:"A quien aplica, en palabras."`
	Target          float64    `json:"target"`
	Current         float64    `json:"current" doc:"Promedio por persona en el periodo en curso."`
	Unit            string     `json:"unit"`
	Reward          string     `json:"reward"`
	Metric          string     `json:"metric" enum:"cleanings_done,checklist_compliance,on_time_rate,late_count,returned_count,avg_minutes,tickets_reported"`
	Comparator      string     `json:"comparator" enum:">=,<="`
	PeriodKind      string     `json:"period_kind" enum:"weekly,monthly"`
	AppliesTo       string     `json:"applies_to" enum:"all,role,user"`
	AppliesID       *uuid.UUID `json:"applies_id" format:"uuid" nullable:"true" doc:"Colaborador (applies_to user)."`
	AppliesRole     *string    `json:"applies_role" nullable:"true" doc:"Clave del rol (applies_to role)."`
	IncentiveAmount *float64   `json:"incentive_amount" nullable:"true"`
	RewardText      *string    `json:"reward_text" nullable:"true"`
	Active          bool       `json:"active"`
}

// GoalWrite es el cuerpo de POST /metrics/goals.
type GoalWrite struct {
	_               struct{}   `additionalProperties:"true"`
	Name            string     `json:"name" minLength:"3" maxLength:"120"`
	Metric          string     `json:"metric" enum:"cleanings_done,checklist_compliance,on_time_rate,late_count,returned_count,avg_minutes,tickets_reported"`
	Target          float64    `json:"target" minimum:"0"`
	Comparator      string     `json:"comparator,omitempty" enum:">=,<="`
	PeriodKind      string     `json:"period_kind,omitempty" enum:"weekly,monthly"`
	AppliesTo       string     `json:"applies_to,omitempty" enum:"all,role,user"`
	AppliesID       *uuid.UUID `json:"applies_id,omitempty" format:"uuid"`
	AppliesRole     *string    `json:"applies_role,omitempty" maxLength:"32"`
	IncentiveAmount *float64   `json:"incentive_amount,omitempty" minimum:"0"`
	RewardText      *string    `json:"reward_text,omitempty" maxLength:"120"`
}

// GoalUpdate es el cuerpo de PATCH /metrics/goals/{id}.
type GoalUpdate struct {
	_               struct{} `additionalProperties:"true"`
	Name            *string  `json:"name,omitempty" minLength:"3" maxLength:"120"`
	Target          *float64 `json:"target,omitempty" minimum:"0"`
	IncentiveAmount *float64 `json:"incentive_amount,omitempty" minimum:"0"`
	RewardText      *string  `json:"reward_text,omitempty" maxLength:"120"`
	Active          *bool    `json:"active,omitempty"`
}

// GoalProgress es una meta vista por el colaborador (app B21).
type GoalProgress struct {
	ID      uuid.UUID `json:"id" format:"uuid"`
	Title   string    `json:"title"`
	Target  float64   `json:"target"`
	Current float64   `json:"current"`
	Unit    string    `json:"unit"`
	Reward  string    `json:"reward"`
	Lower   bool      `json:"lower_is_better"`
	Met     bool      `json:"met"`
	Period  string    `json:"period" doc:"Esta semana o este mes."`
}

// IncentiveRead es un incentivo (W-K3).
type IncentiveRead struct {
	ID          uuid.UUID  `db:"id" json:"id" format:"uuid"`
	EmployeeID  uuid.UUID  `db:"employee_id" json:"employee_id" format:"uuid"`
	PersonName  string     `db:"person_name" json:"person_name"`
	GoalID      uuid.UUID  `db:"goal_id" json:"goal_id" format:"uuid"`
	GoalTitle   string     `db:"goal_title" json:"goal_title"`
	Amount      float64    `db:"amount" json:"amount"`
	Achieved    float64    `db:"achieved_value" json:"achieved_value"`
	Status      string     `db:"status" json:"status" enum:"pending,approved,rejected,paid"`
	PeriodStart string     `db:"period_start" json:"period_start" format:"date"`
	PeriodEnd   string     `db:"period_end" json:"period_end" format:"date"`
	Period      string     `db:"-" json:"period" doc:"El periodo en palabras."`
	DecidedAt   *time.Time `db:"decided_at" json:"decided_at" nullable:"true"`
}

// Decide es el cuerpo de POST /metrics/incentives/decide.
type Decide struct {
	_      struct{}    `additionalProperties:"true"`
	IDs    []uuid.UUID `json:"ids" minItems:"1" maxItems:"200"`
	Status string      `json:"status" enum:"approved,rejected"`
}

type goalRow struct {
	ID              uuid.UUID  `db:"id"`
	Name            string     `db:"name"`
	Metric          string     `db:"metric"`
	Target          float64    `db:"target"`
	Comparator      string     `db:"comparator"`
	PeriodKind      string     `db:"period_kind"`
	AppliesTo       string     `db:"applies_to"`
	AppliesID       *uuid.UUID `db:"applies_id"`
	AppliesRole     *string    `db:"applies_role"`
	IncentiveAmount *float64   `db:"incentive_amount"`
	RewardText      *string    `db:"reward_text"`
	Active          bool       `db:"is_active"`
	AppliesName     *string    `db:"applies_name"`
	RoleName        *string    `db:"role_name"`
	Currency        string     `db:"currency"`
}

const goalSelect = `
	SELECT g.id, g.name, g.metric, g.target::float8 AS target, g.comparator, g.period_kind, g.applies_to, g.applies_id,
	       g.applies_role, g.incentive_amount::float8 AS incentive_amount, g.reward_text, g.is_active,
	       (SELECT u.full_name FROM employees e JOIN shared.users u ON u.id = e.user_id WHERE e.id = g.applies_id) AS applies_name,
	       (SELECT ro.name FROM shared.roles ro JOIN shared.tenants tn ON tn.id = ro.tenant_id AND tn.schema_name = current_schema()
	        WHERE ro.key = g.applies_role LIMIT 1) AS role_name,
	       (SELECT currency FROM shared.tenants WHERE schema_name = current_schema()) AS currency
	FROM goals g`

func loadGoals(ctx context.Context, q database.Querier, where string, args ...any) ([]goalRow, error) {
	rows, err := q.Query(ctx, goalSelect+" "+where, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[goalRow])
}

func money(v float64, currency string) string {
	sym := map[string]string{"GTQ": "Q", "USD": "$"}[strings.TrimSpace(currency)]
	if sym == "" {
		sym = strings.TrimSpace(currency) + " "
	}
	if v == math.Trunc(v) {
		return fmt.Sprintf("%s%.0f", sym, v)
	}
	return fmt.Sprintf("%s%.2f", sym, v)
}

func (g goalRow) reward() string {
	if g.IncentiveAmount != nil && *g.IncentiveAmount > 0 {
		r := money(*g.IncentiveAmount, g.Currency)
		if g.RewardText != nil && *g.RewardText != "" {
			r += " · " + *g.RewardText
		}
		return r
	}
	if g.RewardText != nil {
		return *g.RewardText
	}
	return "Reconocimiento"
}

func (g goalRow) scope() string {
	switch g.AppliesTo {
	case "role":
		if g.RoleName != nil {
			return "Rol: " + *g.RoleName
		}
		return "Un rol"
	case "user":
		if g.AppliesName != nil {
			return *g.AppliesName
		}
		return "Una persona"
	}
	return "Todo el equipo"
}

// applies dice si la meta cuenta para ese colaborador.
func (g goalRow) applies(employee uuid.UUID, roleKey string) bool {
	switch g.AppliesTo {
	case "role":
		return g.AppliesRole != nil && *g.AppliesRole == roleKey
	case "user":
		return g.AppliesID != nil && *g.AppliesID == employee
	}
	return true
}

// value saca de las cifras lo que mide la meta.
func value(metric string, p PersonMetrics) float64 {
	switch metric {
	case "cleanings_done":
		return float64(p.Cleanings)
	case "checklist_compliance":
		return p.ChecklistCompliance
	case "on_time_rate":
		return float64(p.OnTime)
	case "late_count":
		return float64(p.Late)
	case "returned_count":
		return float64(p.Returned)
	case "avg_minutes":
		return float64(p.AvgMinutes)
	case "tickets_reported":
		return float64(p.TicketsReported)
	}
	return 0
}

func meets(g goalRow, v float64) bool {
	if g.Comparator == "<=" {
		return v <= g.Target
	}
	return v >= g.Target
}

// counts dice si el colaborador participa en la meta: las de limpieza piden
// al menos una limpieza en el periodo (si no, "0 retrasos" premiaria a todos).
func counts(metric string, p PersonMetrics) bool {
	return metric == "tickets_reported" || p.Cleanings > 0
}

// current es el periodo en curso de la meta.
func current(kind string, today time.Time) (time.Time, time.Time) {
	if kind == "weekly" {
		from, _, _ := span("week", today)
		return from, today
	}
	from, _, _ := span("month", today)
	return from, today
}

// closed es el ultimo periodo que ya termino.
func closed(kind string, today time.Time) (time.Time, time.Time) {
	if kind == "weekly" {
		from, _, _ := span("week", today)
		return from.AddDate(0, 0, -7), from.AddDate(0, 0, -1)
	}
	first := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	return first.AddDate(0, -1, 0), first.AddDate(0, 0, -1)
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func (s *Service) view(ctx context.Context, q database.Querier, g goalRow, today time.Time) (GoalRead, error) {
	out := GoalRead{
		ID: g.ID, Title: g.Name, Scope: g.scope(), Target: g.Target, Unit: units[g.Metric], Reward: g.reward(),
		Metric: g.Metric, Comparator: g.Comparator, PeriodKind: g.PeriodKind, AppliesTo: g.AppliesTo,
		AppliesID: g.AppliesID, AppliesRole: g.AppliesRole, IncentiveAmount: g.IncentiveAmount, RewardText: g.RewardText,
		Active: g.Active,
	}
	from, to := current(g.PeriodKind, today)
	list, err := people(ctx, q, from, to, authz.PropertyScope{All: true}, nil)
	if err != nil {
		return out, err
	}
	var sum float64
	n := 0
	for _, p := range list {
		if p.EmployeeID == nil || !g.applies(*p.EmployeeID, p.RoleKey) || !counts(g.Metric, p) {
			continue
		}
		sum += value(g.Metric, p)
		n++
	}
	if n > 0 {
		out.Current = round1(sum / float64(n))
	}
	return out, nil
}

// Goals lista las metas con su avance.
func (s *Service) Goals(ctx context.Context, all bool) ([]GoalRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]GoalRead, error) {
		today, err := s.today(ctx, q)
		if err != nil {
			return nil, err
		}
		rows, err := loadGoals(ctx, q, `WHERE g.is_active OR $1 ORDER BY g.is_active DESC, g.created_at`, all)
		if err != nil {
			return nil, err
		}
		out := make([]GoalRead, 0, len(rows))
		for _, g := range rows {
			v, err := s.view(ctx, q, g, today)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	})
}

func (s *Service) goalByID(ctx context.Context, q database.Querier, id uuid.UUID) (GoalRead, error) {
	rows, err := loadGoals(ctx, q, `WHERE g.id = $1`, id)
	if err != nil {
		return GoalRead{}, err
	}
	if len(rows) == 0 {
		return GoalRead{}, ErrGoalNotFound
	}
	today, err := s.today(ctx, q)
	if err != nil {
		return GoalRead{}, err
	}
	return s.view(ctx, q, rows[0], today)
}

// CreateGoal da de alta una meta.
func (s *Service) CreateGoal(ctx context.Context, actor authz.Actor, in GoalWrite) (GoalRead, error) {
	cmp, kind, applies := in.Comparator, in.PeriodKind, in.AppliesTo
	if cmp == "" {
		cmp = ">="
		if in.Metric == "late_count" || in.Metric == "returned_count" || in.Metric == "avg_minutes" {
			cmp = "<="
		}
	}
	if kind == "" {
		kind = "monthly"
	}
	if applies == "" {
		applies = "all"
	}
	if (applies == "user" && in.AppliesID == nil) || (applies == "role" && (in.AppliesRole == nil || *in.AppliesRole == "")) {
		return GoalRead{}, ErrApplies.WithField("body.applies_to")
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (GoalRead, error) {
		if applies == "user" {
			var ok bool
			if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM employees WHERE id = $1)`, in.AppliesID).Scan(&ok); err != nil {
				return GoalRead{}, err
			}
			if !ok {
				return GoalRead{}, ErrApplies.WithField("body.applies_id")
			}
		}
		var id uuid.UUID
		if err := q.QueryRow(ctx, `
			INSERT INTO goals (name, metric, target, comparator, period_kind, applies_to, applies_id, applies_role,
			                   incentive_amount, reward_text, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`,
			strings.TrimSpace(in.Name), in.Metric, in.Target, cmp, kind, applies, in.AppliesID, in.AppliesRole,
			in.IncentiveAmount, in.RewardText, actor.UserID).Scan(&id); err != nil {
			return GoalRead{}, err
		}
		out, err := s.goalByID(ctx, q, id)
		if err != nil {
			return GoalRead{}, err
		}
		return out, audit.Record(ctx, q, audit.Entry{Actor: &actor.UserID, Action: "goal.created", Entity: "goal", EntityID: &id, After: out})
	})
}

// UpdateGoal corrige o desactiva una meta.
func (s *Service) UpdateGoal(ctx context.Context, actor authz.Actor, id uuid.UUID, in GoalUpdate) (GoalRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (GoalRead, error) {
		before, err := s.goalByID(ctx, q, id)
		if err != nil {
			return GoalRead{}, err
		}
		if _, err := q.Exec(ctx, `
			UPDATE goals SET name = coalesce($2, name), target = coalesce($3, target),
			       incentive_amount = coalesce($4, incentive_amount), reward_text = coalesce($5, reward_text),
			       is_active = coalesce($6, is_active), updated_at = now()
			WHERE id = $1`, id, in.Name, in.Target, in.IncentiveAmount, in.RewardText, in.Active); err != nil {
			return GoalRead{}, err
		}
		out, err := s.goalByID(ctx, q, id)
		if err != nil {
			return GoalRead{}, err
		}
		return out, audit.Record(ctx, q, audit.Entry{Actor: &actor.UserID, Action: "goal.updated", Entity: "goal", EntityID: &id, Before: before, After: out})
	})
}

// progressFor son las metas activas del colaborador en su periodo en curso.
func (s *Service) progressFor(ctx context.Context, q database.Querier, today time.Time, employee uuid.UUID, roleKey string) ([]GoalProgress, error) {
	rows, err := loadGoals(ctx, q, `WHERE g.is_active ORDER BY g.created_at`)
	if err != nil {
		return nil, err
	}
	var user uuid.UUID
	if err := q.QueryRow(ctx, `SELECT user_id FROM employees WHERE id = $1`, employee).Scan(&user); err != nil {
		return nil, err
	}
	out := []GoalProgress{}
	cache := map[string]PersonMetrics{}
	for _, g := range rows {
		if !g.applies(employee, roleKey) {
			continue
		}
		p, ok := cache[g.PeriodKind]
		if !ok {
			from, to := current(g.PeriodKind, today)
			list, err := people(ctx, q, from, to, authz.PropertyScope{All: true}, &user)
			if err != nil {
				return nil, err
			}
			if len(list) > 0 {
				p = list[0]
			}
			cache[g.PeriodKind] = p
		}
		v := value(g.Metric, p)
		period := "Este mes"
		if g.PeriodKind == "weekly" {
			period = "Esta semana"
		}
		out = append(out, GoalProgress{
			ID: g.ID, Title: g.Name, Target: g.Target, Current: round1(v), Unit: units[g.Metric], Reward: g.reward(),
			Lower: g.Comparator == "<=", Met: counts(g.Metric, p) && meets(g, v), Period: period,
		})
	}
	return out, nil
}

// --- Incentivos ------------------------------------------------------------------

var monthNames = []string{"ene", "feb", "mar", "abr", "may", "jun", "jul", "ago", "sep", "oct", "nov", "dic"}

func periodLabel(from, to string) string {
	a, err1 := time.Parse(time.DateOnly, from)
	b, err2 := time.Parse(time.DateOnly, to)
	if err1 != nil || err2 != nil {
		return from + " – " + to
	}
	if a.Day() == 1 && b.AddDate(0, 0, 1).Day() == 1 && a.Month() == b.Month() {
		return fmt.Sprintf("%s %d", monthNames[a.Month()-1], a.Year())
	}
	if a.Month() == b.Month() {
		return fmt.Sprintf("%d–%d %s", a.Day(), b.Day(), monthNames[b.Month()-1])
	}
	return fmt.Sprintf("%d %s – %d %s", a.Day(), monthNames[a.Month()-1], b.Day(), monthNames[b.Month()-1])
}

const incentiveSelect = `
	SELECT ia.id, ia.employee_id, coalesce(u.full_name, e.employee_code) AS person_name, ia.goal_id, g.name AS goal_title,
	       ia.amount::float8 AS amount, ia.achieved_value::float8 AS achieved_value, ia.status,
	       to_char(ia.period_start, 'YYYY-MM-DD') AS period_start, to_char(ia.period_end, 'YYYY-MM-DD') AS period_end,
	       ia.decided_at
	FROM incentive_awards ia
	JOIN goals g ON g.id = ia.goal_id
	JOIN employees e ON e.id = ia.employee_id
	LEFT JOIN shared.users u ON u.id = e.user_id`

func incentives(ctx context.Context, q database.Querier, where string, args ...any) ([]IncentiveRead, error) {
	rows, err := q.Query(ctx, incentiveSelect+" "+where, args...)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[IncentiveRead])
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].Period = periodLabel(list[i].PeriodStart, list[i].PeriodEnd)
	}
	return list, nil
}

// Incentives lista los pendientes y los de los ultimos 120 dias.
func (s *Service) Incentives(ctx context.Context) ([]IncentiveRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]IncentiveRead, error) {
		return incentives(ctx, q, `WHERE ia.status = 'pending' OR ia.period_end >= current_date - 120
			ORDER BY (ia.status = 'pending') DESC, ia.period_end DESC, person_name`)
	})
}

// DecideIncentives aprueba o rechaza pendientes y devuelve la lista.
func (s *Service) DecideIncentives(ctx context.Context, actor authz.Actor, in Decide) ([]IncentiveRead, error) {
	err := s.db.InTx(ctx, func(q database.Querier) error {
		rows, err := q.Query(ctx, `
			UPDATE incentive_awards SET status = $2, approved_by = $3, decided_at = now(), updated_at = now()
			WHERE id = ANY($1) AND status = 'pending' RETURNING id`, in.IDs, in.Status, actor.UserID)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return ErrDecide.WithField("body.ids")
		}
		for _, id := range ids {
			if err := audit.Record(ctx, q, audit.Entry{Actor: &actor.UserID, Action: "incentive." + in.Status, Entity: "incentive_award",
				EntityID: &id, After: map[string]any{"status": in.Status}}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Incentives(ctx)
}

// Evaluate revisa cada meta activa en su ultimo periodo cerrado y crea los
// incentivos pendientes de quien la cumplio (idempotente: uno por meta,
// persona y periodo). Avisa a quien maneja la nomina si hubo nuevos.
func (s *Service) Evaluate(ctx context.Context, q database.Querier, tenantID uuid.UUID) (int, error) {
	today, err := s.today(ctx, q)
	if err != nil {
		return 0, err
	}
	goals, err := loadGoals(ctx, q, `WHERE g.is_active ORDER BY g.created_at`)
	if err != nil {
		return 0, err
	}
	created := 0
	cache := map[string][]PersonMetrics{}
	for _, g := range goals {
		from, to := closed(g.PeriodKind, today)
		key := day(from) + day(to)
		list, ok := cache[key]
		if !ok {
			if list, err = people(ctx, q, from, to, authz.PropertyScope{All: true}, nil); err != nil {
				return 0, err
			}
			cache[key] = list
		}
		amount := 0.0
		if g.IncentiveAmount != nil {
			amount = *g.IncentiveAmount
		}
		for _, p := range list {
			if p.EmployeeID == nil || !g.applies(*p.EmployeeID, p.RoleKey) || !counts(g.Metric, p) {
				continue
			}
			v := value(g.Metric, p)
			if !meets(g, v) {
				continue
			}
			tag, err := q.Exec(ctx, `
				INSERT INTO incentive_awards (goal_id, employee_id, period_start, period_end, achieved_value, amount)
				VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (goal_id, employee_id, period_start) DO NOTHING`,
				g.ID, *p.EmployeeID, day(from), day(to), round1(v), amount)
			if err != nil {
				return 0, err
			}
			created += int(tag.RowsAffected())
		}
	}
	if created > 0 && s.notif != nil {
		holders, err := notifications.Holders(ctx, q, tenantID, nil, authz.HRPayrollManage)
		if err != nil {
			return 0, err
		}
		body := "Hay 1 incentivo por aprobar."
		if created > 1 {
			body = fmt.Sprintf("Hay %d incentivos por aprobar.", created)
		}
		if err := s.notif.Notify(ctx, q, tenantID, notifications.Notice{
			Recipients: holders, Type: notifications.TypeIncentivePending, Title: "Metas cumplidas", Body: body,
			Route: "/metricas?tab=incentivos", AppRoute: "/alerts",
			Channels: []string{notifications.ChannelInApp, notifications.ChannelEmail},
		}); err != nil {
			return 0, err
		}
	}
	return created, nil
}

// --- Trabajos --------------------------------------------------------------------

// HandleJob corre performance_snapshot (ayer) e incentive_evaluation: sin
// empresa encola uno por empresa activa.
func (s *Service) HandleJob(ctx context.Context, job jobs.Running) error {
	if job.Type != JobSnapshot && job.Type != JobEvaluate {
		return fmt.Errorf("trabajo desconocido %s", job.Type)
	}
	if job.TenantID == nil {
		return s.db.InTx(ctx, func(q database.Querier) error {
			rows, err := q.Query(ctx, `SELECT id FROM shared.tenants WHERE is_active ORDER BY slug`)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := jobs.Enqueue(ctx, q, jobs.Job{
					Type: job.Type, TenantID: &id, DedupeKey: job.Type + ":" + id.String(), MaxAttempts: 3,
				}); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		if job.Type == JobEvaluate {
			_, err := s.Evaluate(ctx, q, *job.TenantID)
			return err
		}
		today, err := s.today(ctx, q)
		if err != nil {
			return err
		}
		_, err = s.Snapshot(ctx, q, today.AddDate(0, 0, -1))
		return err
	})
}
