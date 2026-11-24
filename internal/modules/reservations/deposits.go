package reservations

import (
	"context"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Deposito (fase04.md §3): se recibe antes del check-in; despues de la salida
// se devuelve (si la inspeccion de salida no tiene cargos) o se retiene en
// parte o completo (si los tiene). Los movimientos son inmutables y
// deposit_status se recalcula con ellos.

// Deposit devuelve el deposito de la reserva (deposits.read).
func (s *Service) Deposit(ctx context.Context, actor authz.Actor, id uuid.UUID) (DepositRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (DepositRead, error) {
		r, _, err := s.visible(ctx, q, actor, id)
		if err != nil {
			return DepositRead{}, err
		}
		return depositOf(ctx, q, r)
	})
}

// AddDepositTx registra un movimiento del deposito y avisa al huesped.
func (s *Service) AddDepositTx(ctx context.Context, actor authz.Actor, id uuid.UUID, in DepositTxCreate) (DepositRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (DepositRead, error) {
		if _, _, err := s.visible(ctx, q, actor, id); err != nil {
			return DepositRead{}, err
		}
		r, err := reservationByID(ctx, q, id, true)
		if err != nil {
			return DepositRead{}, err
		}
		d, err := depositOf(ctx, q, r)
		if err != nil {
			return DepositRead{}, err
		}
		if err := checkDepositTx(r, d, in); err != nil {
			return DepositRead{}, err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO deposit_transactions (reservation_id, type, amount, method, reference, notes, registered_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			id, in.Type, in.Amount, in.Method, trimmed(in.Reference), trimmed(in.Notes), actorID(actor)); err != nil {
			return DepositRead{}, err
		}
		if d, err = s.settleStatus(ctx, q, r); err != nil {
			return DepositRead{}, err
		}
		return d, s.afterDepositTx(ctx, q, actor, r, in, d)
	})
}

func (s *Service) afterDepositTx(ctx context.Context, q database.Querier, actor authz.Actor, r ReservationRead, in DepositTxCreate, d DepositRead) error {
	t, err := s.tenant(ctx, q)
	if err != nil {
		return err
	}
	sent, err := s.sendDeposit(ctx, q, t, r, in, d)
	if err != nil {
		return err
	}
	if err := addEvent(ctx, q, r.ID, "deposit_"+in.Type, actorID(actor), map[string]any{
		"amount": in.Amount, "method": in.Method, "status": d.Status, "email_sent": sent,
	}); err != nil {
		return err
	}
	return audit.Record(ctx, q, audit.Entry{
		Actor: actorID(actor), Action: "deposit." + in.Type, Entity: "reservation", EntityID: &r.ID, PropertyID: &r.PropertyID,
		After: map[string]any{"code": r.Code, "amount": in.Amount, "method": in.Method, "status": d.Status},
	})
}

// checkDepositTx aplica las reglas del movimiento.
func checkDepositTx(r ReservationRead, d DepositRead, in DepositTxCreate) error {
	if r.Status == "cancelled" || r.Status == "no_show" {
		if in.Type != "returned" {
			return ErrTransition
		}
	}
	switch in.Type {
	case "received":
		if r.DepositAmount == nil || *r.DepositAmount <= 0 {
			return ErrNoDeposit
		}
		if cents(d.Received+in.Amount) > cents(*r.DepositAmount) {
			return ErrDepositExceeds.WithField("body.amount")
		}
		return nil
	case "returned", "retained":
		if cents(in.Amount) > cents(d.Balance) {
			return ErrDepositExceeds.WithField("body.amount")
		}
		if r.Status == "cancelled" || r.Status == "no_show" {
			return nil // una cancelada devuelve sin inspeccion
		}
		return settleRules(d, in.Type)
	}
	return ErrTransition
}

// settleRules: devolver o retener necesita la inspeccion de salida; retener,
// un hallazgo con cargo; devolver con cargos, haber retenido antes.
func settleRules(d DepositRead, kind string) error {
	if d.InspectionRequired {
		return ErrNeedsInspection
	}
	if kind == "retained" && !d.chargeFindings {
		return ErrNoCharges
	}
	if kind == "returned" && d.chargeFindings && d.Retained == 0 {
		return ErrChargesFirst
	}
	return nil
}

func cents(v float64) int64 { return int64(math.Round(v * 100)) }

// settleStatus recalcula deposit_status con los movimientos.
func (s *Service) settleStatus(ctx context.Context, q database.Querier, r ReservationRead) (DepositRead, error) {
	d, err := depositOf(ctx, q, r)
	if err != nil {
		return d, err
	}
	status := depositState(r.DepositAmount, d)
	if _, err := q.Exec(ctx, `UPDATE reservations SET deposit_status = $2, updated_at = now() WHERE id = $1`, r.ID, status); err != nil {
		return d, err
	}
	d.Status = status
	return d, nil
}

func depositState(amount *float64, d DepositRead) string {
	switch {
	case d.Received == 0:
		return depositStatusFor(amount)
	case cents(d.Balance) > 0 && d.Returned == 0 && d.Retained == 0:
		return "received"
	case d.Retained > 0 && cents(d.Retained) >= cents(d.Received):
		return "retained_full"
	case d.Retained > 0:
		return "retained_partial"
	case cents(d.Balance) == 0:
		return "returned"
	default:
		return "received"
	}
}

// depositOf lee los movimientos y el estado de la inspeccion de salida.
func depositOf(ctx context.Context, q database.Querier, r ReservationRead) (DepositRead, error) {
	d := DepositRead{Available: true, Amount: r.DepositAmount, Status: r.DepositStatus}
	rows, err := q.Query(ctx, `
		SELECT t.id, t.type, t.amount::float8 AS amount, t.method, t.reference, t.notes,
		       u.full_name AS registered_by_name, t.created_at
		FROM deposit_transactions t LEFT JOIN shared.users u ON u.id = t.registered_by
		WHERE t.reservation_id = $1 ORDER BY t.created_at, t.id`, r.ID)
	if err != nil {
		return d, err
	}
	if d.Transactions, err = pgx.CollectRows(rows, pgx.RowToStructByName[DepositTx]); err != nil {
		return d, err
	}
	for _, t := range d.Transactions {
		switch t.Type {
		case "received":
			d.Received += t.Amount
		case "returned":
			d.Returned += t.Amount
		case "retained":
			d.Retained += t.Amount
		}
	}
	d.Balance = float64(cents(d.Received-d.Returned-d.Retained)) / 100
	var inspected bool
	err = q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM inspections WHERE reservation_id = $1 AND kind = 'post_departure'),
		       coalesce((SELECT sum(coalesce(f.estimated_cost, 0))::float8 FROM inspection_findings f
		                   JOIN inspections i ON i.id = f.inspection_id
		                  WHERE i.reservation_id = $1 AND i.kind = 'post_departure' AND f.charge_to_deposit), 0),
		       EXISTS (SELECT 1 FROM inspection_findings f JOIN inspections i ON i.id = f.inspection_id
		                WHERE i.reservation_id = $1 AND i.kind = 'post_departure' AND f.charge_to_deposit)`,
		r.ID).Scan(&inspected, &d.Charges, &d.chargeFindings)
	d.InspectionRequired = !inspected
	return d, err
}
