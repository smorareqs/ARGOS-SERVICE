package maintenance

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/optional"
	"github.com/smorareqs/argos-service/internal/modules/alerts"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// selectPlan recibe $1 = hoy (AAAA-MM-DD) para calcular due_state.
const selectPlan = `
	SELECT mp.id, mp.asset_id, a.name AS asset_name, a.category AS asset_category,
	       a.property_id, p.code AS property_code, b.name AS building_name,
	       mp.name, mp.frequency_days, mp.responsible_user_id, u.full_name AS responsible_name,
	       mp.vendor_id, v.name AS vendor_name,
	       to_char(mp.last_done_at, 'YYYY-MM-DD') AS last_done_at, to_char(mp.next_due_at, 'YYYY-MM-DD') AS next_due_at,
	       mp.lead_days, mp.is_active,
	       CASE WHEN NOT mp.is_active OR NOT a.is_active THEN 'inactive'
	            WHEN mp.next_due_at < $1::date THEN 'overdue'
	            WHEN mp.next_due_at - mp.lead_days <= $1::date THEN 'upcoming'
	            ELSE 'ok' END AS due_state,
	       ot.id AS open_ticket_id, ot.number AS open_ticket_number,
	       mp.created_at, mp.updated_at
	FROM maintenance_plans mp
	JOIN assets a ON a.id = mp.asset_id
	LEFT JOIN properties p ON p.id = a.property_id
	LEFT JOIN buildings b ON b.id = coalesce(a.building_id, p.building_id)
	LEFT JOIN shared.users u ON u.id = mp.responsible_user_id
	LEFT JOIN vendors v ON v.id = mp.vendor_id
	LEFT JOIN maintenance_tickets ot ON ot.plan_id = mp.id AND ot.status IN ('open', 'assigned', 'in_progress')`

// listPlans lista planes con una condicion; $1 es hoy y los demas args
// empiezan en $2.
func listPlans(ctx context.Context, q database.Querier, today, where string, args ...any) ([]PlanRead, error) {
	rows, err := q.Query(ctx, selectPlan+` WHERE `+where+` ORDER BY mp.next_due_at, mp.name`, append([]any{today}, args...)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[PlanRead])
}

// Plans lista los preventivos del alcance por vencimiento (calendario W-G3).
func (s *Service) Plans(ctx context.Context, actor authz.Actor, f PlanFilter) ([]PlanRead, error) {
	if err := validDate(optionalText(f.From), "query.from"); err != nil {
		return nil, err
	}
	if err := validDate(optionalText(f.To), "query.to"); err != nil {
		return nil, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]PlanRead, error) {
		scope, err := scopeOf(ctx, q, actor)
		if err != nil {
			return nil, err
		}
		today, err := s.today(ctx, q)
		if err != nil {
			return nil, err
		}
		all, ids := scope.SQLArgs()
		return listPlans(ctx, q, today, `($2::bool OR a.property_id = ANY($3::uuid[])
			OR a.building_id IN (SELECT building_id FROM properties WHERE id = ANY($3::uuid[]) AND building_id IS NOT NULL))
			AND ($4::text = '' OR mp.next_due_at >= $4::date)
			AND ($5::text = '' OR mp.next_due_at <= $5::date)
			AND ($6::uuid IS NULL OR mp.asset_id = $6)
			AND ($7::bool IS NULL OR (mp.is_active AND a.is_active) = $7)`,
			all, ids, f.From, f.To, f.AssetID, f.Active)
	})
}

// CreatePlan agrega un preventivo al activo.
func (s *Service) CreatePlan(ctx context.Context, actor authz.Actor, assetID uuid.UUID, in PlanCreate) (PlanRead, error) {
	if err := validDate(in.LastDoneAt, "body.last_done_at"); err != nil {
		return PlanRead{}, err
	}
	if err := validDate(in.NextDueAt, "body.next_due_at"); err != nil {
		return PlanRead{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (PlanRead, error) {
		a, err := s.visibleAsset(ctx, q, actor, assetID)
		if err != nil {
			return PlanRead{}, err
		}
		if err := s.checkPlanRefs(ctx, q, actor, in.ResponsibleID, in.VendorID); err != nil {
			return PlanRead{}, err
		}
		today, err := s.today(ctx, q)
		if err != nil {
			return PlanRead{}, err
		}
		next := addDays(today, in.FrequencyDays)
		switch {
		case in.NextDueAt != nil:
			next = *in.NextDueAt
		case in.LastDoneAt != nil:
			next = addDays(*in.LastDoneAt, in.FrequencyDays)
		}
		lead := 7
		if in.LeadDays != nil {
			lead = *in.LeadDays
		}
		vendor := in.VendorID
		if vendor == nil {
			vendor = a.VendorID
		}
		var id uuid.UUID
		err = q.QueryRow(ctx, `
			INSERT INTO maintenance_plans (asset_id, name, frequency_days, responsible_user_id, vendor_id,
			                               last_done_at, next_due_at, lead_days)
			VALUES ($1, $2, $3, $4, $5, $6::date, $7::date, $8) RETURNING id`,
			assetID, trim(in.Name), in.FrequencyDays, in.ResponsibleID, vendor, in.LastDoneAt, next, lead).Scan(&id)
		if err != nil {
			return PlanRead{}, err
		}
		p, err := planByID(ctx, q, today, id)
		if err != nil {
			return PlanRead{}, err
		}
		return p, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "maintenance_plan.created", Entity: "maintenance_plan", EntityID: &id,
			PropertyID: a.PropertyID, After: planSnapshot(p),
		})
	})
}

// UpdatePlan cambia los datos que vienen. Desactivarlo resuelve su alerta de
// vencido.
func (s *Service) UpdatePlan(ctx context.Context, actor authz.Actor, assetID, planID uuid.UUID, in PlanUpdate) (PlanRead, error) {
	if err := validDate(in.NextDueAt, "body.next_due_at"); err != nil {
		return PlanRead{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (PlanRead, error) {
		if _, err := s.visibleAsset(ctx, q, actor, assetID); err != nil {
			return PlanRead{}, err
		}
		today, err := s.today(ctx, q)
		if err != nil {
			return PlanRead{}, err
		}
		cur, err := planByID(ctx, q, today, planID)
		if err != nil || cur.AssetID != assetID {
			return PlanRead{}, ErrPlanNotFound
		}
		responsible := optional.Apply(cur.ResponsibleID, in.ResponsibleID)
		vendor := optional.Apply(cur.VendorID, in.VendorID)
		if err := s.checkPlanRefs(ctx, q, actor, changed(cur.ResponsibleID, responsible), changed(cur.VendorID, vendor)); err != nil {
			return PlanRead{}, err
		}
		v := mergePlan(cur, in)
		if _, err := q.Exec(ctx, `
			UPDATE maintenance_plans SET name = $2, frequency_days = $3, responsible_user_id = $4, vendor_id = $5,
			       next_due_at = $6::date, lead_days = $7, is_active = $8, updated_at = now()
			WHERE id = $1`, planID, v.Name, v.FrequencyDays, responsible, vendor, v.NextDueAt, v.LeadDays, v.IsActive); err != nil {
			return PlanRead{}, err
		}
		p, err := planByID(ctx, q, today, planID)
		if err != nil {
			return PlanRead{}, err
		}
		if p.DueState != "overdue" {
			if _, err := alerts.ResolveFor(ctx, q, alerts.TypeMaintenanceOverdue, "maintenance_plan", planID, nil); err != nil {
				return PlanRead{}, err
			}
		}
		return p, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "maintenance_plan.updated", Entity: "maintenance_plan", EntityID: &planID,
			PropertyID: p.PropertyID, Before: planSnapshot(cur), After: planSnapshot(p),
		})
	})
}

// mergePlan aplica los campos simples del PATCH sobre el plan actual.
func mergePlan(cur PlanRead, in PlanUpdate) PlanRead {
	if in.Name != nil {
		cur.Name = trim(*in.Name)
	}
	if in.FrequencyDays != nil {
		cur.FrequencyDays = *in.FrequencyDays
	}
	if in.NextDueAt != nil {
		cur.NextDueAt = *in.NextDueAt
	}
	if in.LeadDays != nil {
		cur.LeadDays = *in.LeadDays
	}
	if in.IsActive != nil {
		cur.IsActive = *in.IsActive
	}
	return cur
}

// checkPlanRefs valida responsable (miembro que atiende mantenimiento) y proveedor.
func (s *Service) checkPlanRefs(ctx context.Context, q database.Querier, actor authz.Actor, responsible, vendor *uuid.UUID) error {
	if responsible != nil {
		ok, err := canExecute(ctx, q, actor.TenantID, *responsible)
		if err != nil {
			return err
		}
		if !ok {
			return ErrInvalidAssignee.WithField("body.responsible_user_id")
		}
	}
	return checkVendor(ctx, q, vendor, "body.vendor_id")
}

// advancePlan registra un preventivo hecho hoy: next_due_at avanza una
// frecuencia (sin quedar en el pasado) y se resuelve la alerta de vencido.
func advancePlan(ctx context.Context, q database.Querier, planID uuid.UUID, today string) error {
	if _, err := q.Exec(ctx, `
		UPDATE maintenance_plans
		SET last_done_at = $2::date,
		    next_due_at = next_due_at + frequency_days * greatest(1,
		        ceil(($2::date - next_due_at + 1)::numeric / frequency_days)::int),
		    updated_at = now()
		WHERE id = $1`, planID, today); err != nil {
		return err
	}
	_, err := alerts.ResolveFor(ctx, q, alerts.TypeMaintenanceOverdue, "maintenance_plan", planID, nil)
	return err
}

func planByID(ctx context.Context, q database.Querier, today string, id uuid.UUID) (PlanRead, error) {
	rows, err := q.Query(ctx, selectPlan+` WHERE mp.id = $2`, today, id)
	if err != nil {
		return PlanRead{}, err
	}
	p, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[PlanRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return PlanRead{}, ErrPlanNotFound
	}
	return p, err
}

func planSnapshot(p PlanRead) map[string]any {
	return map[string]any{
		"name": p.Name, "frequency_days": p.FrequencyDays, "next_due_at": p.NextDueAt, "lead_days": p.LeadDays,
		"responsible_user_id": p.ResponsibleID, "vendor_id": p.VendorID, "is_active": p.IsActive,
	}
}

// changed devuelve next si cambio respecto de cur (para validar solo lo nuevo).
func changed(cur, next *uuid.UUID) *uuid.UUID {
	if next == nil || (cur != nil && *cur == *next) {
		return nil
	}
	return next
}

func optionalText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
