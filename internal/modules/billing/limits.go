package billing

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
)

// Limites que se verifican al crear (fase06.md §3.2).
const (
	LimitProperties = "properties"
	LimitUsers      = "users"
)

var featureLabels = map[string]string{
	"reservations": "Reservas", "parking": "Parqueos", "shared_areas": "Áreas comunes",
	"guest_chat": "Chat con huéspedes", "payroll": "Nómina", "smart_locks": "Cerraduras",
}

func limitError(field, msg string) error {
	return apperr.New(apperr.KindValidation, msg).WithField(field)
}

// CheckLimit responde 422 si la empresa ya llego al limite de su plan para
// crear uno mas (propiedades o personas). Sin suscripcion no hay limite.
func (s *Service) CheckLimit(ctx context.Context, q database.Querier, tenantID uuid.UUID, kind, field string) error {
	var name string
	var limits Limits
	err := q.QueryRow(ctx, `
		SELECT p.name, p.limits FROM shared.subscriptions s JOIN shared.plans p ON p.id = s.plan_id
		WHERE s.tenant_id = $1`, tenantID).Scan(&name, &limits)
	if err != nil {
		return notFound(err, nil)
	}
	switch kind {
	case LimitProperties:
		if limits.MaxProperties == nil {
			return nil
		}
		var n int
		if err := q.QueryRow(ctx, `SELECT count(*)::int FROM properties WHERE deleted_at IS NULL`).Scan(&n); err != nil {
			return err
		}
		if n >= *limits.MaxProperties {
			return limitError(field, fmt.Sprintf("Tu plan %s permite %d propiedades. Cambia de plan en Configuración › Facturación para agregar más.", name, *limits.MaxProperties))
		}
	case LimitUsers:
		if limits.MaxUsers == nil {
			return nil
		}
		n, err := users(ctx, q, tenantID)
		if err != nil {
			return err
		}
		if n >= *limits.MaxUsers {
			return limitError(field, fmt.Sprintf("Tu plan %s permite %d personas (contando invitaciones). Cambia de plan en Configuración › Facturación para invitar a más.", name, *limits.MaxUsers))
		}
	}
	return nil
}

// CheckFeatures responde 422 si se quiere activar una bandera que el plan de
// la empresa no incluye. Sin suscripcion no hay restriccion.
func (s *Service) CheckFeatures(ctx context.Context, q database.Querier, tenantID uuid.UUID, features map[string]bool) error {
	var name string
	var limits Limits
	err := q.QueryRow(ctx, `
		SELECT p.name, p.limits FROM shared.subscriptions s JOIN shared.plans p ON p.id = s.plan_id
		WHERE s.tenant_id = $1`, tenantID).Scan(&name, &limits)
	if err != nil {
		return notFound(err, nil)
	}
	plan := Plan{Limits: limits}
	var missing []string
	for k, on := range features {
		if on && !includes(plan, k) {
			label := featureLabels[k]
			if label == "" {
				label = k
			}
			missing = append(missing, label)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return limitError("body.features", fmt.Sprintf("El plan %s no incluye %s.", name, strings.Join(missing, ", ")))
}
