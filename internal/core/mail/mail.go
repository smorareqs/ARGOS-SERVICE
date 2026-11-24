// Package mail envia correos transaccionales (invitaciones, recuperacion de
// contrasena, avisos de seguridad) a partir de plantillas embebidas.
//
// Los servicios no envian directamente: encolan un trabajo `send_email`
// (internal/core/jobs) en la misma transaccion que el cambio que lo origina, y
// el worker llama a Service.Send. En local y testing el Sender escribe el
// correo al log y a un archivo HTML; en despliegue usa Amazon SES.
package mail

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"io/fs"
	"strings"
	texttemplate "text/template"
)

//go:embed templates/*
var templatesFS embed.FS

// Plantillas disponibles. Cada una tiene templates/<nombre>.html (cuerpo HTML,
// dentro de layout.html) y templates/<nombre>.txt (asunto y texto plano).
const (
	TemplateInvitation      = "invitation"
	TemplatePasswordReset   = "password_reset"
	TemplatePasswordChanged = "password_changed"
	// Fase 4 (reservas): correos al huesped.
	TemplateReservationConfirmed = "reservation_confirmed"
	TemplateReservationReminder  = "reservation_reminder"
	TemplateReservationCancelled = "reservation_cancelled"
	TemplateDepositUpdate        = "deposit_update"
	// Fase 5 (RRHH): boleta lista y accion disciplinaria por leer.
	TemplatePayslipAvailable   = "payslip_available"
	TemplateDisciplinaryNotice = "disciplinary_notice"
	// Fase 6 (Persona A).
	TemplateBillingNotice        = "billing_notice"
	TemplateReservationRequested = "reservation_requested"
)

// ErrUnknownTemplate indica una plantilla que no existe.
var ErrUnknownTemplate = errors.New("plantilla de correo desconocida")

// Message es un correo listo para enviar.
type Message struct {
	To      string
	Subject string
	HTML    string
	Text    string
	// Template es el nombre de la plantilla de origen (solo para registro).
	Template string
}

// Sender entrega un mensaje.
type Sender interface {
	Send(ctx context.Context, from string, m Message) error
}

// Service renderiza plantillas y entrega los correos.
type Service struct {
	sender Sender
	from   string
	html   map[string]*htmltemplate.Template
	text   map[string]*texttemplate.Template
}

// NewService carga las plantillas embebidas.
func NewService(sender Sender, from string) (*Service, error) {
	s := &Service{sender: sender, from: from, html: map[string]*htmltemplate.Template{}, text: map[string]*texttemplate.Template{}}
	names, err := fs.Glob(templatesFS, "templates/*.txt")
	if err != nil {
		return nil, err
	}
	for _, path := range names {
		name := strings.TrimSuffix(strings.TrimPrefix(path, "templates/"), ".txt")
		txt, err := texttemplate.ParseFS(templatesFS, path)
		if err != nil {
			return nil, fmt.Errorf("plantilla %s: %w", path, err)
		}
		html, err := htmltemplate.New(name).Funcs(htmltemplate.FuncMap{"dict": dict}).
			ParseFS(templatesFS, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("plantilla %s.html: %w", name, err)
		}
		s.text[name], s.html[name] = txt, html
	}
	return s, nil
}

// Render arma el mensaje de la plantilla con data.
func (s *Service) Render(to, template string, data any) (Message, error) {
	txt, ok := s.text[template]
	if !ok {
		return Message{}, fmt.Errorf("%w: %s", ErrUnknownTemplate, template)
	}
	var subject, text, html bytes.Buffer
	if err := txt.ExecuteTemplate(&subject, "subject", data); err != nil {
		return Message{}, err
	}
	if err := txt.ExecuteTemplate(&text, "text", data); err != nil {
		return Message{}, err
	}
	if err := s.html[template].ExecuteTemplate(&html, "layout", data); err != nil {
		return Message{}, err
	}
	return Message{
		To: to, Subject: strings.TrimSpace(subject.String()), Text: strings.TrimSpace(text.String()) + "\n",
		HTML: html.String(), Template: template,
	}, nil
}

// dict arma un mapa a partir de pares clave-valor (para pasar datos a
// subplantillas como "button").
func dict(pairs ...any) (map[string]any, error) {
	if len(pairs)%2 != 0 {
		return nil, errors.New("dict espera pares clave-valor")
	}
	out := make(map[string]any, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			return nil, errors.New("las claves de dict son texto")
		}
		out[key] = pairs[i+1]
	}
	return out, nil
}

// Send renderiza y entrega el correo.
func (s *Service) Send(ctx context.Context, to, template string, data any) error {
	m, err := s.Render(to, template, data)
	if err != nil {
		return err
	}
	return s.sender.Send(ctx, s.from, m)
}
