package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/security"
)

func newTokenService(t *testing.T) (*Service, *security.TokenManager) {
	t.Helper()
	tokens, err := security.NewTokenManager("test-secret", "HS256", time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return NewService(nil, tokens, nil), tokens
}

func TestIssueAndParseTokens(t *testing.T) {
	svc, _ := newTokenService(t)
	tenantID := uuid.New()
	user := User{ID: uuid.New(), Role: RoleAdmin, TenantID: &tenantID}

	token, err := svc.IssueTokens(user)
	if err != nil {
		t.Fatal(err)
	}
	if token.TokenType != "bearer" {
		t.Errorf("token_type = %q", token.TokenType)
	}

	access, err := svc.ParseToken(token.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if access.Sub != user.ID || access.Role != RoleAdmin || access.Type != security.TokenAccess ||
		access.TenantID == nil || *access.TenantID != tenantID {
		t.Errorf("payload de acceso inesperado: %+v", access)
	}

	refresh, err := svc.ParseToken(token.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if refresh.Type != security.TokenRefresh {
		t.Errorf("tipo del refresh = %q", refresh.Type)
	}
}

func TestParseTokenWithoutTenant(t *testing.T) {
	svc, _ := newTokenService(t)
	token, err := svc.IssueTokens(User{ID: uuid.New(), Role: RolePlatformAdmin})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := svc.ParseToken(token.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if payload.TenantID != nil {
		t.Errorf("tenant_id deberia ser nil: %v", payload.TenantID)
	}
}

func TestParseTokenRejectsBadClaims(t *testing.T) {
	svc, tokens := newTokenService(t)
	cases := map[string]struct {
		sub    string
		claims map[string]any
	}{
		"sub no uuid":       {"user-123", map[string]any{"role": "admin"}},
		"rol desconocido":   {uuid.NewString(), map[string]any{"role": "root"}},
		"sin rol":           {uuid.NewString(), nil},
		"tenant_id no uuid": {uuid.NewString(), map[string]any{"role": "admin", "tenant_id": "x"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			token, err := tokens.CreateAccessToken(c.sub, c.claims)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.ParseToken(token); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("se esperaba ErrInvalidToken, se obtuvo %v", err)
			}
		})
	}
}
