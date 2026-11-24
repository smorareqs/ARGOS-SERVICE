package reservations

import "testing"

func TestMailFormats(t *testing.T) {
	r := ReservationRead{PropertyName: "Torre Botánica 1102", CheckInDate: "2026-12-05", CheckInTime: "15:00"}
	got := instructions("Hola {{huesped}}: te esperamos en {{propiedad}} el {{fecha_entrada}} desde las {{hora_entrada}}.", "Lucía Pérez", r)
	want := "Hola Lucía: te esperamos en Torre Botánica 1102 el sábado 5 de diciembre desde las 15:00."
	if got != want {
		t.Errorf("instructions = %q", got)
	}
	for v, s := range map[float64]string{1200: "Q 1,200.00", 0.5: "Q 0.50", 1234567.891: "Q 1,234,567.89"} {
		if got := money("GTQ", v); got != s {
			t.Errorf("money(%v) = %q, quería %q", v, got, s)
		}
	}
	if got := money("USD", 80); got != "USD 80.00" {
		t.Errorf("money USD = %q", got)
	}
	if got := shortDate("2027-01-03"); got != "3 ene" {
		t.Errorf("shortDate = %q", got)
	}
}

func TestDepositState(t *testing.T) {
	amount := 1200.0
	cases := []struct {
		d    DepositRead
		want string
	}{
		{DepositRead{}, "pending"},
		{DepositRead{Received: 1200, Balance: 1200}, "received"},
		{DepositRead{Received: 1200, Returned: 1200}, "returned"},
		{DepositRead{Received: 1200, Retained: 300, Balance: 900}, "retained_partial"},
		{DepositRead{Received: 1200, Retained: 300, Returned: 900}, "retained_partial"},
		{DepositRead{Received: 1200, Retained: 1200}, "retained_full"},
	}
	for _, c := range cases {
		if got := depositState(&amount, c.d); got != c.want {
			t.Errorf("depositState(%+v) = %s, quería %s", c.d, got, c.want)
		}
	}
	if got := depositState(nil, DepositRead{}); got != "none" {
		t.Errorf("sin monto = %s", got)
	}
}
