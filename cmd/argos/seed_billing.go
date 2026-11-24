package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
)

// Facturacion de demostracion (fase 6): Garrent al dia en el plan Completo
// con tres meses pagados; Costa Limpia en Operacion con la factura de este mes
// vencida y un cobro rechazado, para ver el aviso de pago vencido. Solo la
// primera vez (si la empresa ya tiene facturas, no toca nada).

type demoBilling struct {
	plan        string
	paidMonths  int
	overdueDays int // > 0: la factura del mes vencio hace esos dias
}

var demoBillings = map[string]demoBilling{
	"garrent":      {plan: "completo", paidMonths: 3},
	"costa_limpia": {plan: "operacion", paidMonths: 2, overdueDays: 3},
}

func seedBilling(ctx context.Context, db *database.DB, ids map[string]tenants.Tenant) error {
	for slug, d := range demoBillings {
		t, ok := ids[slug]
		if !ok {
			continue
		}
		if err := db.InTx(ctx, func(q database.Querier) error { return seedTenantBilling(ctx, q, t.ID, d) }); err != nil {
			return fmt.Errorf("facturación de %s: %w", slug, err)
		}
	}
	fmt.Println("Facturación de demostración lista.")
	return nil
}

func seedTenantBilling(ctx context.Context, q database.Querier, tenantID uuid.UUID, d demoBilling) error {
	var invoices int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM shared.invoices WHERE tenant_id = $1`, tenantID).Scan(&invoices); err != nil || invoices > 0 {
		return err
	}
	var subID, planID uuid.UUID
	var price float64
	var planName string
	if err := q.QueryRow(ctx, `SELECT id, price_monthly::float8, name FROM shared.plans WHERE key = $1`, d.plan).
		Scan(&planID, &price, &planName); err != nil {
		return err
	}
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if err := q.QueryRow(ctx, `
		INSERT INTO shared.subscriptions (tenant_id, plan_id, cycle, status, current_period_start, current_period_end,
		                                  provider, provider_customer_id, payment_method)
		VALUES ($1, $2, 'monthly', 'active', $3, $4, 'fake', 'fake_cus_demo', 'Tarjeta de prueba terminada en 4242')
		ON CONFLICT (tenant_id) DO UPDATE SET plan_id = EXCLUDED.plan_id, cycle = 'monthly', status = 'active',
		  current_period_start = EXCLUDED.current_period_start, current_period_end = EXCLUDED.current_period_end,
		  trial_ends_at = NULL, provider_customer_id = EXCLUDED.provider_customer_id, payment_method = EXCLUDED.payment_method
		RETURNING id`, tenantID, planID, start, start.AddDate(0, 1, 0)).Scan(&subID); err != nil {
		return err
	}
	subtotal := float64(int64(price/1.12*100+0.5)) / 100
	inv := demoInvoice{
		tenantID: tenantID, subID: subID, planID: planID, plan: planName, subtotal: subtotal,
		tax: float64(int64((price-subtotal)*100+0.5)) / 100,
	}
	for i := d.paidMonths; i >= 0; i-- {
		inv.from = start.AddDate(0, -i, 0)
		inv.open = i == 0 && d.overdueDays > 0
		inv.due = inv.from.AddDate(0, 0, 5)
		if inv.open {
			inv.due = now.AddDate(0, 0, -d.overdueDays)
		}
		if err := seedInvoice(ctx, q, inv); err != nil {
			return err
		}
	}
	if d.overdueDays > 0 {
		_, err := q.Exec(ctx, `UPDATE shared.subscriptions SET status = 'past_due', status_changed_at = now() WHERE id = $1`, subID)
		return err
	}
	return nil
}

type demoInvoice struct {
	tenantID, subID, planID uuid.UUID
	plan                    string
	subtotal, tax           float64
	from, due               time.Time
	open                    bool
}

// seedInvoice crea una factura pagada (con su pago) o abierta (con un cobro
// rechazado).
func seedInvoice(ctx context.Context, q database.Querier, d demoInvoice) error {
	status, paidAt := "paid", &d.due
	pay, reason := "succeeded", (*string)(nil)
	if d.open {
		r := "La tarjeta fue rechazada por el banco emisor."
		status, paidAt, pay, reason = "open", nil, "failed", &r
	}
	var seq int64
	if err := q.QueryRow(ctx, `SELECT nextval('shared.invoice_number_seq')`).Scan(&seq); err != nil {
		return err
	}
	var invID uuid.UUID
	if err := q.QueryRow(ctx, `
		INSERT INTO shared.invoices (tenant_id, subscription_id, plan_id, number, description, period_start, period_end,
		                             subtotal, tax, total, currency, status, due_at, paid_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'GTQ', $11, $12, $13, $6) RETURNING id`,
		d.tenantID, d.subID, d.planID, fmt.Sprintf("ARG-%d-%06d", d.from.Year(), seq),
		fmt.Sprintf("Plan %s · mensual · %s", d.plan, d.from.Format("01/2006")), d.from, d.from.AddDate(0, 1, 0),
		d.subtotal, d.tax, d.subtotal+d.tax, status, d.due, paidAt).Scan(&invID); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `
		INSERT INTO shared.payments (invoice_id, provider, provider_payment_id, amount, currency, status, failure_reason, created_at)
		VALUES ($1, 'fake', $2, $3, 'GTQ', $4, $5, $6)`, invID, "fake_pay_demo_"+invID.String()[:8], d.subtotal+d.tax, pay, reason, d.due)
	return err
}
