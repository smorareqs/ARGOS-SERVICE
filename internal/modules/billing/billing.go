// Package billing cobra a las empresas por usar ARGOS (fase06.md §3): planes
// con limites, suscripcion por empresa, facturas, pagos por pasarela
// (PaymentProvider, con FakeProvider en local y pruebas), el trabajo diario
// billing_cycle y el modo solo lectura de una empresa suspendida.
//
// Todo vive en el schema shared. Lo que cuenta uso (propiedades) corre con el
// search_path de la empresa que deja el Guard.
package billing

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
)

// JobCycle es el trabajo diario que factura, vence y suspende.
const JobCycle = "billing_cycle"

// Estados de la suscripcion.
const (
	StatusTrialing  = "trialing"
	StatusActive    = "active"
	StatusPastDue   = "past_due"
	StatusSuspended = "suspended"
	StatusCancelled = "cancelled"
)

// Ciclos de cobro.
const (
	CycleMonthly = "monthly"
	CycleYearly  = "yearly"
)

// DefaultPlan es el plan de una empresa nueva (en prueba).
const DefaultPlan = "completo"

// DefaultTrialDays son los dias de prueba de una empresa nueva.
const DefaultTrialDays = 14

// dueAfter es el plazo para pagar una factura desde que se emite.
const dueAfter = 5 * 24 * time.Hour

// taxRate es el IVA de Guatemala, incluido en el precio del plan.
const taxRate = 0.12

// ReadOnly indica si una empresa con este estado solo puede leer: toda
// escritura responde 402 salvo facturacion (fase06.md §3.2).
func ReadOnly(status string) bool {
	return status == StatusSuspended || status == StatusCancelled
}

// Errores del modulo.
var (
	ErrPlanNotFound         = apperr.New(apperr.KindNotFound, "El plan no existe.")
	ErrPlanKeyTaken         = apperr.New(apperr.KindAlreadyExists, "Ya hay un plan con esa clave.")
	ErrPlanKey              = apperr.New(apperr.KindValidation, "La clave va en minúsculas, números y guiones bajos (2 a 32).")
	ErrPlanInactive         = apperr.New(apperr.KindValidation, "Ese plan ya no se ofrece.")
	ErrSubscriptionNotFound = apperr.New(apperr.KindNotFound, "La suscripción no existe.")
	ErrInvoiceNotFound      = apperr.New(apperr.KindNotFound, "La factura no existe.")
	ErrNothingToPay         = apperr.New(apperr.KindValidation, "No hay facturas pendientes ni un cambio de plan que pagar.")
	ErrCycle                = apperr.New(apperr.KindValidation, "El ciclo es mensual (monthly) o anual (yearly).")
	ErrCancelled            = apperr.New(apperr.KindValidation, "La suscripción ya terminó: elige un plan para volver a activarla.")
	ErrTrialDays            = apperr.New(apperr.KindValidation, "La prueba dura de 1 a 90 días.")
	ErrUnknownFeature       = apperr.New(apperr.KindValidation, "Hay una funcionalidad que no existe.")
	ErrWebhookSignature     = apperr.New(apperr.KindAuthentication, "Firma del webhook inválida.")
	ErrWebhookPayload       = apperr.New(apperr.KindValidation, "El webhook no se pudo leer.")
	ErrProvider             = apperr.New(apperr.KindNotFound, "Pasarela desconocida.")
)

// Service es el servicio de facturacion.
type Service struct {
	db       *database.DB
	provider Provider
	log      *slog.Logger
	webBase  string
	now      func() time.Time
}

// NewService crea el servicio con la pasarela indicada.
func NewService(db *database.DB, provider Provider, log *slog.Logger, webBase string) *Service {
	return &Service{db: db, provider: provider, log: log, webBase: strings.TrimRight(webBase, "/"), now: time.Now}
}

// SetClock cambia el reloj (pruebas).
func (s *Service) SetClock(now func() time.Time) {
	s.now = now
}

// Provider devuelve la pasarela configurada.
func (s *Service) Provider() Provider {
	return s.provider
}

// --- utilidades ----------------------------------------------------------------

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// split separa un precio con IVA incluido en subtotal e impuesto.
func split(total float64) (subtotal, tax float64) {
	subtotal = round2(total / (1 + taxRate))
	return subtotal, round2(total - subtotal)
}

func addCycle(t time.Time, cycle string) time.Time {
	if cycle == CycleYearly {
		return t.AddDate(1, 0, 0)
	}
	return t.AddDate(0, 1, 0)
}

func validCycle(c string) bool {
	return c == CycleMonthly || c == CycleYearly
}

var monthsES = [...]string{"ene", "feb", "mar", "abr", "may", "jun", "jul", "ago", "sep", "oct", "nov", "dic"}

// shortDate da "5 dic 2026".
func shortDate(t time.Time) string {
	return fmt.Sprintf("%d %s %d", t.Day(), monthsES[t.Month()-1], t.Year())
}

// money da "Q 1,450.00" en quetzales y "USD 80.00" en otras monedas.
func money(currency string, v float64) string {
	prefix := strings.TrimSpace(currency) + " "
	if strings.TrimSpace(currency) == "GTQ" {
		prefix = "Q "
	}
	cents := int64(math.Round(v * 100))
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	whole := fmt.Sprint(cents / 100)
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return fmt.Sprintf("%s%s%s.%02d", sign, prefix, b.String(), cents%100)
}

func cycleLabel(c string) string {
	if c == CycleYearly {
		return "anual"
	}
	return "mensual"
}

// StatusLabel da el estado en palabras.
func StatusLabel(s string) string {
	switch s {
	case StatusTrialing:
		return "En prueba"
	case StatusActive:
		return "Al día"
	case StatusPastDue:
		return "Pago vencido"
	case StatusSuspended:
		return "Suspendida"
	case StatusCancelled:
		return "Cancelada"
	}
	return s
}

func notFound(err, nf error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nf
	}
	return err
}
