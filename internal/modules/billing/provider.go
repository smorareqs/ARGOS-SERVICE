package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

// Provider es la pasarela de pago (fase06.md §3.2). La implementacion real
// depende de la decision pendiente en avance.md; FakeProvider cubre local y
// pruebas con el mismo contrato.
type Provider interface {
	// Name es la clave de la pasarela (subscriptions.provider y la ruta del
	// webhook /public/billing/webhook/{name}).
	Name() string
	// CreateCustomer registra a la empresa en la pasarela.
	CreateCustomer(ctx context.Context, c Customer) (string, error)
	// Checkout cobra una factura: o la cobra al momento (succeeded/failed) o
	// devuelve la URL de la pagina de pago (redirect) y avisa por webhook.
	Checkout(ctx context.Context, c Charge) (ChargeResult, error)
	// Cancel deja de cobrar la suscripcion en la pasarela.
	Cancel(ctx context.Context, customerRef string) error
	// ParseWebhook verifica la firma y traduce el aviso de la pasarela.
	ParseWebhook(signature string, body []byte) (Event, error)
}

// Customer son los datos de la empresa para la pasarela.
type Customer struct {
	TenantID uuid.UUID
	Name     string
	Email    string
}

// Charge es una factura por cobrar.
type Charge struct {
	InvoiceID   uuid.UUID
	Number      string
	Description string
	Amount      float64
	Currency    string
	CustomerRef string
	ReturnURL   string
}

// Resultados de un cobro.
const (
	ChargeSucceeded = "succeeded"
	ChargeFailed    = "failed"
	ChargeRedirect  = "redirect"
)

// ChargeResult es lo que devuelve la pasarela al cobrar.
type ChargeResult struct {
	Status      string
	PaymentRef  string
	Method      string
	RedirectURL string
	Reason      string
}

// Tipos de evento de webhook.
const (
	EventPaymentSucceeded = "payment.succeeded"
	EventPaymentFailed    = "payment.failed"
)

// Event es un aviso de la pasarela ya verificado.
type Event struct {
	ID            string         `json:"id"`
	Type          string         `json:"type"`
	InvoiceNumber string         `json:"invoice_number"`
	PaymentRef    string         `json:"payment_id"`
	Amount        float64        `json:"amount"`
	Method        string         `json:"method"`
	Reason        string         `json:"reason"`
	Raw           map[string]any `json:"-"`
}

// FakeProvider simula la pasarela: cobra al momento con una tarjeta de prueba
// y firma sus webhooks con HMAC-SHA256 (cabecera X-Argos-Signature:
// sha256=<hex>). Decline hace que los cobros fallen (pruebas).
type FakeProvider struct {
	Secret  []byte
	Decline bool
}

// NewFakeProvider crea la pasarela simulada con el secreto de los webhooks.
func NewFakeProvider(secret string) *FakeProvider {
	return &FakeProvider{Secret: []byte(secret)}
}

// Name implementa Provider.
func (f *FakeProvider) Name() string { return "fake" }

// CreateCustomer implementa Provider.
func (f *FakeProvider) CreateCustomer(_ context.Context, c Customer) (string, error) {
	return "fake_cus_" + strings.ReplaceAll(c.TenantID.String(), "-", "")[:12], nil
}

// Checkout implementa Provider: cobra al instante.
func (f *FakeProvider) Checkout(_ context.Context, _ Charge) (ChargeResult, error) {
	if f.Decline {
		return ChargeResult{Status: ChargeFailed, Reason: "La tarjeta de prueba fue rechazada.", Method: "Tarjeta de prueba terminada en 0002"}, nil
	}
	return ChargeResult{
		Status: ChargeSucceeded, PaymentRef: "fake_pay_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16],
		Method: "Tarjeta de prueba terminada en 4242",
	}, nil
}

// Cancel implementa Provider.
func (f *FakeProvider) Cancel(context.Context, string) error { return nil }

// Sign firma un cuerpo como lo haria la pasarela (pruebas y seed).
func (f *FakeProvider) Sign(body []byte) string {
	mac := hmac.New(sha256.New, f.Secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// ParseWebhook implementa Provider.
func (f *FakeProvider) ParseWebhook(signature string, body []byte) (Event, error) {
	if len(f.Secret) == 0 || !hmac.Equal([]byte(signature), []byte(f.Sign(body))) {
		return Event{}, ErrWebhookSignature
	}
	var e Event
	if err := json.Unmarshal(body, &e); err != nil || e.ID == "" || e.Type == "" {
		return Event{}, ErrWebhookPayload
	}
	_ = json.Unmarshal(body, &e.Raw)
	return e, nil
}
