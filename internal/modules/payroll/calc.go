package payroll

import (
	"math"
	"time"
)

// Reglas de calculo de fase05.md §3, como funciones puras para probarlas sin
// base de datos. Las fechas son dias (medianoche UTC) y los montos se
// redondean a centavos en cada paso, como en una planilla.

// Esquemas de pago (employees.pay_type).
const (
	PayFixedMonthly = "fixed_monthly"
	PayPerDay       = "per_day"
	PayPerHour      = "per_hour"
	PayPerCleaning  = "per_cleaning"
)

// Tipos de periodo.
const (
	KindBiweekly = "biweekly"
	KindMonthly  = "monthly"
)

// span es un periodo de nomina.
type span struct {
	Kind       string
	Start, End time.Time
}

// worker es lo del colaborador que entra al calculo.
type worker struct {
	PayType     string
	PayRate     float64
	OnPayroll   bool
	Hire        time.Time
	Termination *time.Time
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func day(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// inclusiveDays cuenta los dias de a a b, ambos incluidos (0 si b < a).
func inclusiveDays(a, b time.Time) int {
	if b.Before(a) {
		return 0
	}
	return int(day(b).Sub(day(a)).Hours()/24) + 1
}

// employedDays son los dias del rango [a, b] en que la persona estaba contratada.
func employedDays(a, b time.Time, w worker) int {
	if w.Hire.After(a) {
		a = w.Hire
	}
	if w.Termination != nil && w.Termination.Before(b) {
		b = *w.Termination
	}
	return inclusiveDays(a, b)
}

// factor es la parte del periodo que estuvo contratada (1 = completo).
func (p span) factor(w worker) float64 {
	total := inclusiveDays(p.Start, p.End)
	if total == 0 {
		return 0
	}
	return float64(employedDays(p.Start, p.End, w)) / float64(total)
}

// share es la parte de un monto mensual que toca en el periodo.
func (p span) share(monthly float64, w worker) float64 {
	if p.Kind == KindBiweekly {
		monthly /= 2
	}
	return round2(monthly * p.factor(w))
}

// baseAmount es el salario ordinario del periodo segun el esquema.
func baseAmount(p span, w worker, days, hours float64, cleanings int) float64 {
	switch w.PayType {
	case PayFixedMonthly:
		return p.share(w.PayRate, w)
	case PayPerDay:
		return round2(days * w.PayRate)
	case PayPerHour:
		return round2(hours * w.PayRate)
	case PayPerCleaning:
		return round2(float64(cleanings) * w.PayRate)
	}
	return 0
}

// incentiveBonus es la bonificacion incentivo del periodo (solo en planilla).
func incentiveBonus(p span, w worker, monthly float64) float64 {
	if !w.OnPayroll {
		return 0
	}
	return p.share(monthly, w)
}

// amounts son los montos de una linea.
type amounts struct {
	Base, Overtime, Incentives, OtherEarnings, IncentiveBonus, Bonus14, Aguinaldo, OtherDeductions float64
}

// settle calcula IGSS, total devengado y liquido. El IGSS se cobra sobre base,
// extras y otros devengos (no sobre Bono 14, aguinaldo, incentivos ni la
// bonificacion incentivo) y solo a quien esta en planilla.
func settle(a amounts, onPayroll bool, rate float64) (igss, gross, net float64) {
	if onPayroll {
		igss = round2((a.Base + a.Overtime + a.OtherEarnings) * rate)
	}
	gross = round2(a.Base + a.Overtime + a.Incentives + a.OtherEarnings + a.IncentiveBonus + a.Bonus14 + a.Aguinaldo)
	net = round2(gross - igss - a.OtherDeductions)
	return igss, gross, net
}

// bonusYear dice si el periodo paga el bono del mes indicado: cuando se cruza
// con los primeros 15 dias de ese mes (plazo legal del Bono 14 y del
// aguinaldo). Devuelve el año del pago.
func bonusYear(p span, month int) (int, bool) {
	for _, y := range []int{p.Start.Year(), p.End.Year()} {
		from := time.Date(y, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
		to := from.AddDate(0, 0, 14)
		if !p.End.Before(from) && !p.Start.After(to) {
			return y, true
		}
	}
	return 0, false
}

// bonusWindow son los doce meses que cuenta el bono pagado en year/month:
// del primer dia de ese mes del año anterior al dia previo de ese mes.
func bonusWindow(year, month int) (time.Time, time.Time) {
	end := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	return end.AddDate(-1, 0, 0), end.AddDate(0, 0, -1)
}

// paidPeriod es lo devengado (base + extras) en un periodo cerrado.
type paidPeriod struct {
	Start, End time.Time
	Earnings   float64
}

// daysPerMonth convierte dias a meses para mostrarlos (365 / 12).
const daysPerMonth = 365.0 / 12

// bonusAmount calcula el Bono 14 o el aguinaldo como dice la ley: salario
// diario promedio × dias trabajados en la ventana ÷ 365 × 12 (un salario
// promedio por año completo, proporcional si trabajo menos). Lo devengado en
// periodos cerrados en ARGOS cuenta tal cual; los dias contratados sin nomina
// en ARGOS se estiman con el salario diario de ese historial o, si no hay, con
// el salario mensual actual.
func bonusAmount(kind string, year, month int, w worker, history []paidPeriod, monthly float64) BonusDetail {
	start, end := bonusWindow(year, month)
	d := BonusDetail{Kind: kind, WindowStart: start.Format(time.DateOnly), WindowEnd: end.Format(time.DateOnly)}
	if !w.OnPayroll {
		return d
	}
	covered, total := 0, 0.0
	for _, h := range history {
		days := employedDays(maxTime(h.Start, start), minTime(h.End, end), w)
		if days <= 0 {
			continue
		}
		covered += days
		total += h.Earnings
	}
	daily := monthly * 12 / 365
	if covered > 0 {
		daily = total / float64(covered)
	}
	missing := max(0, employedDays(start, end, w)-covered)
	total += daily * float64(missing)
	d.MonthsHistory = int(math.Round(float64(covered) / daysPerMonth))
	d.MonthsEstimated = int(math.Round(float64(missing) / daysPerMonth))
	d.Earnings = round2(total)
	d.Amount = round2(total / 12)
	return d
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// monthlyEquivalent es el salario ordinario de un mes completo para estimar
// meses sin historial: la tarifa en salario fijo; en los demas esquemas, lo
// devengado en el periodo llevado a un mes.
func monthlyEquivalent(p span, w worker, base, overtime float64, monthDays int) float64 {
	if w.PayType == PayFixedMonthly {
		return w.PayRate
	}
	worked := employedDays(p.Start, p.End, w)
	if worked == 0 {
		return 0
	}
	return (base + overtime) * float64(monthDays) / float64(worked)
}
