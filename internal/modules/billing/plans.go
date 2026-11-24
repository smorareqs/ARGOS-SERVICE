package billing

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Limits son los limites de un plan. nil = sin limite.
type Limits struct {
	MaxProperties *int     `json:"max_properties" nullable:"true" required:"false" minimum:"0" doc:"null = sin límite."`
	MaxUsers      *int     `json:"max_users" nullable:"true" required:"false" minimum:"0" doc:"Personas con acceso (sin huéspedes). null = sin límite."`
	Features      []string `json:"features" doc:"Banderas que el plan permite activar: reservations, parking, shared_areas, guest_chat, payroll, smart_locks."`
}

// Plan es un plan (W-P2 y el selector de W-O4).
type Plan struct {
	ID           uuid.UUID `json:"id" format:"uuid"`
	Key          string    `json:"key"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	PriceMonthly float64   `json:"price_monthly"`
	PriceYearly  float64   `json:"price_yearly"`
	Currency     string    `json:"currency"`
	Limits       Limits    `json:"limits"`
	IsActive     bool      `json:"is_active"`
	SortOrder    int       `json:"sort_order"`
	Subscribers  int       `json:"subscribers" doc:"Empresas con este plan."`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// NewPlan es el cuerpo de POST /platform/plans.
type NewPlan struct {
	Key          string  `json:"key" minLength:"2" maxLength:"32"`
	Name         string  `json:"name" minLength:"2" maxLength:"64"`
	Description  string  `json:"description,omitempty" maxLength:"255"`
	PriceMonthly float64 `json:"price_monthly" minimum:"0"`
	PriceYearly  float64 `json:"price_yearly" minimum:"0"`
	Currency     string  `json:"currency,omitempty" maxLength:"3" doc:"Por defecto GTQ."`
	Limits       Limits  `json:"limits"`
	SortOrder    int     `json:"sort_order,omitempty"`
}

// PlanChanges es el cuerpo de PATCH /platform/plans/{id}.
type PlanChanges struct {
	Name         *string  `json:"name,omitempty" minLength:"2" maxLength:"64"`
	Description  *string  `json:"description,omitempty" maxLength:"255"`
	PriceMonthly *float64 `json:"price_monthly,omitempty" minimum:"0"`
	PriceYearly  *float64 `json:"price_yearly,omitempty" minimum:"0"`
	Limits       *Limits  `json:"limits,omitempty"`
	IsActive     *bool    `json:"is_active,omitempty" doc:"false: deja de ofrecerse; quien ya lo tiene lo conserva."`
	SortOrder    *int     `json:"sort_order,omitempty"`
}

const selectPlan = `
	SELECT p.id, p.key, p.name, p.description, p.price_monthly::float8, p.price_yearly::float8, trim(p.currency),
	       p.limits, p.is_active, p.sort_order,
	       (SELECT count(*)::int FROM shared.subscriptions s WHERE s.plan_id = p.id AND s.status <> 'cancelled'),
	       p.created_at, p.updated_at
	FROM shared.plans p`

func scanPlan(row pgx.Row) (Plan, error) {
	var p Plan
	err := row.Scan(&p.ID, &p.Key, &p.Name, &p.Description, &p.PriceMonthly, &p.PriceYearly, &p.Currency,
		&p.Limits, &p.IsActive, &p.SortOrder, &p.Subscribers, &p.CreatedAt, &p.UpdatedAt)
	if p.Limits.Features == nil {
		p.Limits.Features = []string{}
	}
	return p, err
}

func plans(ctx context.Context, q database.Querier, onlyActive bool) ([]Plan, error) {
	rows, err := q.Query(ctx, selectPlan+` WHERE $1::bool = false OR p.is_active ORDER BY p.sort_order, p.price_monthly`, onlyActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func planByID(ctx context.Context, q database.Querier, id uuid.UUID) (Plan, error) {
	p, err := scanPlan(q.QueryRow(ctx, selectPlan+` WHERE p.id = $1`, id))
	return p, notFound(err, ErrPlanNotFound)
}

func planByKey(ctx context.Context, q database.Querier, key string) (Plan, error) {
	p, err := scanPlan(q.QueryRow(ctx, selectPlan+` WHERE p.key = $1`, strings.TrimSpace(key)))
	return p, notFound(err, ErrPlanNotFound.WithField("body.plan_key"))
}

// Plans lista los planes (todos para la plataforma; activos para empresas).
func (s *Service) Plans(ctx context.Context, onlyActive bool) ([]Plan, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]Plan, error) {
		return plans(ctx, q, onlyActive)
	})
}

var planKeyRe = regexp.MustCompile(`^[a-z0-9_]{2,32}$`)

func validLimits(l Limits) error {
	for _, f := range l.Features {
		if !slices.Contains(authz.Features, authz.Feature(f)) {
			return ErrUnknownFeature.WithField("body.limits.features")
		}
	}
	return nil
}

func cleanLimits(l Limits) Limits {
	seen := []string{}
	for _, f := range l.Features {
		if !slices.Contains(seen, f) {
			seen = append(seen, f)
		}
	}
	l.Features = seen
	return l
}

// CreatePlan da de alta un plan (plataforma).
func (s *Service) CreatePlan(ctx context.Context, actor uuid.UUID, in NewPlan) (Plan, error) {
	in.Key = strings.ToLower(strings.TrimSpace(in.Key))
	if !planKeyRe.MatchString(in.Key) {
		return Plan{}, ErrPlanKey.WithField("body.key")
	}
	if err := validLimits(in.Limits); err != nil {
		return Plan{}, err
	}
	currency := strings.ToUpper(strings.TrimSpace(in.Currency))
	if currency == "" {
		currency = "GTQ"
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Plan, error) {
		var id uuid.UUID
		err := q.QueryRow(ctx, `
			INSERT INTO shared.plans (key, name, description, price_monthly, price_yearly, currency, limits, sort_order)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
			in.Key, strings.TrimSpace(in.Name), strings.TrimSpace(in.Description), round2(in.PriceMonthly),
			round2(in.PriceYearly), currency, cleanLimits(in.Limits), in.SortOrder).Scan(&id)
		if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Plan{}, ErrPlanKeyTaken.WithField("body.key")
		}
		if err != nil {
			return Plan{}, err
		}
		p, err := planByID(ctx, q, id)
		if err != nil {
			return Plan{}, err
		}
		return p, audit.RecordPlatform(ctx, q, audit.PlatformEntry{
			Actor: &actor, Action: "plan.created", Entity: "plan", EntityID: &id,
			Metadata: map[string]any{"key": p.Key, "name": p.Name, "price_monthly": p.PriceMonthly},
		})
	})
}

// UpdatePlan cambia un plan (plataforma). Los limites nuevos aplican desde el
// siguiente alta o invitacion; no quitan nada a quien ya lo tiene.
func (s *Service) UpdatePlan(ctx context.Context, actor uuid.UUID, id uuid.UUID, in PlanChanges) (Plan, error) {
	if in.Limits != nil {
		if err := validLimits(*in.Limits); err != nil {
			return Plan{}, err
		}
		l := cleanLimits(*in.Limits)
		in.Limits = &l
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Plan, error) {
		before, err := planByID(ctx, q, id)
		if err != nil {
			return Plan{}, err
		}
		if _, err := q.Exec(ctx, `
			UPDATE shared.plans SET
			  name = coalesce($2, name), description = coalesce($3, description),
			  price_monthly = coalesce($4, price_monthly), price_yearly = coalesce($5, price_yearly),
			  limits = coalesce($6, limits), is_active = coalesce($7, is_active), sort_order = coalesce($8, sort_order),
			  updated_at = now()
			WHERE id = $1`, id, trimmedPtr(in.Name), trimmedPtr(in.Description), in.PriceMonthly, in.PriceYearly,
			in.Limits, in.IsActive, in.SortOrder); err != nil {
			return Plan{}, err
		}
		after, err := planByID(ctx, q, id)
		if err != nil {
			return Plan{}, err
		}
		return after, audit.RecordPlatform(ctx, q, audit.PlatformEntry{
			Actor: &actor, Action: "plan.updated", Entity: "plan", EntityID: &id,
			Metadata: map[string]any{"key": after.Key, "before": before, "after": after},
		})
	})
}

func trimmedPtr(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	return &v
}
