package mail

import (
	"context"
	"strings"
	"testing"
)

type captureSender struct{ last Message }

func (c *captureSender) Send(_ context.Context, _ string, m Message) error {
	c.last = m
	return nil
}

func TestRenderTemplates(t *testing.T) {
	sender := &captureSender{}
	svc, err := NewService(sender, "ARGOS <no-reply@argos.gt>")
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{
		"InviterName": "Ana López", "TenantName": "Garrent", "RoleName": "Supervisor",
		"Link": "http://localhost:4200/invitacion?token=abc", "ExpiresOn": "28 de noviembre",
		"Message": "<script>alert(1)</script>",
	}
	if err := svc.Send(context.Background(), "jorge@garrent.gt", TemplateInvitation, data); err != nil {
		t.Fatal(err)
	}
	m := sender.last
	if m.Subject != "Ana López te invitó a Garrent en ARGOS" {
		t.Errorf("asunto: %q", m.Subject)
	}
	if !strings.Contains(m.HTML, `href="http://localhost:4200/invitacion?token=abc"`) {
		t.Error("el HTML debe llevar el enlace")
	}
	if strings.Contains(m.HTML, "<script>") {
		t.Error("el mensaje del invitador debe ir escapado en el HTML")
	}
	if !strings.Contains(m.Text, "http://localhost:4200/invitacion?token=abc") {
		t.Error("el texto plano debe llevar el enlace")
	}

	for _, name := range []string{TemplatePasswordReset, TemplatePasswordChanged} {
		if _, err := svc.Render("x@y.gt", name, map[string]any{"Name": "Ana", "Link": "http://l", "Minutes": 60, "When": "hoy"}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := svc.Render("x@y.gt", "no_existe", nil); err == nil {
		t.Error("una plantilla desconocida debe fallar")
	}
}

func TestReservationTemplates(t *testing.T) {
	svc, err := NewService(&captureSender{}, "ARGOS <no-reply@argos.gt>")
	if err != nil {
		t.Fatal(err)
	}
	stay := func() map[string]any {
		return map[string]any{
			"TenantName": "Garrent", "GuestName": "Lucía", "Code": "ARG-7K3P9Q", "PropertyName": "Torre Botánica 1102",
			"Place": "Torre Botánica 1102 · Zona 10", "CheckIn": "sábado 5 de diciembre", "CheckOut": "martes 8 de diciembre",
			"CheckInTime": "15:00", "CheckOutTime": "11:00", "Nights": 3, "Instructions": "Pide la llave en recepción.",
			"AppCode": "482913", "AppLink": "argos://guest?token=x",
		}
	}
	for _, name := range []string{TemplateReservationConfirmed, TemplateReservationReminder, TemplateReservationCancelled} {
		m, err := svc.Render("lucia@correo.com", name, stay())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(m.Subject, "ARG-7K3P9Q") && !strings.Contains(m.Subject, "Torre Botánica 1102") {
			t.Errorf("%s: asunto %q", name, m.Subject)
		}
		if !strings.Contains(m.HTML, "ARG-7K3P9Q") || !strings.Contains(m.Text, "ARG-7K3P9Q") {
			t.Errorf("%s: falta el código de la reserva", name)
		}
	}
	confirmed, _ := svc.Render("lucia@correo.com", TemplateReservationConfirmed, stay())
	if !strings.Contains(confirmed.HTML, "482913") || !strings.Contains(confirmed.HTML, "Pide la llave") {
		t.Error("la confirmación debe llevar el código de la app y las instrucciones")
	}
	for kind, subject := range map[string]string{
		"received": "Recibimos tu depósito · ARG-7K3P9Q", "returned": "Te devolvimos tu depósito · ARG-7K3P9Q",
		"retained": "Movimiento en tu depósito · ARG-7K3P9Q",
	} {
		data := stay()
		data["Kind"], data["Amount"], data["Balance"], data["HasBalance"] = kind, "Q 1,200.00", "Q 0.00", false
		m, err := svc.Render("lucia@correo.com", TemplateDepositUpdate, data)
		if err != nil {
			t.Fatalf("depósito %s: %v", kind, err)
		}
		if m.Subject != subject || !strings.Contains(m.HTML, "Q 1,200.00") {
			t.Errorf("depósito %s: asunto %q", kind, m.Subject)
		}
	}
}
