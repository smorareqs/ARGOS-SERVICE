// Package reqctx guarda en el contexto datos de la peticion HTTP que los
// servicios necesitan sin conocer HTTP: la IP de origen y el agente de
// usuario (bitacora, sesiones y bloqueo por intentos).
package reqctx

import (
	"context"
	"net/netip"
)

type clientKey struct{}

type client struct {
	ip        string
	userAgent string
}

// WithClient guarda la IP y el agente de usuario de la peticion.
func WithClient(ctx context.Context, ip, userAgent string) context.Context {
	if len(userAgent) > 256 {
		userAgent = userAgent[:256]
	}
	return context.WithValue(ctx, clientKey{}, client{ip: ip, userAgent: userAgent})
}

// IP devuelve la IP de origen (vacia si no se conoce).
func IP(ctx context.Context) string {
	c, _ := ctx.Value(clientKey{}).(client)
	return c.ip
}

// UserAgent devuelve el agente de usuario (vacio si no se conoce).
func UserAgent(ctx context.Context) string {
	c, _ := ctx.Value(clientKey{}).(client)
	return c.userAgent
}

// ValidIP devuelve la IP en forma canonica para una columna inet, o nil.
func ValidIP(raw string) *string {
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return nil
	}
	s := addr.String()
	return &s
}

// OptionalString devuelve nil para la cadena vacia.
func OptionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type traceKey struct{}

// Trace es quien hizo la peticion, para el registro de acceso (fase06.md §5:
// logs con tenant_id y user_id). El middleware de acceso la crea vacia y el
// Guard la completa al autenticar.
type Trace struct {
	UserID   string
	TenantID string
}

// WithTrace guarda una Trace vacia en el contexto y la devuelve.
func WithTrace(ctx context.Context) (context.Context, *Trace) {
	t := &Trace{}
	return context.WithValue(ctx, traceKey{}, t), t
}

// TraceOf devuelve la Trace de la peticion (nil fuera de HTTP).
func TraceOf(ctx context.Context) *Trace {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	return t
}
