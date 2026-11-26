// Package realtime reenvia avisos a la web y la app al instante por
// WebSocket (`GET /api/v1/ws`, fase03.md §4; lo reutiliza el chat de la
// fase 6).
//
// Quien produce el aviso (la API o el worker) llama a Publish dentro de su
// transaccion: es un NOTIFY de PostgreSQL, asi que se entrega solo si el
// cambio se confirma y llega a cualquier proceso de la API. El Hub de cada
// proceso de la API escucha el canal y lo reenvia a los sockets abiertos de
// esos usuarios en esa empresa.
package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/auth"
)

// channel es el canal de NOTIFY.
const channel = "argos_realtime"

// maxPayload deja margen bajo el limite de 8000 bytes de NOTIFY.
const maxPayload = 7000

// Message es lo que recibe el cliente.
type Message struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

type envelope struct {
	Tenant uuid.UUID       `json:"t"`
	Users  []uuid.UUID     `json:"u"`
	Type   string          `json:"y"`
	Data   json.RawMessage `json:"d,omitempty"`
}

// Publish manda type/data a los usuarios de la empresa con la transaccion q.
// Si data no cabe en un NOTIFY se manda solo el tipo (el cliente recarga).
func Publish(ctx context.Context, q database.Querier, tenantID uuid.UUID, users []uuid.UUID, typ string, data any) error {
	if len(users) == 0 {
		return nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("realtime: %w", err)
	}
	env := envelope{Tenant: tenantID, Users: users, Type: typ, Data: raw}
	payload, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("realtime: %w", err)
	}
	if len(payload) > maxPayload {
		env.Data = nil
		if payload, err = json.Marshal(env); err != nil {
			return err
		}
	}
	_, err = q.Exec(ctx, "SELECT pg_notify($1, $2)", channel, string(payload))
	return err
}

type client struct {
	tenant uuid.UUID
	user   uuid.UUID
	send   chan Message
}

// Hub mantiene los sockets abiertos de este proceso.
type Hub struct {
	db   *database.DB
	auth *auth.Service
	log  *slog.Logger

	mu      sync.RWMutex
	clients map[uuid.UUID]map[*client]struct{}
}

// NewHub crea el hub. Run debe correr mientras viva la API.
func NewHub(db *database.DB, authSvc *auth.Service, log *slog.Logger) *Hub {
	return &Hub{db: db, auth: authSvc, log: log, clients: map[uuid.UUID]map[*client]struct{}{}}
}

// Run escucha el canal de NOTIFY hasta que ctx termine; si la conexion se
// cae, reintenta con espera creciente.
func (h *Hub) Run(ctx context.Context) {
	wait := time.Second
	for ctx.Err() == nil {
		err := h.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		h.log.WarnContext(ctx, "Tiempo real: se perdio la escucha; reintentando", "error", err, "wait", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, 30*time.Second)
	}
}

func (h *Hub) listen(ctx context.Context) error {
	conn, err := h.db.Pool().Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return err
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var env envelope
		if err := json.Unmarshal([]byte(n.Payload), &env); err != nil {
			continue
		}
		h.dispatch(env)
	}
}

func (h *Hub) dispatch(env envelope) {
	msg := Message{Type: env.Type, Data: env.Data}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, u := range env.Users {
		for c := range h.clients[u] {
			if c.tenant != env.Tenant {
				continue
			}
			select {
			case c.send <- msg:
			default: // cliente lento: se pierde este aviso (la bandeja se recarga sola).
			}
		}
	}
}

// Connected cuenta los sockets abiertos de un usuario (pruebas).
func (h *Hub) Connected(user uuid.UUID) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients[user])
}

func (h *Hub) add(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clients[c.user] == nil {
		h.clients[c.user] = map[*client]struct{}{}
	}
	h.clients[c.user][c] = struct{}{}
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients[c.user], c)
	if len(h.clients[c.user]) == 0 {
		delete(h.clients, c.user)
	}
}

// ServeHTTP atiende GET /api/v1/ws. El token va en `?token=` (los navegadores
// no permiten headers en WebSocket) o en Authorization: Bearer.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		if v := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(v), "bearer ") {
			token = strings.TrimSpace(v[7:])
		}
	}
	if token == "" {
		http.Error(w, "Falta el token.", http.StatusUnauthorized)
		return
	}
	p, err := h.auth.ResolvePrincipal(r.Context(), token)
	if err != nil || p.Membership == nil {
		http.Error(w, "Sesión no válida.", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// La autenticacion es por token, no por cookie: cualquier origen.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()

	c := &client{tenant: p.Membership.TenantID, user: p.User.ID, send: make(chan Message, 32)}
	h.add(c)
	defer h.remove(c)

	ctx := conn.CloseRead(context.Background())
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	if err := write(ctx, conn, Message{Type: "hello"}); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-c.send:
			if err := write(ctx, conn, msg); err != nil {
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func write(ctx context.Context, conn *websocket.Conn, msg Message) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, b)
}
