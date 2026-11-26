package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrInvalidToken indica que el token del dispositivo ya no sirve (se borra).
var ErrInvalidToken = errors.New("token de dispositivo invalido")

// PushMessage es un push para un dispositivo.
type PushMessage struct {
	Token string
	Title string
	Body  string
	// Data viaja a la app (app_route para abrir la pantalla).
	Data map[string]string
}

// PushSender manda un push.
type PushSender interface {
	Send(ctx context.Context, m PushMessage) error
}

// LogPush solo registra el push (local y pruebas).
type LogPush struct{ Log *slog.Logger }

// Send registra el push en el log.
func (l LogPush) Send(ctx context.Context, m PushMessage) error {
	if l.Log != nil {
		l.Log.InfoContext(ctx, "Push (solo log)", "title", m.Title, "body", m.Body, "route", m.Data["app_route"])
	}
	return nil
}

// FCM manda push con Firebase Cloud Messaging HTTP v1, autenticado con la
// cuenta de servicio del proyecto (JWT firmado → token OAuth de 1 hora).
type FCM struct {
	projectID   string
	clientEmail string
	tokenURI    string
	key         any
	http        *http.Client

	mu      sync.Mutex
	access  string
	expires time.Time
}

type serviceAccount struct {
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// NewFCM lee la cuenta de servicio: una ruta a su JSON o el JSON mismo.
func NewFCM(credentials string) (*FCM, error) {
	raw := []byte(credentials)
	if !strings.HasPrefix(strings.TrimSpace(credentials), "{") {
		b, err := os.ReadFile(credentials)
		if err != nil {
			return nil, fmt.Errorf("leyendo ARGOS_FCM_CREDENTIALS: %w", err)
		}
		raw = b
	}
	var sa serviceAccount
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, fmt.Errorf("ARGOS_FCM_CREDENTIALS no es una cuenta de servicio: %w", err)
	}
	if sa.ProjectID == "" || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, errors.New("ARGOS_FCM_CREDENTIALS: faltan project_id, client_email o private_key")
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(sa.PrivateKey))
	if err != nil {
		return nil, fmt.Errorf("ARGOS_FCM_CREDENTIALS: llave privada invalida: %w", err)
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return &FCM{
		projectID: sa.ProjectID, clientEmail: sa.ClientEmail, tokenURI: sa.TokenURI, key: key,
		http: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (f *FCM) token(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.access != "" && time.Until(f.expires) > time.Minute {
		return f.access, nil
	}
	now := time.Now()
	assertion, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":   f.clientEmail,
		"scope": "https://www.googleapis.com/auth/firebase.messaging",
		"aud":   f.tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}).SignedString(f.key)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := f.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return "", fmt.Errorf("token de FCM: %s %s", res.Status, b)
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", err
	}
	f.access, f.expires = out.AccessToken, now.Add(time.Duration(out.ExpiresIn)*time.Second)
	return f.access, nil
}

// Send manda el push. Un token no registrado devuelve ErrInvalidToken.
func (f *FCM) Send(ctx context.Context, m PushMessage) error {
	access, err := f.token(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"token":        m.Token,
			"notification": map[string]string{"title": m.Title, "body": m.Body},
			"data":         m.Data,
			"android": map[string]any{
				"priority":     "high",
				"notification": map[string]string{"channel_id": "argos_default", "color": "#003841"},
			},
			"apns": map[string]any{"payload": map[string]any{"aps": map[string]any{"sound": "default"}}},
		},
	})
	if err != nil {
		return err
	}
	endpoint := "https://fcm.googleapis.com/v1/projects/" + f.projectID + "/messages:send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	res, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
	if res.StatusCode == http.StatusNotFound || bytes.Contains(b, []byte("UNREGISTERED")) ||
		(res.StatusCode == http.StatusBadRequest && bytes.Contains(b, []byte("registration token"))) {
		return ErrInvalidToken
	}
	return fmt.Errorf("FCM: %s %s", res.Status, b)
}
