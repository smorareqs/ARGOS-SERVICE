package ratelimit

import (
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	l := New(3, time.Minute)
	l.SetClock(func() time.Time { return now })
	for i := range 3 {
		if ok, _ := l.Allow("ip"); !ok {
			t.Fatalf("la petición %d debía pasar", i+1)
		}
	}
	ok, wait := l.Allow("ip")
	if ok || wait != time.Minute {
		t.Fatalf("la cuarta debía frenarse con 1 min de espera: %v %v", ok, wait)
	}
	if ok, _ := l.Allow("otra"); !ok {
		t.Fatal("otra clave cuenta aparte")
	}
	now = now.Add(61 * time.Second)
	if ok, _ := l.Allow("ip"); !ok {
		t.Fatal("en la ventana siguiente vuelve a pasar")
	}
}
