package billing

import (
	"context"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func ptr[T any](v T) *T { return &v }

func TestDecide(t *testing.T) {
	now := at("2026-12-10T12:00:00Z")
	cases := []struct {
		name string
		in   state
		want decision
	}{
		{"periodo vigente sin deuda", state{Status: StatusActive, PeriodEnd: at("2026-12-20T00:00:00Z")}, decision{}},
		{"periodo vencido se renueva", state{Status: StatusActive, PeriodEnd: at("2026-12-01T00:00:00Z")}, decision{Renew: true}},
		{"cancelación programada", state{Status: StatusActive, PeriodEnd: at("2026-12-01T00:00:00Z"), CancelAtEnd: true}, decision{Status: StatusCancelled}},
		{"prueba vigente", state{Status: StatusTrialing, PeriodEnd: at("2026-12-20T00:00:00Z"), TrialEndsAt: ptr(at("2026-12-20T00:00:00Z"))}, decision{}},
		{"prueba terminada factura", state{Status: StatusTrialing, PeriodEnd: at("2026-12-05T00:00:00Z"), TrialEndsAt: ptr(at("2026-12-05T00:00:00Z"))}, decision{Renew: true, Status: StatusActive}},
		{"factura vencida", state{Status: StatusActive, PeriodEnd: at("2026-12-30T00:00:00Z"), GraceDays: 7, OpenDue: ptr(at("2026-12-08T00:00:00Z"))}, decision{Status: StatusPastDue}},
		{"gracia agotada suspende", state{Status: StatusPastDue, PeriodEnd: at("2026-12-30T00:00:00Z"), GraceDays: 7, OpenDue: ptr(at("2026-12-01T00:00:00Z"))}, decision{Status: StatusSuspended}},
		{"suspendida no renueva", state{Status: StatusSuspended, PeriodEnd: at("2026-12-01T00:00:00Z"), GraceDays: 7, OpenDue: ptr(at("2026-11-01T00:00:00Z"))}, decision{}},
		{"factura por vencer no cambia", state{Status: StatusActive, PeriodEnd: at("2026-12-30T00:00:00Z"), GraceDays: 7, OpenDue: ptr(at("2026-12-12T00:00:00Z"))}, decision{}},
		{"cancelada no hace nada", state{Status: StatusCancelled, PeriodEnd: at("2026-11-01T00:00:00Z")}, decision{}},
	}
	for _, c := range cases {
		if got := decide(c.in, now); got != c.want {
			t.Errorf("%s: %+v, quería %+v", c.name, got, c.want)
		}
	}
}

func TestSplitAndMoney(t *testing.T) {
	sub, tax := split(1450)
	if sub != 1294.64 || tax != 155.36 {
		t.Errorf("split(1450) = %v + %v", sub, tax)
	}
	if got := money("GTQ", 14500); got != "Q 14,500.00" {
		t.Errorf("money: %q", got)
	}
	if got := addCycle(at("2026-01-31T00:00:00Z"), CycleYearly); !got.Equal(at("2027-01-31T00:00:00Z")) {
		t.Errorf("addCycle anual: %v", got)
	}
}

func TestFakeWebhook(t *testing.T) {
	f := NewFakeProvider("secreto")
	body := []byte(`{"id":"evt_1","type":"payment.succeeded","invoice_number":"ARG-2026-000001","payment_id":"p1","amount":690}`)
	e, err := f.ParseWebhook(f.Sign(body), body)
	if err != nil || e.InvoiceNumber != "ARG-2026-000001" || e.Amount != 690 {
		t.Fatalf("webhook válido: %+v %v", e, err)
	}
	if _, err := f.ParseWebhook("sha256=00", body); err == nil {
		t.Error("una firma inválida debe fallar")
	}
	f.Decline = true
	if r, _ := f.Checkout(context.Background(), Charge{}); r.Status != ChargeFailed {
		t.Errorf("Decline: %+v", r)
	}
}
