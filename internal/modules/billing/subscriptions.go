package billing

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
)

// SubscriptionRead es la suscripcion de una empresa.
type SubscriptionRead struct {
	ID                 uuid.UUID    `json:"id" format:"uuid"`
	TenantID           uuid.UUID    `json:"tenant_id" format:"uuid"`
	TenantName         string       `json:"tenant_name"`
	TenantSlug         string       `json:"tenant_slug"`
	PlanID             uuid.UUID    `json:"plan_id" format:"uuid"`
	PlanKey            string       `json:"plan_key"`
	PlanName           string       `json:"plan_name"`
	Cycle              string       `json:"cycle" enum:"monthly,yearly"`
	Price              float64      `json:"price" doc:"Precio del plan en el ciclo actual (IVA incluido)."`
	Currency           string       `json:"currency"`
	Limits             Limits       `json:"limits"`
	Status             string       `json:"status" enum:"trialing,active,past_due,suspended,cancelled"`
	StatusLabel        string       `json:"status_label"`
	ReadOnly           bool         `json:"read_only" doc:"true: la empresa solo puede leer (402 en escrituras)."`
	CurrentPeriodStart time.Time    `json:"current_period_start"`
	CurrentPeriodEnd   time.Time    `json:"current_period_end"`
	TrialEndsAt        *time.Time   `json:"trial_ends_at" nullable:"true"`
	GraceDays          int          `json:"grace_days"`
	CancelAtPeriodEnd  bool         `json:"cancel_at_period_end"`
	Provider           string       `json:"provider"`
	PaymentMethod      *string      `json:"payment_method" nullable:"true" doc:"Lo que dice la pasarela (\"Visa terminada en 4242\")."`
	StatusChangedAt    time.Time    `json:"status_changed_at"`
	OpenInvoice        *InvoiceRead `json:"open_invoice,omitempty" doc:"La factura pendiente más antigua (no viene si no hay)."`
	SuspendsAt         *time.Time   `json:"suspends_at" nullable:"true" doc:"Cuándo pasa a solo lectura si no se paga."`
	FailedPayments     int          `json:"failed_payments" doc:"Cobros fallidos en los últimos 90 días."`
	LastPaymentAt      *time.Time   `json:"last_payment_at" nullable:"true"`
}

// Usage es lo que usa la empresa frente a los limites del plan.
type Usage struct {
	Properties   int   `json:"properties"`
	Users        int   `json:"users" doc:"Personas con acceso o invitadas (sin huéspedes)."`
	StorageBytes int64 `json:"storage_bytes"`
}

// Overview es la pantalla de facturacion de la empresa (W-O4).
type Overview struct {
	Subscription SubscriptionRead `json:"subscription"`
	Usage        Usage            `json:"usage"`
	Plans        []Plan           `json:"plans" doc:"Planes que se pueden contratar."`
}

// InvoiceRead es una factura.
type InvoiceRead struct {
	ID          uuid.UUID  `json:"id" format:"uuid"`
	TenantID    uuid.UUID  `json:"tenant_id" format:"uuid"`
	Number      string     `json:"number"`
	Description string     `json:"description"`
	PlanName    string     `json:"plan_name"`
	PeriodStart time.Time  `json:"period_start"`
	PeriodEnd   time.Time  `json:"period_end"`
	Subtotal    float64    `json:"subtotal"`
	Tax         float64    `json:"tax"`
	Total       float64    `json:"total"`
	Currency    string     `json:"currency"`
	Status      string     `json:"status" enum:"draft,open,paid,void,uncollectible"`
	DueAt       time.Time  `json:"due_at"`
	PaidAt      *time.Time `json:"paid_at" nullable:"true"`
	LastError   *string    `json:"last_error" nullable:"true" doc:"Motivo del último cobro fallido."`
	CreatedAt   time.Time  `json:"created_at"`
}

const selectInvoice = `
	SELECT i.id, i.tenant_id, i.number, i.description, p.name, i.period_start, i.period_end, i.subtotal::float8,
	       i.tax::float8, i.total::float8, trim(i.currency), i.status, i.due_at, i.paid_at,
	       (SELECT pm.failure_reason FROM shared.payments pm WHERE pm.invoice_id = i.id AND pm.status = 'failed'
	        ORDER BY pm.created_at DESC LIMIT 1),
	       i.created_at
	FROM shared.invoices i JOIN shared.plans p ON p.id = i.plan_id`

func scanInvoice(row pgx.Row) (InvoiceRead, error) {
	var i InvoiceRead
	err := row.Scan(&i.ID, &i.TenantID, &i.Number, &i.Description, &i.PlanName, &i.PeriodStart, &i.PeriodEnd,
		&i.Subtotal, &i.Tax, &i.Total, &i.Currency, &i.Status, &i.DueAt, &i.PaidAt, &i.LastError, &i.CreatedAt)
	return i, err
}

func invoicesWhere(ctx context.Context, q database.Querier, where string, args ...any) ([]InvoiceRead, error) {
	rows, err := q.Query(ctx, selectInvoice+" WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InvoiceRead{}
	for rows.Next() {
		i, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

const selectSubscription = `
	SELECT s.id, s.tenant_id, t.name, t.slug, p.id, p.key, p.name, s.cycle,
	       CASE WHEN s.cycle = 'yearly' THEN p.price_yearly ELSE p.price_monthly END::float8, trim(p.currency), p.limits,
	       s.status, s.current_period_start, s.current_period_end, s.trial_ends_at, s.grace_days,
	       s.cancel_at_period_end, s.provider, s.payment_method, s.status_changed_at,
	       (SELECT count(*)::int FROM shared.payments pm JOIN shared.invoices i ON i.id = pm.invoice_id
	        WHERE i.subscription_id = s.id AND pm.status = 'failed' AND pm.created_at > now() - interval '90 days'),
	       (SELECT max(pm.created_at) FROM shared.payments pm JOIN shared.invoices i ON i.id = pm.invoice_id
	        WHERE i.subscription_id = s.id AND pm.status = 'succeeded')
	FROM shared.subscriptions s
	JOIN shared.tenants t ON t.id = s.tenant_id
	JOIN shared.plans p ON p.id = s.plan_id`

func scanSubscription(row pgx.Row) (SubscriptionRead, error) {
	var s SubscriptionRead
	err := row.Scan(&s.ID, &s.TenantID, &s.TenantName, &s.TenantSlug, &s.PlanID, &s.PlanKey, &s.PlanName, &s.Cycle,
		&s.Price, &s.Currency, &s.Limits, &s.Status, &s.CurrentPeriodStart, &s.CurrentPeriodEnd, &s.TrialEndsAt,
		&s.GraceDays, &s.CancelAtPeriodEnd, &s.Provider, &s.PaymentMethod, &s.StatusChangedAt, &s.FailedPayments,
		&s.LastPaymentAt)
	if s.Limits.Features == nil {
		s.Limits.Features = []string{}
	}
	s.StatusLabel, s.ReadOnly = StatusLabel(s.Status), ReadOnly(s.Status)
	return s, err
}

// withOpenInvoice completa la factura pendiente y la fecha de suspension.
func withOpenInvoice(ctx context.Context, q database.Querier, s SubscriptionRead) (SubscriptionRead, error) {
	open, err := invoicesWhere(ctx, q, `i.subscription_id = $1 AND i.status = 'open' ORDER BY i.due_at LIMIT 1`, s.ID)
	if err != nil {
		return s, err
	}
	if len(open) == 1 {
		s.OpenInvoice = &open[0]
		if !ReadOnly(s.Status) {
			at := open[0].DueAt.AddDate(0, 0, s.GraceDays)
			s.SuspendsAt = &at
		}
	}
	return s, nil
}

func subscriptionOf(ctx context.Context, q database.Querier, tenantID uuid.UUID) (SubscriptionRead, error) {
	s, err := scanSubscription(q.QueryRow(ctx, selectSubscription+` WHERE s.tenant_id = $1`, tenantID))
	if err != nil {
		return s, notFound(err, ErrSubscriptionNotFound)
	}
	return withOpenInvoice(ctx, q, s)
}

func subscriptionByID(ctx context.Context, q database.Querier, id uuid.UUID) (SubscriptionRead, error) {
	s, err := scanSubscription(q.QueryRow(ctx, selectSubscription+` WHERE s.id = $1`, id))
	if err != nil {
		return s, notFound(err, ErrSubscriptionNotFound)
	}
	return withOpenInvoice(ctx, q, s)
}

func lockSubscription(ctx context.Context, q database.Querier, tenantID uuid.UUID) error {
	var id uuid.UUID
	err := q.QueryRow(ctx, `SELECT id FROM shared.subscriptions WHERE tenant_id = $1 FOR UPDATE`, tenantID).Scan(&id)
	return notFound(err, ErrSubscriptionNotFound)
}

// usage cuenta propiedades (schema de la empresa), personas y almacenamiento.
func usage(ctx context.Context, q database.Querier, tenantID uuid.UUID) (Usage, error) {
	var u Usage
	if err := q.QueryRow(ctx, `SELECT count(*)::int FROM properties WHERE deleted_at IS NULL`).Scan(&u.Properties); err != nil {
		return u, err
	}
	if err := q.QueryRow(ctx, `SELECT coalesce(sum(size_bytes), 0)::bigint FROM files WHERE status = 'ready'`).Scan(&u.StorageBytes); err != nil {
		return u, err
	}
	n, err := users(ctx, q, tenantID)
	u.Users = n
	return u, err
}

// users cuenta las personas con membresia (no huespedes) y las invitaciones
// pendientes de correos que aun no son miembros.
func users(ctx context.Context, q database.Querier, tenantID uuid.UUID) (int, error) {
	var n int
	err := q.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM shared.memberships m JOIN shared.roles r ON r.id = m.role_id
		        WHERE m.tenant_id = $1 AND r.key <> 'guest')
		     + (SELECT count(*) FROM shared.invitations i
		        WHERE i.tenant_id = $1 AND i.status = 'pending'
		          AND NOT EXISTS (SELECT 1 FROM shared.memberships m JOIN shared.users u ON u.id = m.user_id
		                          WHERE m.tenant_id = $1 AND lower(u.email) = lower(i.email)))`, tenantID).Scan(&n)
	return n, err
}

// Overview arma la pantalla de facturacion de la empresa activa.
func (s *Service) Overview(ctx context.Context, tenantID uuid.UUID) (Overview, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Overview, error) {
		return overview(ctx, q, tenantID)
	})
}

func overview(ctx context.Context, q database.Querier, tenantID uuid.UUID) (Overview, error) {
	sub, err := subscriptionOf(ctx, q, tenantID)
	if err != nil {
		return Overview{}, err
	}
	u, err := usage(ctx, q, tenantID)
	if err != nil {
		return Overview{}, err
	}
	ps, err := plans(ctx, q, true)
	return Overview{Subscription: sub, Usage: u, Plans: ps}, err
}

// Invoices lista las facturas de la empresa (recientes primero).
func (s *Service) Invoices(ctx context.Context, tenantID uuid.UUID) ([]InvoiceRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]InvoiceRead, error) {
		return invoicesWhere(ctx, q, `i.tenant_id = $1 AND i.status <> 'draft' ORDER BY i.created_at DESC LIMIT 60`, tenantID)
	})
}

// SubscriptionFilter filtra la lista de la plataforma.
type SubscriptionFilter struct {
	Status string
}

// Subscriptions lista las suscripciones de todas las empresas (W-P3).
func (s *Service) Subscriptions(ctx context.Context, f SubscriptionFilter) ([]SubscriptionRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]SubscriptionRead, error) {
		rows, err := q.Query(ctx, selectSubscription+`
			WHERE $1 = '' OR s.status = $1
			ORDER BY CASE s.status WHEN 'suspended' THEN 0 WHEN 'past_due' THEN 1 WHEN 'trialing' THEN 2 ELSE 3 END,
			         t.name`, f.Status)
		if err != nil {
			return nil, err
		}
		var list []SubscriptionRead
		for rows.Next() {
			sub, err := scanSubscription(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			list = append(list, sub)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		out := make([]SubscriptionRead, 0, len(list))
		for _, sub := range list {
			full, err := withOpenInvoice(ctx, q, sub)
			if err != nil {
				return nil, err
			}
			out = append(out, full)
		}
		return out, nil
	})
}

// TenantInvoices lista las facturas de una empresa (plataforma).
func (s *Service) TenantInvoices(ctx context.Context, tenantID uuid.UUID) ([]InvoiceRead, error) {
	return s.Invoices(ctx, tenantID)
}
