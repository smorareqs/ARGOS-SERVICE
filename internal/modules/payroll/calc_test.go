package payroll

import (
	"testing"
	"time"
)

func d(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestBaseAmount(t *testing.T) {
	q1 := span{Kind: KindBiweekly, Start: d("2026-11-01"), End: d("2026-11-15")}
	month := span{Kind: KindMonthly, Start: d("2026-11-01"), End: d("2026-11-30")}
	full := worker{PayRate: 4000, Hire: d("2024-01-10")}
	cases := []struct {
		name string
		p    span
		w    worker
		days float64
		hrs  float64
		cl   int
		want float64
	}{
		{"fijo quincenal", q1, worker{PayType: PayFixedMonthly, PayRate: 4000, Hire: full.Hire}, 0, 0, 0, 2000},
		{"fijo mensual", month, worker{PayType: PayFixedMonthly, PayRate: 4000, Hire: full.Hire}, 0, 0, 0, 4000},
		{"fijo con ingreso a media quincena", q1, worker{PayType: PayFixedMonthly, PayRate: 4000, Hire: d("2026-11-11")}, 0, 0, 0, 666.67},
		{"fijo con baja a media quincena", q1, worker{PayType: PayFixedMonthly, PayRate: 4000, Hire: full.Hire, Termination: ptr(d("2026-11-05"))}, 0, 0, 0, 666.67},
		{"por día", q1, worker{PayType: PayPerDay, PayRate: 150}, 10.5, 0, 0, 1575},
		{"por hora", q1, worker{PayType: PayPerHour, PayRate: 32.5}, 0, 81.25, 0, 2640.63},
		{"por limpieza", q1, worker{PayType: PayPerCleaning, PayRate: 85}, 0, 0, 22, 1870},
	}
	for _, c := range cases {
		if got := baseAmount(c.p, c.w, c.days, c.hrs, c.cl); got != c.want {
			t.Errorf("%s: base = %v, quería %v", c.name, got, c.want)
		}
	}
}

func TestSettle(t *testing.T) {
	a := amounts{Base: 2000, Overtime: 200, Incentives: 125, OtherEarnings: 0, IncentiveBonus: 125, Bonus14: 166.67, OtherDeductions: 100}
	igss, gross, net := settle(a, true, 0.0483)
	if igss != 106.26 || gross != 2616.67 || net != 2410.41 {
		t.Errorf("en planilla: igss %v, devengado %v, liquido %v", igss, gross, net)
	}
	igss, _, net = settle(a, false, 0.0483)
	if igss != 0 || net != 2516.67 {
		t.Errorf("fuera de planilla: igss %v, liquido %v", igss, net)
	}
}

func TestIncentiveBonus(t *testing.T) {
	q := span{Kind: KindBiweekly, Start: d("2026-11-16"), End: d("2026-11-30")}
	if got := incentiveBonus(q, worker{OnPayroll: true, Hire: d("2020-01-01")}, 250); got != 125 {
		t.Errorf("quincena completa = %v", got)
	}
	if got := incentiveBonus(q, worker{OnPayroll: false, Hire: d("2020-01-01")}, 250); got != 0 {
		t.Errorf("fuera de planilla = %v", got)
	}
	if got := incentiveBonus(q, worker{OnPayroll: true, Hire: d("2026-11-24")}, 250); got != 58.33 {
		t.Errorf("ingreso a media quincena = %v", got)
	}
}

func TestBonusYear(t *testing.T) {
	cases := []struct {
		p     span
		month int
		year  int
		ok    bool
	}{
		{span{Start: d("2027-07-01"), End: d("2027-07-15")}, 7, 2027, true},
		{span{Start: d("2027-07-16"), End: d("2027-07-31")}, 7, 0, false},
		{span{Start: d("2027-06-16"), End: d("2027-06-30")}, 7, 0, false},
		{span{Start: d("2026-12-01"), End: d("2026-12-31")}, 12, 2026, true},
		{span{Start: d("2026-11-16"), End: d("2026-11-30")}, 12, 0, false},
	}
	for _, c := range cases {
		y, ok := bonusYear(c.p, c.month)
		if y != c.year || ok != c.ok {
			t.Errorf("bonusYear(%v, %d) = %d %v", c.p.Start.Format(time.DateOnly), c.month, y, ok)
		}
	}
	s, e := bonusWindow(2027, 7)
	if s.Format(time.DateOnly) != "2026-07-01" || e.Format(time.DateOnly) != "2027-06-30" {
		t.Errorf("ventana Bono 14 = %v a %v", s, e)
	}
	s, e = bonusWindow(2026, 12)
	if s.Format(time.DateOnly) != "2025-12-01" || e.Format(time.DateOnly) != "2026-11-30" {
		t.Errorf("ventana aguinaldo = %v a %v", s, e)
	}
}

func TestBonusAmount(t *testing.T) {
	// Un año completo sin historial: un salario.
	w := worker{PayType: PayFixedMonthly, PayRate: 4000, OnPayroll: true, Hire: d("2020-01-01")}
	if got := bonusAmount("aguinaldo", 2026, 12, w, nil, 4000); got.Amount != 4000 || got.MonthsEstimated != 12 || got.MonthsHistory != 0 {
		t.Errorf("año completo: %+v", got)
	}
	// Ingreso el 1 de junio: 183 de 365 días (salario × días / 365).
	w.Hire = d("2026-06-01")
	if got := bonusAmount("aguinaldo", 2026, 12, w, nil, 4000); got.Amount != 2005.48 || got.MonthsEstimated != 6 {
		t.Errorf("medio año: %+v", got)
	}
	// Ingreso el 16 de junio: 168 días.
	w.Hire = d("2026-06-16")
	if got := bonusAmount("aguinaldo", 2026, 12, w, nil, 4000); got.Amount != 1841.1 {
		t.Errorf("ingreso a mitad de mes: %+v", got)
	}
	// El historial da el salario diario para estimar los días sin nómina en ARGOS.
	w.Hire = d("2020-01-01")
	hist := []paidPeriod{{d("2026-10-01"), d("2026-10-31"), 4600}, {d("2026-11-01"), d("2026-11-30"), 4400}}
	got := bonusAmount("aguinaldo", 2026, 12, w, hist, 0)
	if got.MonthsHistory != 2 || got.MonthsEstimated != 10 || got.Amount != 4487.7 {
		t.Errorf("con historial: %+v", got)
	}
	// Fuera de planilla no hay bono.
	w.OnPayroll = false
	if got := bonusAmount("bonus14", 2027, 7, w, nil, 4000); got.Amount != 0 {
		t.Errorf("fuera de planilla: %+v", got)
	}
}

func TestMonthlyEquivalent(t *testing.T) {
	q := span{Kind: KindBiweekly, Start: d("2026-11-01"), End: d("2026-11-15")}
	if got := monthlyEquivalent(q, worker{PayType: PayFixedMonthly, PayRate: 4000}, 2000, 0, 30); got != 4000 {
		t.Errorf("fijo = %v", got)
	}
	if got := monthlyEquivalent(q, worker{PayType: PayPerCleaning, Hire: d("2020-01-01")}, 1500, 0, 30); got != 3000 {
		t.Errorf("por limpieza = %v", got)
	}
}

func ptr[T any](v T) *T { return &v }
