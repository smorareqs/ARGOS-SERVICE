package auth

import (
	"sync"
	"time"
)

// Bloqueo por fuerza bruta (fase01.md §3.7): 10 intentos fallidos en 15
// minutos por correo + IP bloquean ese par hasta que termine la ventana.
const (
	maxFailedAttempts = 10
	attemptWindow     = 15 * time.Minute
)

// attemptLimiter cuenta intentos fallidos en memoria. Basta con una instancia
// de la API; con varias, cada una cuenta por separado (sin Redis, por
// decision del proyecto).
type attemptLimiter struct {
	mu      sync.Mutex
	entries map[string]*attempts
	now     func() time.Time
}

type attempts struct {
	count int
	first time.Time
}

func newAttemptLimiter(now func() time.Time) *attemptLimiter {
	return &attemptLimiter{entries: map[string]*attempts{}, now: now}
}

// blockedFor devuelve cuanto falta para poder reintentar (0 si no hay bloqueo).
func (l *attemptLimiter) blockedFor(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		return 0
	}
	elapsed := l.now().Sub(e.first)
	if elapsed >= attemptWindow {
		delete(l.entries, key)
		return 0
	}
	if e.count < maxFailedAttempts {
		return 0
	}
	return attemptWindow - elapsed
}

// fail registra un intento fallido.
func (l *attemptLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[key]
	if !ok || now.Sub(e.first) >= attemptWindow {
		l.entries[key] = &attempts{count: 1, first: now}
		l.prune(now)
		return
	}
	e.count++
}

// reset olvida los intentos tras un login correcto.
func (l *attemptLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// prune descarta ventanas vencidas para que el mapa no crezca sin limite.
func (l *attemptLimiter) prune(now time.Time) {
	if len(l.entries) < 1024 {
		return
	}
	for k, e := range l.entries {
		if now.Sub(e.first) >= attemptWindow {
			delete(l.entries, k)
		}
	}
}
