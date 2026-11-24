package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/modules/audit"
)

// state es lo que decide el ciclo de una suscripcion.
type state struct {
	Status      string
	PeriodEnd   time.Time
	TrialEndsAt *time.Time
	CancelAtEnd bool
	GraceDays   int
	// OpenDue es el vencimiento de la factura pendiente mas antigua.
	OpenDue *time.Time
}

// decision es lo que hay que hacer con ella.
type decision struct {
	// Renew abre el periodo siguiente y lo factura.
	Renew bool
	// Status es el estado nuevo ("" = sin cambio).
	Status string
}

// decide aplica las reglas de fase06.md §3.2:
//   - una prueba que termino abre su primer periodo pagado (factura);
//   - un periodo que termino se renueva, salvo cancelacion programada
//     (cancelled) o empresa suspendida (no se le sigue facturando);
//   - una factura vencida pasa la suscripcion a past_due y, tras grace_days,
//     a suspended (solo lectura).
func decide(s state, now time.Time) decision {
	if s.Status == StatusCancelled {
		return decision{}
	}
	d, status := periodDecision(s, now)
	if d.Status == StatusCancelled {
		return d
	}
	status = debtStatus(s, status, now)
	if status != s.Status {
		d.Status = status
	}
	return d
}

// periodDecision resuelve el fin de la prueba o del periodo.
func periodDecision(s state, now time.Time) (decision, string) {
	switch {
	case s.Status == StatusTrialing:
		if s.TrialEndsAt != nil && !now.Before(*s.TrialEndsAt) {
			return decision{Renew: true}, StatusActive
		}
	case !now.Before(s.PeriodEnd) && s.CancelAtEnd:
		return decision{Status: StatusCancelled}, StatusCancelled
	case !now.Before(s.PeriodEnd) && s.Status != StatusSuspended:
		return decision{Renew: true}, s.Status
	}
	return decision{}, s.Status
}

// debtStatus aplica el vencimiento y la gracia de la factura pendiente.
func debtStatus(s state, status string, now time.Time) string {
	if s.OpenDue == nil || status == StatusTrialing {
		return status
	}
	switch {
	case now.After(s.OpenDue.AddDate(0, 0, s.GraceDays)):
		return StatusSuspended
	case now.After(*s.OpenDue) && status == StatusActive:
		return StatusPastDue
	}
	return status
}

// CycleStats resume una pasada del ciclo.
type CycleStats struct {
	Invoiced, PastDue, Suspended, Cancelled int
}

// HandleJob atiende billing_cycle (trabajo de plataforma).
func (s *Service) HandleJob(ctx context.Context, job jobs.Running) error {
	if job.Type != JobCycle {
		return fmt.Errorf("billing: tipo de trabajo desconocido %s", job.Type)
	}
	st, err := s.Cycle(ctx)
	if err == nil {
		s.log.InfoContext(ctx, "Ciclo de facturación", "invoiced", st.Invoiced, "past_due", st.PastDue,
			"suspended", st.Suspended, "cancelled", st.Cancelled)
	}
	return err
}

// Cycle recorre las suscripciones: factura, vence, suspende y cancela. Cada
// suscripcion va en su propia transaccion para que una falla no frene al resto.
func (s *Service) Cycle(ctx context.Context) (CycleStats, error) {
	var ids []uuid.UUID
	err := s.db.InTx(ctx, func(q database.Querier) error {
		rows, err := q.Query(ctx, `SELECT id FROM shared.subscriptions WHERE status <> 'cancelled' ORDER BY id`)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	if err != nil {
		return CycleStats{}, err
	}
	var st CycleStats
	var errs []error
	for _, id := range ids {
		if err := s.db.InTx(ctx, func(q database.Querier) error { return s.step(ctx, q, id, &st) }); err != nil {
			errs = append(errs, fmt.Errorf("suscripción %s: %w", id, err))
		}
	}
	return st, errors.Join(errs...)
}

func (s *Service) step(ctx context.Context, q database.Querier, id uuid.UUID, st *CycleStats) error {
	sub, err := subscriptionByID(ctx, q, id)
	if err != nil {
		return err
	}
	if _, err := q.Exec(ctx, `SELECT 1 FROM shared.subscriptions WHERE id = $1 FOR UPDATE`, id); err != nil {
		return err
	}
	in := state{
		Status: sub.Status, PeriodEnd: sub.CurrentPeriodEnd, TrialEndsAt: sub.TrialEndsAt,
		CancelAtEnd: sub.CancelAtPeriodEnd, GraceDays: sub.GraceDays,
	}
	if sub.OpenInvoice != nil {
		due := sub.OpenInvoice.DueAt
		in.OpenDue = &due
	}
	now := s.now()
	d := decide(in, now)
	if d.Renew {
		if err := s.renew(ctx, q, sub, now); err != nil {
			return err
		}
		st.Invoiced++
	}
	if d.Status == "" {
		return nil
	}
	return s.transition(ctx, q, sub, d.Status, st)
}

// renew abre el periodo siguiente y emite su factura. Si el periodo vencio
// hace mas de un ciclo (empresa reactivada tarde), arranca desde hoy.
func (s *Service) renew(ctx context.Context, q database.Querier, sub SubscriptionRead, now time.Time) error {
	start := sub.CurrentPeriodEnd
	if sub.Status == StatusTrialing && sub.TrialEndsAt != nil {
		start = *sub.TrialEndsAt
	}
	if addCycle(start, sub.Cycle).Before(now) {
		start = now
	}
	end := addCycle(start, sub.Cycle)
	if _, err := q.Exec(ctx, `
		UPDATE shared.subscriptions SET current_period_start = $2, current_period_end = $3, updated_at = now()
		WHERE id = $1`, sub.ID, start, end); err != nil {
		return err
	}
	sub.CurrentPeriodStart, sub.CurrentPeriodEnd = start, end
	invID, err := s.issueInvoice(ctx, q, sub, start, end)
	if err != nil {
		return err
	}
	inv, err := invoicesWhere(ctx, q, `i.id = $1`, invID)
	if err != nil || len(inv) == 0 {
		return err
	}
	return s.notify(ctx, q, sub.TenantID, "Tu factura de ARGOS está lista",
		"emitimos la factura "+inv[0].Number+" por "+money(inv[0].Currency, inv[0].Total)+".",
		[]string{inv[0].Description, "Vence el " + shortDate(inv[0].DueAt) + "."})
}

// transition cambia el estado, deja bitacora y avisa.
func (s *Service) transition(ctx context.Context, q database.Querier, sub SubscriptionRead, to string, st *CycleStats) error {
	if _, err := q.Exec(ctx, `
		UPDATE shared.subscriptions SET status = $2, status_changed_at = now(), updated_at = now() WHERE id = $1`,
		sub.ID, to); err != nil {
		return err
	}
	if err := audit.RecordPlatform(ctx, q, audit.PlatformEntry{
		TenantID: &sub.TenantID, Action: "billing.status_changed", Entity: "subscription", EntityID: &sub.ID,
		Metadata: map[string]any{"from": sub.Status, "to": to},
	}); err != nil {
		return err
	}
	switch to {
	case StatusPastDue:
		st.PastDue++
		lines := []string{}
		if sub.OpenInvoice != nil {
			lines = append(lines, "Factura "+sub.OpenInvoice.Number+" por "+money(sub.OpenInvoice.Currency, sub.OpenInvoice.Total)+".")
			lines = append(lines, "Si no se paga antes del "+shortDate(sub.OpenInvoice.DueAt.AddDate(0, 0, sub.GraceDays))+", la empresa queda en solo lectura.")
		}
		return s.notify(ctx, q, sub.TenantID, "Tienes un pago vencido", "la factura de ARGOS de "+sub.TenantName+" venció sin pago.", lines)
	case StatusSuspended:
		st.Suspended++
		return s.notify(ctx, q, sub.TenantID, "Tu empresa quedó en solo lectura",
			"como la factura sigue sin pagarse, "+sub.TenantName+" solo puede consultar datos en ARGOS.",
			[]string{"Nadie pierde información. Al pagar, todo vuelve a funcionar en el momento."})
	case StatusCancelled:
		st.Cancelled++
		return s.notify(ctx, q, sub.TenantID, "Tu suscripción terminó",
			"la suscripción de "+sub.TenantName+" terminó como pediste. La empresa queda en solo lectura.",
			[]string{"Puedes volver cuando quieras eligiendo un plan."})
	}
	return nil
}

// Webhook procesa un aviso de la pasarela (POST /public/billing/webhook/{provider}).
// Es idempotente por event_id: un aviso repetido no hace nada.
func (s *Service) Webhook(ctx context.Context, provider, signature string, body []byte) error {
	if provider != s.provider.Name() {
		return ErrProvider
	}
	e, err := s.provider.ParseWebhook(signature, body)
	if err != nil {
		return err
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		payload := json.RawMessage(body)
		tag, err := q.Exec(ctx, `
			INSERT INTO shared.payment_events (provider, event_id, type, payload) VALUES ($1, $2, $3, $4)
			ON CONFLICT (provider, event_id) DO NOTHING`, provider, e.ID, e.Type, payload)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		inv, err := invoicesWhere(ctx, q, `i.number = $1`, e.InvoiceNumber)
		if err != nil {
			return err
		}
		if len(inv) == 0 {
			_, err := q.Exec(ctx, `UPDATE shared.payment_events SET error = 'factura desconocida', processed_at = now()
				WHERE provider = $1 AND event_id = $2`, provider, e.ID)
			return err
		}
		amount := e.Amount
		if amount == 0 {
			amount = inv[0].Total
		}
		switch e.Type {
		case EventPaymentSucceeded:
			err = s.applyPayment(ctx, q, nil, inv[0], e.PaymentRef, amount, e.Method, e.Raw)
		case EventPaymentFailed:
			err = s.recordFailure(ctx, q, inv[0], e.PaymentRef, amount, e.Reason, e.Raw)
		}
		if err != nil {
			return err
		}
		_, err = q.Exec(ctx, `UPDATE shared.payment_events SET processed_at = now() WHERE provider = $1 AND event_id = $2`, provider, e.ID)
		return err
	})
}
