package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/modules/authz"
)

func TestAttemptLimiterBlocksAfterTenFailures(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	l := newAttemptLimiter(func() time.Time { return now })
	key := "ana@garrent.gt|10.0.0.1"

	for range maxFailedAttempts - 1 {
		l.fail(key)
	}
	if wait := l.blockedFor(key); wait != 0 {
		t.Fatalf("con 9 fallos no deberia bloquear (espera %v)", wait)
	}
	l.fail(key)
	now = now.Add(5 * time.Minute)
	if wait := l.blockedFor(key); wait != 10*time.Minute {
		t.Fatalf("espera = %v, se esperaban 10m", wait)
	}
	if wait := l.blockedFor("ana@garrent.gt|10.0.0.2"); wait != 0 {
		t.Errorf("otra IP no deberia estar bloqueada")
	}
	now = now.Add(10 * time.Minute)
	if wait := l.blockedFor(key); wait != 0 {
		t.Errorf("al terminar la ventana deberia desbloquear (espera %v)", wait)
	}
}

func TestAttemptLimiterResetClearsFailures(t *testing.T) {
	l := newAttemptLimiter(time.Now)
	for range maxFailedAttempts {
		l.fail("k")
	}
	l.reset("k")
	if wait := l.blockedFor("k"); wait != 0 {
		t.Errorf("tras un login correcto no deberia quedar bloqueo")
	}
}

func TestOpaqueTokensAreRandomAndHashed(t *testing.T) {
	a, err := newOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newOpaqueToken()
	if a == b || len(a) < 40 {
		t.Fatalf("tokens inesperados: %q %q", a, b)
	}
	if h := hashToken(a); len(h) != 64 || h == hashToken(b) || h != hashToken(a) {
		t.Errorf("hash inesperado: %q", h)
	}
}

func TestPrincipalCan(t *testing.T) {
	p := Principal{Permissions: map[authz.Permission]bool{authz.UsersRead: true, authz.TasksRead: true}}
	if !p.Can(authz.UsersRead, authz.TasksRead) || p.Can(authz.UsersRead, authz.UsersManage) {
		t.Error("Can no respeta todos los permisos pedidos")
	}
	if !p.Platform() {
		t.Error("sin membresia deberia ser modo plataforma")
	}
}

func TestReusedErrorIsInvalidToken(t *testing.T) {
	err := error(&reusedError{family: uuid.New()})
	if !errors.Is(err, ErrInvalidToken) {
		t.Error("un refresh reutilizado debe responder como token invalido")
	}
}

func TestValidIP(t *testing.T) {
	if ip := validIP("172.18.0.1"); ip == nil || *ip != "172.18.0.1" {
		t.Errorf("IPv4 valida descartada: %v", ip)
	}
	if ip := validIP("no-es-ip"); ip != nil {
		t.Errorf("una IP invalida deberia ser nil: %v", *ip)
	}
}
