package billing

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/core/mail"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// CheckoutInput es el cuerpo de POST /billing/checkout. Sin cambios de plan ni
// de ciclo paga la factura pendiente.
type CheckoutInput struct {
	PlanKey *string `json:"plan_key,omitempty" maxLength:"32" doc:"Plan nuevo (cambia desde hoy y emite su factura)."`
	Cycle   *string `json:"cycle,omitempty" enum:"monthly,yearly"`
}

// CheckoutResult es la respuesta del cobro.
type CheckoutResult struct {
	Status      string   `json:"status" enum:"succeeded,failed,redirect"`
	Message     string   `json:"message"`
	RedirectURL *string  `json:"redirect_url" nullable:"true" doc:"Con una pasarela real: abre esta página para pagar."`
	Overview    Overview `json:"overview"`
}

// issueInvoice emite la factura del periodo de la suscripcion.
func (s *Service) issueInvoice(ctx context.Context, q database.Querier, sub SubscriptionRead, start, end time.Time) (uuid.UUID, error) {
	var seq int64
	if err := q.QueryRow(ctx, `SELECT nextval('shared.invoice_number_seq')`).Scan(&seq); err != nil {
		return uuid.Nil, err
	}
	number := fmt.Sprintf("ARG-%d-%06d", start.Year(), seq)
	subtotal, tax := split(sub.Price)
	desc := fmt.Sprintf("Plan %s · %s · %s al %s", sub.PlanName, cycleLabel(sub.Cycle), shortDate(start), shortDate(end.Add(-time.Second)))
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO shared.invoices (tenant_id, subscription_id, plan_id, number, description, period_start, period_end,
		                             subtotal, tax, total, currency, status, due_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'open', $12) RETURNING id`,
		sub.TenantID, sub.ID, sub.PlanID, number, desc, start, end, subtotal, tax, round2(subtotal+tax), sub.Currency,
		s.now().Add(dueAfter)).Scan(&id)
	if err != nil {
		return uuid.Nil, err
	}
	return id, audit.RecordPlatform(ctx, q, audit.PlatformEntry{
		TenantID: &sub.TenantID, Action: "billing.invoice_issued", Entity: "invoice", EntityID: &id,
		Metadata: map[string]any{"number": number, "total": round2(subtotal + tax), "plan": sub.PlanKey},
	})
}

// Checkout cambia de plan si se pide y cobra la factura pendiente.
func (s *Service) Checkout(ctx context.Context, actor authz.Actor, in CheckoutInput) (CheckoutResult, error) {
	if in.Cycle != nil && !validCycle(*in.Cycle) {
		return CheckoutResult{}, ErrCycle.WithField("body.cycle")
	}
	res, err := database.InTxValue(ctx, s.db, func(q database.Querier) (CheckoutResult, error) {
		if err := lockSubscription(ctx, q, actor.TenantID); err != nil {
			return CheckoutResult{}, err
		}
		sub, err := subscriptionOf(ctx, q, actor.TenantID)
		if err != nil {
			return CheckoutResult{}, err
		}
		if sub, err = s.changePlan(ctx, q, actor, sub, in); err != nil {
			return CheckoutResult{}, err
		}
		if sub.OpenInvoice == nil {
			return CheckoutResult{}, ErrNothingToPay
		}
		return s.charge(ctx, q, &actor.UserID, sub, *sub.OpenInvoice)
	})
	if err != nil || res.Status == ChargeRedirect {
		return res, err
	}
	// El resumen se lee despues del cobro para reflejar el estado nuevo.
	res.Overview, err = s.Overview(ctx, actor.TenantID)
	return res, err
}

// changePlan aplica un cambio de plan o de ciclo: anula lo pendiente, abre un
// periodo nuevo desde hoy y emite su factura. Verifica que el uso quepa en el
// plan nuevo y apaga las banderas que ese plan no incluye.
func (s *Service) changePlan(ctx context.Context, q database.Querier, actor authz.Actor, sub SubscriptionRead, in CheckoutInput) (SubscriptionRead, error) {
	planKey, cycle, changed := target(sub, in)
	if !changed {
		return sub, nil
	}
	plan, off, err := s.admit(ctx, q, sub, planKey)
	if err != nil {
		return sub, err
	}
	now := s.now()
	if _, err := q.Exec(ctx, `UPDATE shared.invoices SET status = 'void', updated_at = now()
		WHERE subscription_id = $1 AND status = 'open'`, sub.ID); err != nil {
		return sub, err
	}
	if _, err := q.Exec(ctx, `
		UPDATE shared.subscriptions SET plan_id = $2, cycle = $3, current_period_start = $4, current_period_end = $5,
		       trial_ends_at = NULL, cancel_at_period_end = false, updated_at = now()
		WHERE id = $1`, sub.ID, plan.ID, cycle, now, addCycle(now, cycle)); err != nil {
		return sub, err
	}
	updated, err := subscriptionOf(ctx, q, sub.TenantID)
	if err != nil {
		return sub, err
	}
	if _, err := s.issueInvoice(ctx, q, updated, now, addCycle(now, cycle)); err != nil {
		return sub, err
	}
	if err := audit.RecordPlatform(ctx, q, audit.PlatformEntry{
		Actor: &actor.UserID, TenantID: &sub.TenantID, Action: "billing.plan_changed", Entity: "subscription", EntityID: &sub.ID,
		Metadata: map[string]any{"from": sub.PlanKey, "to": plan.Key, "cycle": cycle, "features_off": off},
	}); err != nil {
		return sub, err
	}
	return subscriptionOf(ctx, q, sub.TenantID)
}

// target es el plan y el ciclo pedidos, y si cambian algo (una suscripcion
// cancelada siempre se vuelve a contratar).
func target(sub SubscriptionRead, in CheckoutInput) (planKey, cycle string, changed bool) {
	planKey, cycle = sub.PlanKey, sub.Cycle
	if in.PlanKey != nil && strings.TrimSpace(*in.PlanKey) != "" {
		planKey = strings.TrimSpace(*in.PlanKey)
	}
	if in.Cycle != nil {
		cycle = *in.Cycle
	}
	return planKey, cycle, planKey != sub.PlanKey || cycle != sub.Cycle || sub.Status == StatusCancelled
}

// admit valida el plan nuevo (activo y con espacio para lo que se usa) y
// apaga las banderas que no incluye.
func (s *Service) admit(ctx context.Context, q database.Querier, sub SubscriptionRead, planKey string) (Plan, []string, error) {
	plan, err := planByKey(ctx, q, planKey)
	if err != nil {
		return plan, nil, err
	}
	if !plan.IsActive && plan.ID != sub.PlanID {
		return plan, nil, ErrPlanInactive.WithField("body.plan_key")
	}
	if err := fits(ctx, q, sub.TenantID, plan); err != nil {
		return plan, nil, err
	}
	off, err := trimFeatures(ctx, q, sub.TenantID, plan)
	return plan, off, err
}

// fits comprueba que lo que usa la empresa quepa en el plan.
func fits(ctx context.Context, q database.Querier, tenantID uuid.UUID, plan Plan) error {
	u, err := usage(ctx, q, tenantID)
	if err != nil {
		return err
	}
	if m := plan.Limits.MaxProperties; m != nil && u.Properties > *m {
		return limitError("body.plan_key", fmt.Sprintf("El plan %s permite %d propiedades y tienes %d. Desactiva o borra las que sobran antes de cambiar.", plan.Name, *m, u.Properties))
	}
	if m := plan.Limits.MaxUsers; m != nil && u.Users > *m {
		return limitError("body.plan_key", fmt.Sprintf("El plan %s permite %d personas y tienes %d (con invitaciones). Suspende o revoca accesos antes de cambiar.", plan.Name, *m, u.Users))
	}
	return nil
}

// trimFeatures apaga las banderas que el plan no incluye y devuelve cuales.
func trimFeatures(ctx context.Context, q database.Querier, tenantID uuid.UUID, plan Plan) ([]string, error) {
	var features map[string]bool
	if err := q.QueryRow(ctx, `SELECT features FROM shared.tenants WHERE id = $1`, tenantID).Scan(&features); err != nil {
		return nil, err
	}
	off := []string{}
	for k, on := range features {
		if on && !includes(plan, k) {
			features[k] = false
			off = append(off, k)
		}
	}
	if len(off) == 0 {
		return off, nil
	}
	_, err := q.Exec(ctx, `UPDATE shared.tenants SET features = $2, updated_at = now() WHERE id = $1`, tenantID, features)
	return off, err
}

func includes(plan Plan, feature string) bool {
	for _, f := range plan.Limits.Features {
		if f == feature {
			return true
		}
	}
	return false
}

// charge cobra una factura con la pasarela y aplica el resultado.
func (s *Service) charge(ctx context.Context, q database.Querier, actor *uuid.UUID, sub SubscriptionRead, inv InvoiceRead) (CheckoutResult, error) {
	customer, err := s.customer(ctx, q, sub)
	if err != nil {
		return CheckoutResult{}, err
	}
	r, err := s.provider.Checkout(ctx, Charge{
		InvoiceID: inv.ID, Number: inv.Number, Description: inv.Description, Amount: inv.Total, Currency: inv.Currency,
		CustomerRef: customer, ReturnURL: s.webBase + "/configuracion?tab=facturacion",
	})
	if err != nil {
		return CheckoutResult{}, err
	}
	switch r.Status {
	case ChargeSucceeded:
		if err := s.applyPayment(ctx, q, actor, inv, r.PaymentRef, inv.Total, r.Method, map[string]any{"source": "checkout"}); err != nil {
			return CheckoutResult{}, err
		}
		return CheckoutResult{Status: r.Status, Message: "Pago recibido: " + money(inv.Currency, inv.Total) + ". Gracias."}, nil
	case ChargeRedirect:
		url := r.RedirectURL
		return CheckoutResult{Status: r.Status, Message: "Completa el pago en la página de la pasarela.", RedirectURL: &url}, nil
	default:
		if err := s.recordFailure(ctx, q, inv, r.PaymentRef, inv.Total, r.Reason, map[string]any{"source": "checkout"}); err != nil {
			return CheckoutResult{}, err
		}
		return CheckoutResult{Status: ChargeFailed, Message: "No se pudo cobrar: " + r.Reason}, nil
	}
}

// customer registra a la empresa en la pasarela la primera vez.
func (s *Service) customer(ctx context.Context, q database.Querier, sub SubscriptionRead) (string, error) {
	var ref *string
	if err := q.QueryRow(ctx, `SELECT provider_customer_id FROM shared.subscriptions WHERE id = $1`, sub.ID).Scan(&ref); err != nil {
		return "", err
	}
	if ref != nil {
		return *ref, nil
	}
	id, err := s.provider.CreateCustomer(ctx, Customer{TenantID: sub.TenantID, Name: sub.TenantName})
	if err != nil {
		return "", err
	}
	_, err = q.Exec(ctx, `UPDATE shared.subscriptions SET provider_customer_id = $2, provider = $3 WHERE id = $1`,
		sub.ID, id, s.provider.Name())
	return id, err
}

// applyPayment marca la factura pagada y, si no queda nada pendiente, deja la
// suscripcion al dia (una empresa suspendida vuelve a operar).
func (s *Service) applyPayment(ctx context.Context, q database.Querier, actor *uuid.UUID, inv InvoiceRead, ref string, amount float64, method string, raw map[string]any) error {
	tag, err := q.Exec(ctx, `UPDATE shared.invoices SET status = 'paid', paid_at = $2, updated_at = now()
		WHERE id = $1 AND status = 'open'`, inv.ID, s.now())
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO shared.payments (invoice_id, provider, provider_payment_id, amount, currency, status, raw)
		VALUES ($1, $2, $3, $4, $5, 'succeeded', $6)`, inv.ID, s.provider.Name(), ref, amount, inv.Currency, raw); err != nil {
		return err
	}
	if method != "" {
		if _, err := q.Exec(ctx, `UPDATE shared.subscriptions SET payment_method = $2 WHERE tenant_id = $1`, inv.TenantID, method); err != nil {
			return err
		}
	}
	var pending int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM shared.invoices WHERE tenant_id = $1 AND status = 'open'`, inv.TenantID).Scan(&pending); err != nil {
		return err
	}
	if pending == 0 {
		if _, err := q.Exec(ctx, `
			UPDATE shared.subscriptions SET status = 'active', status_changed_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND status IN ('past_due', 'suspended', 'trialing', 'cancelled')`, inv.TenantID); err != nil {
			return err
		}
	}
	if err := audit.RecordPlatform(ctx, q, audit.PlatformEntry{
		Actor: actor, TenantID: &inv.TenantID, Action: "billing.payment_succeeded", Entity: "invoice", EntityID: &inv.ID,
		Metadata: map[string]any{"number": inv.Number, "amount": amount, "provider_payment_id": ref},
	}); err != nil {
		return err
	}
	return s.notify(ctx, q, inv.TenantID, "Pago recibido",
		"recibimos el pago de la factura "+inv.Number+" por "+money(inv.Currency, amount)+".", nil)
}

// recordFailure registra un cobro fallido y avisa.
func (s *Service) recordFailure(ctx context.Context, q database.Querier, inv InvoiceRead, ref string, amount float64, reason string, raw map[string]any) error {
	if reason == "" {
		reason = "La pasarela rechazó el cobro."
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO shared.payments (invoice_id, provider, provider_payment_id, amount, currency, status, failure_reason, raw)
		VALUES ($1, $2, nullif($3, ''), $4, $5, 'failed', $6, $7)`, inv.ID, s.provider.Name(), ref, amount, inv.Currency, reason, raw); err != nil {
		return err
	}
	if err := audit.RecordPlatform(ctx, q, audit.PlatformEntry{
		TenantID: &inv.TenantID, Action: "billing.payment_failed", Entity: "invoice", EntityID: &inv.ID,
		Metadata: map[string]any{"number": inv.Number, "amount": amount, "reason": reason},
	}); err != nil {
		return err
	}
	return s.notify(ctx, q, inv.TenantID, "No pudimos cobrar tu factura",
		"no pudimos cobrar la factura "+inv.Number+" por "+money(inv.Currency, amount)+".",
		[]string{"Motivo: " + reason, "Vence el " + shortDate(inv.DueAt) + ". Revisa el método de pago para evitar la suspensión."})
}

// Cancel deja la suscripcion sin renovar al terminar el periodo.
func (s *Service) Cancel(ctx context.Context, actor authz.Actor) (Overview, error) {
	return s.setCancel(ctx, actor, true)
}

// Resume deshace la cancelacion antes de que termine el periodo.
func (s *Service) Resume(ctx context.Context, actor authz.Actor) (Overview, error) {
	return s.setCancel(ctx, actor, false)
}

func (s *Service) setCancel(ctx context.Context, actor authz.Actor, cancel bool) (Overview, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Overview, error) {
		if err := lockSubscription(ctx, q, actor.TenantID); err != nil {
			return Overview{}, err
		}
		sub, err := subscriptionOf(ctx, q, actor.TenantID)
		if err != nil {
			return Overview{}, err
		}
		if sub.Status == StatusCancelled {
			return Overview{}, ErrCancelled
		}
		if _, err := q.Exec(ctx, `UPDATE shared.subscriptions SET cancel_at_period_end = $2, updated_at = now() WHERE id = $1`,
			sub.ID, cancel); err != nil {
			return Overview{}, err
		}
		action := "billing.cancel_scheduled"
		if !cancel {
			action = "billing.cancel_undone"
		} else if err := s.provider.Cancel(ctx, ""); err != nil {
			return Overview{}, err
		}
		if err := audit.RecordPlatform(ctx, q, audit.PlatformEntry{
			Actor: &actor.UserID, TenantID: &sub.TenantID, Action: action, Entity: "subscription", EntityID: &sub.ID,
			Metadata: map[string]any{"period_end": sub.CurrentPeriodEnd},
		}); err != nil {
			return Overview{}, err
		}
		return overview(ctx, q, actor.TenantID)
	})
}

// ReactivateInput es el cuerpo de POST /platform/subscriptions/{id}/reactivate.
type ReactivateInput struct {
	Days int `json:"days,omitempty" minimum:"0" maximum:"60" doc:"Días extra para pagar lo pendiente (por defecto 7)."`
}

// Reactivate devuelve una empresa suspendida o cancelada a operar (plataforma).
// Lo pendiente vence en Days dias.
func (s *Service) Reactivate(ctx context.Context, actor uuid.UUID, id uuid.UUID, in ReactivateInput) (SubscriptionRead, error) {
	days := in.Days
	if days == 0 {
		days = 7
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (SubscriptionRead, error) {
		sub, err := subscriptionByID(ctx, q, id)
		if err != nil {
			return sub, err
		}
		now := s.now()
		end := sub.CurrentPeriodEnd
		if !end.After(now) {
			end = addCycle(now, sub.Cycle)
		}
		if _, err := q.Exec(ctx, `
			UPDATE shared.subscriptions SET status = 'active', cancel_at_period_end = false, current_period_end = $2,
			       status_changed_at = now(), updated_at = now()
			WHERE id = $1`, id, end); err != nil {
			return sub, err
		}
		if _, err := q.Exec(ctx, `UPDATE shared.invoices SET due_at = $2, updated_at = now()
			WHERE subscription_id = $1 AND status = 'open'`, id, now.AddDate(0, 0, days)); err != nil {
			return sub, err
		}
		if err := audit.RecordPlatform(ctx, q, audit.PlatformEntry{
			Actor: &actor, TenantID: &sub.TenantID, Action: "billing.reactivated", Entity: "subscription", EntityID: &id,
			Metadata: map[string]any{"from": sub.Status, "days": days},
		}); err != nil {
			return sub, err
		}
		return subscriptionByID(ctx, q, id)
	})
}

// TrialInput es el cuerpo de POST /platform/tenants/{id}/trial.
type TrialInput struct {
	Days    int     `json:"days" minimum:"1" maximum:"90"`
	PlanKey *string `json:"plan_key,omitempty" maxLength:"32" doc:"Plan de la prueba; por defecto el actual."`
}

// StartTrial pone a una empresa en prueba por Days dias (plataforma).
func (s *Service) StartTrial(ctx context.Context, actor uuid.UUID, tenantID uuid.UUID, in TrialInput) (SubscriptionRead, error) {
	if in.Days < 1 || in.Days > 90 {
		return SubscriptionRead{}, ErrTrialDays.WithField("body.days")
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (SubscriptionRead, error) {
		sub, err := subscriptionOf(ctx, q, tenantID)
		if err != nil {
			return sub, err
		}
		planID := sub.PlanID
		if in.PlanKey != nil {
			p, err := planByKey(ctx, q, *in.PlanKey)
			if err != nil {
				return sub, err
			}
			planID = p.ID
		}
		now := s.now()
		ends := now.AddDate(0, 0, in.Days)
		if _, err := q.Exec(ctx, `UPDATE shared.invoices SET status = 'void', updated_at = now()
			WHERE subscription_id = $1 AND status = 'open'`, sub.ID); err != nil {
			return sub, err
		}
		if _, err := q.Exec(ctx, `
			UPDATE shared.subscriptions SET status = 'trialing', plan_id = $2, trial_ends_at = $3,
			       current_period_start = $4, current_period_end = $3, cancel_at_period_end = false,
			       status_changed_at = now(), updated_at = now()
			WHERE id = $1`, sub.ID, planID, ends, now); err != nil {
			return sub, err
		}
		if err := audit.RecordPlatform(ctx, q, audit.PlatformEntry{
			Actor: &actor, TenantID: &tenantID, Action: "billing.trial_started", Entity: "subscription", EntityID: &sub.ID,
			Metadata: map[string]any{"days": in.Days, "ends_at": ends},
		}); err != nil {
			return sub, err
		}
		return subscriptionOf(ctx, q, tenantID)
	})
}

// EnsureSubscription crea la suscripcion de una empresa nueva: en prueba por
// trialDays en el plan indicado (o DefaultPlan). Lo llama el alta de empresas
// dentro de su transaccion.
func (s *Service) EnsureSubscription(ctx context.Context, q database.Querier, tenantID uuid.UUID, planKey string, trialDays int) error {
	if planKey == "" {
		planKey = DefaultPlan
	}
	if trialDays <= 0 {
		trialDays = DefaultTrialDays
	}
	plan, err := planByKey(ctx, q, planKey)
	if err != nil {
		return err
	}
	now := s.now()
	ends := now.AddDate(0, 0, trialDays)
	_, err = q.Exec(ctx, `
		INSERT INTO shared.subscriptions (tenant_id, plan_id, cycle, status, current_period_start, current_period_end,
		                                  trial_ends_at, provider)
		VALUES ($1, $2, 'monthly', 'trialing', $3, $4, $4, $5)
		ON CONFLICT (tenant_id) DO NOTHING`, tenantID, plan.ID, now, ends, s.provider.Name())
	return err
}

// notify escribe a quien administra la facturacion de la empresa.
func (s *Service) notify(ctx context.Context, q database.Querier, tenantID uuid.UUID, title, summary string, lines []string) error {
	var tenantName string
	if err := q.QueryRow(ctx, `SELECT name FROM shared.tenants WHERE id = $1`, tenantID).Scan(&tenantName); err != nil {
		return err
	}
	rows, err := q.Query(ctx, `
		SELECT DISTINCT u.email, u.full_name
		FROM shared.memberships m
		JOIN shared.users u ON u.id = m.user_id AND u.is_active
		JOIN shared.role_permissions rp ON rp.role_id = m.role_id AND rp.permission = $2
		WHERE m.tenant_id = $1 AND m.status = 'active'`, tenantID, string(authz.TenantBilling))
	if err != nil {
		return err
	}
	type person struct{ email, name string }
	var people []person
	for rows.Next() {
		var p person
		if err := rows.Scan(&p.email, &p.name); err != nil {
			rows.Close()
			return err
		}
		people = append(people, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if lines == nil {
		lines = []string{}
	}
	for _, p := range people {
		name := p.name
		if f := strings.Fields(name); len(f) > 0 {
			name = f[0]
		}
		if err := jobs.EnqueueEmail(ctx, q, &tenantID, jobs.Email{To: p.email, Template: mail.TemplateBillingNotice, Data: map[string]any{
			"Name": name, "TenantName": tenantName, "Title": title, "Summary": summary, "Lines": lines,
			"Link": s.webBase + "/configuracion?tab=facturacion",
		}}); err != nil {
			return err
		}
	}
	return nil
}
