// Package ratelimit limita peticiones por clave (IP, usuario o clave API) con
// una ventana fija en memoria (fase06.md §5). No usa Redis: cada instancia de
// la API cuenta por su lado, asi que con N instancias el limite efectivo es
// N veces el configurado. Suficiente para frenar abusos; documentado en la
// guia de operacion.
package ratelimit

import (
	"sync"
	"time"
)

// maxKeys es el tamaño a partir del cual se limpian las ventanas vencidas.
const maxKeys = 10000

type window struct {
	start time.Time
	hits  int
}

// Limiter cuenta peticiones por clave en ventanas fijas.
type Limiter struct {
	mu     sync.Mutex
	limit  int
	every  time.Duration
	keys   map[string]*window
	nowFor func() time.Time
}

// New crea un limitador de limit peticiones por cada ventana every.
func New(limit int, every time.Duration) *Limiter {
	return &Limiter{limit: limit, every: every, keys: map[string]*window{}, nowFor: time.Now}
}

// PerMinute crea un limitador de n peticiones por minuto; n <= 0 = sin
// limite (devuelve nil, que los middlewares ignoran).
func PerMinute(n int) *Limiter {
	if n <= 0 {
		return nil
	}
	return New(n, time.Minute)
}

// SetClock cambia el reloj (pruebas).
func (l *Limiter) SetClock(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nowFor = now
}

// Allow registra una peticion de key. Si supera el limite devuelve false y
// cuanto falta para que se abra la ventana siguiente.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.nowFor()
	if len(l.keys) >= maxKeys {
		for k, w := range l.keys {
			if now.Sub(w.start) >= l.every {
				delete(l.keys, k)
			}
		}
	}
	w, ok := l.keys[key]
	if !ok || now.Sub(w.start) >= l.every {
		w = &window{start: now}
		l.keys[key] = w
	}
	if w.hits >= l.limit {
		return false, w.start.Add(l.every).Sub(now)
	}
	w.hits++
	return true, 0
}
