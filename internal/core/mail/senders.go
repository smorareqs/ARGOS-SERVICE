package mail

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// LogSender no envia nada: escribe el correo en el log y, si Dir no esta
// vacio, como archivo HTML para abrirlo en el navegador (local y testing).
type LogSender struct {
	Log *slog.Logger
	Dir string
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// Send implementa Sender.
func (s LogSender) Send(ctx context.Context, from string, m Message) error {
	attrs := []any{"to", m.To, "subject", m.Subject, "template", m.Template}
	if s.Dir != "" {
		if err := os.MkdirAll(s.Dir, 0o750); err != nil {
			return fmt.Errorf("creando %s: %w", s.Dir, err)
		}
		name := fmt.Sprintf("%s-%s-%s.html", time.Now().Format("20060102-150405"), m.Template,
			unsafeName.ReplaceAllString(m.To, "_"))
		path := filepath.Join(s.Dir, name)
		header := fmt.Sprintf("<!-- De: %s | Para: %s | Asunto: %s -->\n", from, m.To, m.Subject)
		if err := os.WriteFile(path, []byte(header+m.HTML), 0o600); err != nil {
			return fmt.Errorf("guardando el correo: %w", err)
		}
		attrs = append(attrs, "file", path)
	}
	s.Log.InfoContext(ctx, "Correo (no enviado: driver log)\n"+m.Text, attrs...)
	return nil
}

// SESSender envia por Amazon SES (API v2).
type SESSender struct {
	Client *sesv2.Client
}

// Send implementa Sender.
func (s SESSender) Send(ctx context.Context, from string, m Message) error {
	_, err := s.Client.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(from),
		Destination:      &types.Destination{ToAddresses: []string{m.To}},
		Content: &types.EmailContent{Simple: &types.Message{
			Subject: &types.Content{Data: aws.String(m.Subject), Charset: aws.String("UTF-8")},
			Body: &types.Body{
				Html: &types.Content{Data: aws.String(m.HTML), Charset: aws.String("UTF-8")},
				Text: &types.Content{Data: aws.String(m.Text), Charset: aws.String("UTF-8")},
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("SES: %w", err)
	}
	return nil
}

// Domain devuelve el dominio de un correo (para registrar sin exponer la
// direccion completa).
func Domain(email string) string {
	if at := strings.LastIndexByte(email, '@'); at >= 0 {
		return strings.ToLower(email[at+1:])
	}
	return ""
}
