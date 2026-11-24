// Package events es el bus de eventos de dominio dentro del proceso: un modulo
// publica ("ticket.created") y otros reaccionan sin que el primero los
// conozca. Asi notificaciones (Persona B) se engancha a mantenimiento,
// alertas y reservas (Persona A) sin tocar su codigo (fase03.md §4).
//
// Los suscriptores corren en orden, de forma sincrona y dentro de la misma
// transaccion que el cambio: si uno falla, el cambio se revierte. Por eso un
// suscriptor no hace trabajo lento ni llama a servicios externos; encola un
// job (jobs.Enqueue con la misma q) y el worker hace el resto.
package events

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
)

// Event es un hecho de dominio ya ocurrido. Data es un struct propio de cada
// tipo (documentado junto a la constante del tipo en su modulo).
type Event struct {
	Type     string
	TenantID uuid.UUID
	// ActorID es quien lo provoco; nil = el sistema (un job).
	ActorID *uuid.UUID
	Data    any
}

// Handler reacciona a un evento con la transaccion del cambio.
type Handler func(ctx context.Context, q database.Querier, e Event) error

// Bus registra suscriptores y publica eventos. El valor cero no sirve: usa New.
type Bus struct {
	mu   sync.RWMutex
	subs map[string][]Handler
}

// New crea un bus vacio.
func New() *Bus {
	return &Bus{subs: map[string][]Handler{}}
}

// Subscribe agrega un handler al tipo. Se llama al armar la aplicacion
// (app.NewServices), nunca durante una peticion.
func (b *Bus) Subscribe(eventType string, h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[eventType] = append(b.subs[eventType], h)
}

// Publish corre los handlers del tipo en orden. Un bus nil no hace nada (las
// pruebas de un modulo no necesitan armarlo).
func (b *Bus) Publish(ctx context.Context, q database.Querier, e Event) error {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	handlers := b.subs[e.Type]
	b.mu.RUnlock()
	for _, h := range handlers {
		if err := h(ctx, q, e); err != nil {
			return fmt.Errorf("evento %s: %w", e.Type, err)
		}
	}
	return nil
}
