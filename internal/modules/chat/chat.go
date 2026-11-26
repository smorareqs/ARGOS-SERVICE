// Package chat es el chat interno y el chat huesped ↔ empresa (fase06.md §2,
// Persona B). Sigue el contrato que ya usa la web (W-M1, features/chat).
//
//   - Internas: cualquiera con chat.internal crea una 1:1 (se reutiliza si ya
//     existe) o un grupo con personas de la empresa. Solo las ven sus
//     participantes; quien administra usuarios puede unirse a cualquiera.
//   - Con huesped: una por reserva. Se crea al confirmar la reserva (bandera
//     guest_chat) o la primera vez que el huesped abre el chat, con el huesped,
//     quienes tienen chat.guest y un saludo. La ve todo el que tiene chat.guest
//     (y alcance sobre la propiedad); quien responde queda como participante.
//     Se archiva 24 h despues del check-out o al cancelar.
//   - Cada mensaje sale por tiempo real (`chat.message`, Data ChatMessage) a
//     quien puede ver la conversacion y por push a sus participantes (los
//     usuarios segun su preferencia chat_message; el huesped a su telefono).
package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/core/paging"
	"github.com/smorareqs/argos-service/internal/core/storage"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/guestaccess"
	"github.com/smorareqs/argos-service/internal/modules/notifications"
	"github.com/smorareqs/argos-service/internal/modules/realtime"
	"github.com/smorareqs/argos-service/internal/modules/reservations"
)

// EventMessage es el tipo del mensaje de tiempo real.
const EventMessage = "chat.message"

// JobArchive archiva las conversaciones de huespedes que ya salieron.
const JobArchive = "chat_archive"

// Errores del modulo.
var (
	ErrNotFound     = apperr.New(apperr.KindNotFound, "La conversación no existe.")
	ErrEmpty        = apperr.New(apperr.KindValidation, "Escribe un mensaje o adjunta una foto.").WithField("body.body")
	ErrArchived     = apperr.New(apperr.KindValidation, "Esta conversación ya está archivada.")
	ErrPeople       = apperr.New(apperr.KindValidation, "Elige al menos una persona de la empresa.").WithField("body.user_ids")
	ErrUnknownUser  = apperr.New(apperr.KindValidation, "Una de las personas no está en la empresa.").WithField("body.user_ids")
	ErrKind         = apperr.New(apperr.KindValidation, "Solo se crean conversaciones internas; las de huéspedes salen de la reserva.").WithField("body.kind")
	ErrImage        = apperr.New(apperr.KindValidation, "La foto no se subió bien. Intenta de nuevo.").WithField("body.file_id")
	ErrGuestOnly    = apperr.New(apperr.KindValidation, "En la conversación con el huésped no se agregan personas a mano.")
	ErrNotAllowed   = apperr.New(apperr.KindPermissionDenied, "No puedes cambiar los participantes de esta conversación.")
	ErrFeature      = apperr.New(apperr.KindPermissionDenied, "El chat con la empresa no está activo para tu estancia.")
	ErrNotMember    = apperr.New(apperr.KindNotFound, "Esa persona no está en la conversación.")
	ErrAlreadyThere = apperr.New(apperr.KindAlreadyExists, "Esa persona ya está en la conversación.")
)

// ChatParticipant es un miembro de la conversacion.
type ChatParticipant struct {
	ConversationID uuid.UUID `db:"conversation_id" json:"-"`
	Type           string    `db:"type" json:"type" enum:"user,guest"`
	ID             uuid.UUID `db:"id" json:"id" format:"uuid"`
	Name           string    `db:"name" json:"name"`
	Role           *string   `db:"role" json:"role" nullable:"true"`
	PhotoURL       *string   `db:"photo_url" json:"photo_url" nullable:"true"`
}

// ChatReservation es la reserva de una conversacion con huesped.
type ChatReservation struct {
	ID           uuid.UUID `db:"id" json:"id" format:"uuid"`
	Code         string    `db:"code" json:"code"`
	PropertyID   uuid.UUID `db:"property_id" json:"property_id" format:"uuid"`
	PropertyCode string    `db:"property_code" json:"property_code"`
	PropertyName string    `db:"property_name" json:"property_name"`
	CheckInDate  string    `db:"check_in_date" json:"check_in_date" format:"date"`
	CheckOutDate string    `db:"check_out_date" json:"check_out_date" format:"date"`
	Status       string    `db:"status" json:"status"`
	GuestName    string    `db:"guest_name" json:"guest_name"`
	GuestsCount  int       `db:"guests_count" json:"guests_count"`
}

// ChatMessage es un mensaje.
type ChatMessage struct {
	ID             uuid.UUID `json:"id" format:"uuid"`
	ConversationID uuid.UUID `json:"conversation_id" format:"uuid"`
	SenderType     string    `json:"sender_type" enum:"user,guest,system"`
	SenderID       string    `json:"sender_id" doc:"Vacio en los mensajes del sistema."`
	SenderName     string    `json:"sender_name"`
	Body           *string   `json:"body" nullable:"true"`
	FileURL        *string   `json:"file_url" nullable:"true" doc:"URL firmada de la foto (1 h)."`
	CreatedAt      time.Time `json:"created_at"`
	Mine           bool      `json:"mine" doc:"Lo mando quien tiene la sesion."`
}

// Conversation es una conversacion con su resumen.
type Conversation struct {
	ID            uuid.UUID         `json:"id" format:"uuid"`
	Kind          string            `json:"kind" enum:"internal,guest"`
	Title         string            `json:"title"`
	Reservation   *ChatReservation  `json:"reservation" doc:"Solo en las de huesped."`
	Participants  []ChatParticipant `json:"participants"`
	LastMessage   *ChatMessage      `json:"last_message"`
	LastMessageAt *time.Time        `json:"last_message_at" nullable:"true"`
	Unread        int               `json:"unread"`
	IsArchived    bool              `json:"is_archived"`
	Muted         bool              `json:"muted"`
}

// ConversationPage es GET /chat/conversations.
type ConversationPage struct {
	Items      []Conversation `json:"items"`
	NextCursor *string        `json:"next_cursor" nullable:"true" doc:"Siempre null: llegan todas las vigentes."`
}

// MessagePage son mensajes, los antiguos primero.
type MessagePage struct {
	Items      []ChatMessage `json:"items"`
	NextCursor *string       `json:"next_cursor" nullable:"true" doc:"Para pedir los anteriores con ?before=."`
}

// ConversationCreate es el cuerpo de POST /chat/conversations.
type ConversationCreate struct {
	_       struct{}    `additionalProperties:"true"`
	Kind    string      `json:"kind,omitempty" enum:"internal" doc:"Solo internal."`
	Title   *string     `json:"title,omitempty" maxLength:"128" doc:"Nombre del grupo; sin titulo y con una persona es 1:1."`
	UserIDs []uuid.UUID `json:"user_ids" minItems:"1" maxItems:"50"`
}

// MessageCreate es el cuerpo de POST .../messages.
type MessageCreate struct {
	_      struct{}   `additionalProperties:"true"`
	Body   string     `json:"body,omitempty" maxLength:"4000"`
	FileID *uuid.UUID `json:"file_id,omitempty" format:"uuid" doc:"Foto subida con module=chat, kind=image."`
}

// ParticipantAdd es el cuerpo de POST .../participants.
type ParticipantAdd struct {
	_      struct{}  `additionalProperties:"true"`
	UserID uuid.UUID `json:"user_id" format:"uuid"`
}

// ConversationMute es el cuerpo de POST .../mute.
type ConversationMute struct {
	_     struct{} `additionalProperties:"true"`
	Muted bool     `json:"muted"`
}

// ChatPerson es alguien con quien se puede abrir una conversacion interna.
type ChatPerson struct {
	ID   uuid.UUID `db:"id" json:"id" format:"uuid"`
	Name string    `db:"name" json:"name"`
	Role *string   `db:"role" json:"role" nullable:"true"`
}

// GuestChat es GET /guest/chat: la conversacion del huesped con la empresa.
type GuestChat struct {
	ID           uuid.UUID         `json:"id" format:"uuid"`
	Title        string            `json:"title" doc:"Nombre de la empresa."`
	Participants []ChatParticipant `json:"participants" doc:"Quienes atienden."`
	Messages     []ChatMessage     `json:"messages" doc:"Los ultimos 100, antiguos primero."`
	Unread       int               `json:"unread"`
	IsArchived   bool              `json:"is_archived"`
}

// Guest es el huesped que escribe (lo arma la API con su sesion).
type Guest struct {
	TenantID      uuid.UUID
	TenantName    string
	ReservationID uuid.UUID
	GuestID       uuid.UUID
	Enabled       bool // bandera guest_chat
}

// Service es el chat.
type Service struct {
	db      *database.DB
	storage *storage.Service
	log     *slog.Logger
	now     func() time.Time
}

// NewService crea el servicio.
func NewService(db *database.DB, st *storage.Service, log *slog.Logger) *Service {
	return &Service{db: db, storage: st, log: log, now: time.Now}
}

// SetClock cambia el reloj (pruebas).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// viewer es quien mira: un usuario o un huesped.
type viewer struct {
	kind string
	id   uuid.UUID
}

func userViewer(id uuid.UUID) viewer  { return viewer{kind: "user", id: id} }
func guestViewer(id uuid.UUID) viewer { return viewer{kind: "guest", id: id} }

// ── Lecturas ──────────────────────────────────────────────

type convRow struct {
	ID            uuid.UUID  `db:"id"`
	Kind          string     `db:"kind"`
	Title         *string    `db:"title"`
	ReservationID *uuid.UUID `db:"reservation_id"`
	PropertyID    *uuid.UUID `db:"property_id"`
	LastMessageAt *time.Time `db:"last_message_at"`
	IsArchived    bool       `db:"is_archived"`
	LastReadAt    *time.Time `db:"last_read_at"`
	Member        bool       `db:"member"`
	Muted         bool       `db:"muted"`
}

const convSelect = `
	SELECT c.id, c.kind, c.title, c.reservation_id, r.property_id, c.last_message_at, c.is_archived,
	       cp.last_read_at, (cp.id IS NOT NULL) AS member, coalesce(cp.muted, false) AS muted
	FROM conversations c
	LEFT JOIN conversation_participants cp ON cp.conversation_id = c.id AND cp.user_id = $1
	LEFT JOIN reservations r ON r.id = c.reservation_id`

type msgRow struct {
	ID             uuid.UUID `db:"id"`
	ConversationID uuid.UUID `db:"conversation_id"`
	SenderType     string    `db:"sender_type"`
	SenderID       string    `db:"sender_id"`
	SenderName     string    `db:"sender_name"`
	Body           *string   `db:"body"`
	FileKey        *string   `db:"file_key"`
	CreatedAt      time.Time `db:"created_at"`
}

const msgSelect = `
	SELECT m.id, m.conversation_id, m.sender_type,
	       coalesce(m.sender_user_id::text, m.sender_guest_id::text, '') AS sender_id,
	       coalesce(u.full_name, g.full_name, 'ARGOS') AS sender_name,
	       m.body, f.s3_key AS file_key, m.created_at
	FROM messages m
	LEFT JOIN shared.users u ON u.id = m.sender_user_id
	LEFT JOIN guests g ON g.id = m.sender_guest_id
	LEFT JOIN files f ON f.id = m.file_id AND f.deleted_at IS NULL`

func (s *Service) message(ctx context.Context, tenantID uuid.UUID, r msgRow, v viewer) ChatMessage {
	m := ChatMessage{
		ID: r.ID, ConversationID: r.ConversationID, SenderType: r.SenderType, SenderID: r.SenderID,
		SenderName: r.SenderName, Body: r.Body, CreatedAt: r.CreatedAt.UTC(),
		Mine: r.SenderType == v.kind && r.SenderID == v.id.String(),
	}
	if r.FileKey != nil && s.storage != nil {
		if url, err := s.storage.PresignDownload(ctx, tenantID, *r.FileKey, time.Hour); err == nil {
			m.FileURL = &url
		}
	}
	return m
}

// visible dice si el actor ve la conversacion.
func visible(actor authz.Actor, scope authz.PropertyScope, c convRow) bool {
	if c.Kind == "internal" {
		return c.Member
	}
	return actor.Permissions[authz.ChatGuest] && c.PropertyID != nil && scope.Allows(*c.PropertyID)
}

// List devuelve las conversaciones del actor de ese tipo, las que tienen
// mensajes nuevos primero. Las archivadas salen 30 dias.
func (s *Service) List(ctx context.Context, actor authz.Actor, kind string) (ConversationPage, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (ConversationPage, error) {
		if kind == "guest" && !actor.Permissions[authz.ChatGuest] {
			return ConversationPage{Items: []Conversation{}}, nil
		}
		scope, err := authz.ResolvePropertyScope(ctx, q, actor)
		if err != nil {
			return ConversationPage{}, err
		}
		all, ids := scope.SQLArgs()
		rows, err := q.Query(ctx, convSelect+`
			WHERE c.kind = $2
			  AND (($2 = 'internal' AND cp.id IS NOT NULL) OR ($2 = 'guest' AND ($3::bool OR r.property_id = ANY($4::uuid[]))))
			  AND (NOT c.is_archived OR c.archived_at > $5)
			ORDER BY c.is_archived, c.last_message_at DESC NULLS LAST, c.created_at DESC
			LIMIT 200`, actor.UserID, kind, all, ids, s.now().Add(-30*24*time.Hour))
		if err != nil {
			return ConversationPage{}, err
		}
		convs, err := pgx.CollectRows(rows, pgx.RowToStructByName[convRow])
		if err != nil {
			return ConversationPage{}, err
		}
		items, err := s.summaries(ctx, q, actor, convs)
		return ConversationPage{Items: items}, err
	})
}

// summaries arma participantes, reserva, ultimo mensaje y no leidos de varias
// conversaciones con cuatro consultas.
func (s *Service) summaries(ctx context.Context, q database.Querier, actor authz.Actor, convs []convRow) ([]Conversation, error) {
	out := make([]Conversation, 0, len(convs))
	if len(convs) == 0 {
		return out, nil
	}
	ids := make([]uuid.UUID, len(convs))
	reads := make([]*time.Time, len(convs))
	var resIDs []uuid.UUID
	for i, c := range convs {
		ids[i], reads[i] = c.ID, c.LastReadAt
		if c.ReservationID != nil {
			resIDs = append(resIDs, *c.ReservationID)
		}
	}
	people, err := participants(ctx, q, actor.TenantID, ids)
	if err != nil {
		return nil, err
	}
	stays := map[uuid.UUID]ChatReservation{}
	if len(resIDs) > 0 {
		rows, err := q.Query(ctx, `
			SELECT r.id, r.code, r.property_id, p.code AS property_code, p.name AS property_name,
			       r.check_in_date::text AS check_in_date, r.check_out_date::text AS check_out_date, r.status,
			       coalesce(g.full_name, 'Huésped') AS guest_name, (r.adults + r.children) AS guests_count
			FROM reservations r
			JOIN properties p ON p.id = r.property_id
			LEFT JOIN guests g ON g.id = r.primary_guest_id
			WHERE r.id = ANY($1)`, resIDs)
		if err != nil {
			return nil, err
		}
		list, err := pgx.CollectRows(rows, pgx.RowToStructByName[ChatReservation])
		if err != nil {
			return nil, err
		}
		for _, r := range list {
			stays[r.ID] = r
		}
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (m.conversation_id) * FROM (`+msgSelect+`
		WHERE m.conversation_id = ANY($1) AND m.deleted_at IS NULL) m
		ORDER BY m.conversation_id, m.created_at DESC`, ids)
	if err != nil {
		return nil, err
	}
	lastRows, err := pgx.CollectRows(rows, pgx.RowToStructByName[msgRow])
	if err != nil {
		return nil, err
	}
	last := map[uuid.UUID]msgRow{}
	for _, m := range lastRows {
		last[m.ConversationID] = m
	}
	unread := map[uuid.UUID]int{}
	rows, err = q.Query(ctx, `
		SELECT m.conversation_id, count(*)::int
		FROM messages m
		JOIN unnest($1::uuid[], $2::timestamptz[]) AS x(cid, read_at) ON x.cid = m.conversation_id
		WHERE m.deleted_at IS NULL AND (x.read_at IS NULL OR m.created_at > x.read_at)
		  AND NOT (m.sender_type = 'user' AND m.sender_user_id = $3)
		GROUP BY 1`, ids, reads, actor.UserID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id uuid.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return nil, err
		}
		unread[id] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	me := userViewer(actor.UserID)
	for _, c := range convs {
		conv := Conversation{
			ID: c.ID, Kind: c.Kind, Participants: people[c.ID], LastMessageAt: c.LastMessageAt,
			Unread: unread[c.ID], IsArchived: c.IsArchived, Muted: c.Muted,
		}
		if conv.Participants == nil {
			conv.Participants = []ChatParticipant{}
		}
		if c.ReservationID != nil {
			if r, ok := stays[*c.ReservationID]; ok {
				conv.Reservation = &r
			}
		}
		if m, ok := last[c.ID]; ok {
			msg := s.message(ctx, actor.TenantID, m, me)
			conv.LastMessage = &msg
		}
		conv.Title = title(c, conv, actor.UserID)
		out = append(out, conv)
	}
	return out, nil
}

// title: el del grupo, el huesped o los nombres de los demas.
func title(c convRow, conv Conversation, me uuid.UUID) string {
	if c.Title != nil && strings.TrimSpace(*c.Title) != "" {
		return *c.Title
	}
	if conv.Reservation != nil {
		return conv.Reservation.GuestName
	}
	var names []string
	for _, p := range conv.Participants {
		if p.Type == "user" && p.ID != me {
			names = append(names, p.Name)
		}
	}
	if len(names) == 0 {
		return "Solo tú"
	}
	if len(names) > 3 {
		return strings.Join(names[:3], ", ") + fmt.Sprintf(" y %d más", len(names)-3)
	}
	return strings.Join(names, ", ")
}

func participants(ctx context.Context, q database.Querier, tenantID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID][]ChatParticipant, error) {
	rows, err := q.Query(ctx, `
		SELECT cp.conversation_id, cp.participant_type AS type, coalesce(cp.user_id, cp.guest_id) AS id,
		       coalesce(u.full_name, g.full_name, '') AS name,
		       CASE WHEN cp.guest_id IS NOT NULL THEN 'Huésped' ELSE (
		         SELECT ro.name FROM shared.memberships ms JOIN shared.roles ro ON ro.id = ms.role_id
		         WHERE ms.user_id = cp.user_id AND ms.tenant_id = $2 ORDER BY ms.created_at LIMIT 1) END AS role,
		       NULL::text AS photo_url
		FROM conversation_participants cp
		LEFT JOIN shared.users u ON u.id = cp.user_id
		LEFT JOIN guests g ON g.id = cp.guest_id
		WHERE cp.conversation_id = ANY($1)
		ORDER BY cp.participant_type DESC, cp.joined_at`, ids, tenantID)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[ChatParticipant])
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID][]ChatParticipant{}
	for _, p := range list {
		out[p.ConversationID] = append(out[p.ConversationID], p)
	}
	return out, nil
}

// load trae la conversacion si el actor la ve (si no, 404).
func load(ctx context.Context, q database.Querier, actor authz.Actor, id uuid.UUID, lock bool) (convRow, error) {
	sql := convSelect + ` WHERE c.id = $2`
	if lock {
		sql += ` FOR UPDATE OF c`
	}
	rows, err := q.Query(ctx, sql, actor.UserID, id)
	if err != nil {
		return convRow{}, err
	}
	c, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[convRow])
	if errors.Is(err, pgx.ErrNoRows) {
		return convRow{}, ErrNotFound
	}
	if err != nil {
		return convRow{}, err
	}
	scope, err := authz.ResolvePropertyScope(ctx, q, actor)
	if err != nil {
		return convRow{}, err
	}
	if !visible(actor, scope, c) {
		return convRow{}, ErrNotFound
	}
	return c, nil
}

// Get es una conversacion con su resumen.
func (s *Service) Get(ctx context.Context, actor authz.Actor, id uuid.UUID) (Conversation, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Conversation, error) {
		return s.get(ctx, q, actor, id)
	})
}

func (s *Service) get(ctx context.Context, q database.Querier, actor authz.Actor, id uuid.UUID) (Conversation, error) {
	c, err := load(ctx, q, actor, id, false)
	if err != nil {
		return Conversation{}, err
	}
	list, err := s.summaries(ctx, q, actor, []convRow{c})
	if err != nil {
		return Conversation{}, err
	}
	return list[0], nil
}

// Messages devuelve una pagina de mensajes, los antiguos primero; before es el
// next_cursor de la pagina anterior.
func (s *Service) Messages(ctx context.Context, actor authz.Actor, id uuid.UUID, before string, limit int) (MessagePage, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (MessagePage, error) {
		if _, err := load(ctx, q, actor, id, false); err != nil {
			return MessagePage{}, err
		}
		return s.page(ctx, q, actor.TenantID, id, before, limit, userViewer(actor.UserID))
	})
}

func (s *Service) page(ctx context.Context, q database.Querier, tenantID, id uuid.UUID, before string, limit int, v viewer) (MessagePage, error) {
	limit = min(paging.Limit(limit), 200)
	key, err := paging.Decode(before, 2)
	if err != nil {
		return MessagePage{}, apperr.New(apperr.KindValidation, "El cursor de la página no es válido.").WithField("query.before")
	}
	var beforeAt, beforeID *string
	if key != nil {
		beforeAt, beforeID = &key[0], &key[1]
	}
	rows, err := q.Query(ctx, msgSelect+`
		WHERE m.conversation_id = $1 AND m.deleted_at IS NULL
		  AND ($2::timestamptz IS NULL OR (m.created_at, m.id) < ($2::timestamptz, $3::uuid))
		ORDER BY m.created_at DESC, m.id DESC LIMIT $4`, id, beforeAt, beforeID, limit+1)
	if err != nil {
		return MessagePage{}, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[msgRow])
	if err != nil {
		return MessagePage{}, err
	}
	list, next := paging.Page(list, limit, func(m msgRow) []string {
		return []string{m.CreatedAt.Format(time.RFC3339Nano), m.ID.String()}
	})
	items := make([]ChatMessage, len(list))
	for i, m := range list {
		items[len(list)-1-i] = s.message(ctx, tenantID, m, v)
	}
	return MessagePage{Items: items, NextCursor: next}, nil
}

// People son las personas de la empresa con chat interno (menos el actor).
func (s *Service) People(ctx context.Context, actor authz.Actor) ([]ChatPerson, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]ChatPerson, error) {
		rows, err := q.Query(ctx, `
			SELECT DISTINCT ON (u.id) u.id, u.full_name AS name, r.name AS role
			FROM shared.memberships m
			JOIN shared.users u ON u.id = m.user_id AND u.is_active
			JOIN shared.roles r ON r.id = m.role_id
			JOIN shared.role_permissions rp ON rp.role_id = r.id AND rp.permission = $3
			WHERE m.tenant_id = $1 AND m.status = 'active' AND u.id <> $2
			ORDER BY u.id`, actor.TenantID, actor.UserID, string(authz.ChatInternal))
		if err != nil {
			return nil, err
		}
		list, err := pgx.CollectRows(rows, pgx.RowToStructByName[ChatPerson])
		if err != nil {
			return nil, err
		}
		slices.SortFunc(list, func(a, b ChatPerson) int { return strings.Compare(a.Name, b.Name) })
		return list, nil
	})
}

// ── Escrituras del personal ───────────────────────────────

// Create abre una conversacion interna. Con una sola persona y sin titulo
// reutiliza la 1:1 que ya exista entre los dos.
func (s *Service) Create(ctx context.Context, actor authz.Actor, in ConversationCreate) (Conversation, error) {
	if in.Kind != "" && in.Kind != "internal" {
		return Conversation{}, ErrKind
	}
	others := make([]uuid.UUID, 0, len(in.UserIDs))
	for _, u := range in.UserIDs {
		if u != actor.UserID && u != uuid.Nil && !slices.Contains(others, u) {
			others = append(others, u)
		}
	}
	if len(others) == 0 {
		return Conversation{}, ErrPeople
	}
	var groupTitle *string
	if in.Title != nil && strings.TrimSpace(*in.Title) != "" {
		t := strings.TrimSpace(*in.Title)
		groupTitle = &t
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Conversation, error) {
		var n int
		if err := q.QueryRow(ctx, `
			SELECT count(*) FROM shared.memberships m JOIN shared.users u ON u.id = m.user_id AND u.is_active
			WHERE m.tenant_id = $1 AND m.status = 'active' AND m.user_id = ANY($2)`, actor.TenantID, others).Scan(&n); err != nil {
			return Conversation{}, err
		}
		if n != len(others) {
			return Conversation{}, ErrUnknownUser
		}
		if len(others) == 1 && groupTitle == nil {
			var existing uuid.UUID
			err := q.QueryRow(ctx, `
				SELECT c.id FROM conversations c
				WHERE c.kind = 'internal' AND c.title IS NULL
				  AND (SELECT count(*) FROM conversation_participants x WHERE x.conversation_id = c.id) = 2
				  AND EXISTS (SELECT 1 FROM conversation_participants a WHERE a.conversation_id = c.id AND a.user_id = $1)
				  AND EXISTS (SELECT 1 FROM conversation_participants b WHERE b.conversation_id = c.id AND b.user_id = $2)
				LIMIT 1`, actor.UserID, others[0]).Scan(&existing)
			if err == nil {
				return s.get(ctx, q, actor, existing)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return Conversation{}, err
			}
		}
		var id uuid.UUID
		if err := q.QueryRow(ctx, `INSERT INTO conversations (kind, title, created_by) VALUES ('internal', $1, $2) RETURNING id`,
			groupTitle, actor.UserID).Scan(&id); err != nil {
			return Conversation{}, err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO conversation_participants (conversation_id, participant_type, user_id, last_read_at)
			SELECT $1, 'user', u, CASE WHEN u = $3 THEN now() END FROM unnest($2::uuid[]) AS u`,
			id, append([]uuid.UUID{actor.UserID}, others...), actor.UserID); err != nil {
			return Conversation{}, err
		}
		return s.get(ctx, q, actor, id)
	})
}

// Send manda un mensaje del actor.
func (s *Service) Send(ctx context.Context, actor authz.Actor, id uuid.UUID, in MessageCreate) (ChatMessage, error) {
	body := strings.TrimSpace(in.Body)
	if body == "" && in.FileID == nil {
		return ChatMessage{}, ErrEmpty
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (ChatMessage, error) {
		c, err := load(ctx, q, actor, id, true)
		if err != nil {
			return ChatMessage{}, err
		}
		if c.IsArchived {
			return ChatMessage{}, ErrArchived
		}
		if !c.Member {
			if err := join(ctx, q, id, actor.UserID); err != nil {
				return ChatMessage{}, err
			}
		}
		var bodyPtr *string
		if body != "" {
			bodyPtr = &body
		}
		var msgID uuid.UUID
		if err := q.QueryRow(ctx, `
			INSERT INTO messages (conversation_id, sender_type, sender_user_id, body, created_at)
			VALUES ($1, 'user', $2, $3, $4) RETURNING id`, id, actor.UserID, bodyPtr, s.now()).Scan(&msgID); err != nil {
			return ChatMessage{}, err
		}
		if in.FileID != nil {
			tag, err := q.Exec(ctx, `
				UPDATE files SET entity_type = 'message', entity_id = $1, updated_at = now()
				WHERE id = $2 AND module = 'chat' AND kind = 'image' AND status = 'ready'
				  AND uploaded_by_user_id = $3 AND entity_id IS NULL`, msgID, *in.FileID, actor.UserID)
			if err != nil {
				return ChatMessage{}, err
			}
			if tag.RowsAffected() != 1 {
				return ChatMessage{}, ErrImage
			}
			if _, err := q.Exec(ctx, `UPDATE messages SET file_id = $2 WHERE id = $1`, msgID, *in.FileID); err != nil {
				return ChatMessage{}, err
			}
		}
		return s.deliver(ctx, q, actor.TenantID, c, msgID, userViewer(actor.UserID))
	})
}

func join(ctx context.Context, q database.Querier, id, user uuid.UUID) error {
	_, err := q.Exec(ctx, `
		INSERT INTO conversation_participants (conversation_id, participant_type, user_id)
		VALUES ($1, 'user', $2) ON CONFLICT (conversation_id, user_id) WHERE user_id IS NOT NULL DO NOTHING`, id, user)
	return err
}

// deliver deja el mensaje como el ultimo, lo marca leido para quien lo manda y
// lo reparte por tiempo real y push.
func (s *Service) deliver(ctx context.Context, q database.Querier, tenantID uuid.UUID, c convRow, msgID uuid.UUID, from viewer) (ChatMessage, error) {
	rows, err := q.Query(ctx, msgSelect+` WHERE m.id = $1`, msgID)
	if err != nil {
		return ChatMessage{}, err
	}
	row, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[msgRow])
	if err != nil {
		return ChatMessage{}, err
	}
	if _, err := q.Exec(ctx, `UPDATE conversations SET last_message_at = $2, updated_at = now() WHERE id = $1`, c.ID, row.CreatedAt); err != nil {
		return ChatMessage{}, err
	}
	readSQL := `UPDATE conversation_participants SET last_read_at = $2 WHERE conversation_id = $1 AND user_id = $3`
	if from.kind == "guest" {
		readSQL = `UPDATE conversation_participants SET last_read_at = $2 WHERE conversation_id = $1 AND guest_id = $3`
	}
	if _, err := q.Exec(ctx, readSQL, c.ID, row.CreatedAt, from.id); err != nil {
		return ChatMessage{}, err
	}

	// Quien recibe: participantes (para el push) y, en las de huesped, todo el
	// que tiene chat.guest sobre la propiedad (tiempo real).
	type member struct {
		User  *uuid.UUID
		Guest *uuid.UUID
		Muted bool
	}
	prow, err := q.Query(ctx, `SELECT user_id, guest_id, muted FROM conversation_participants WHERE conversation_id = $1`, c.ID)
	if err != nil {
		return ChatMessage{}, err
	}
	var members []member
	for prow.Next() {
		var m member
		if err := prow.Scan(&m.User, &m.Guest, &m.Muted); err != nil {
			prow.Close()
			return ChatMessage{}, err
		}
		members = append(members, m)
	}
	prow.Close()
	if err := prow.Err(); err != nil {
		return ChatMessage{}, err
	}
	live := []uuid.UUID{}
	var push []uuid.UUID
	guestInside := false
	for _, m := range members {
		if m.Guest != nil {
			guestInside = true
			continue
		}
		if m.User == nil || (from.kind == "user" && *m.User == from.id) {
			continue
		}
		live = append(live, *m.User)
		if !m.Muted {
			push = append(push, *m.User)
		}
	}
	if c.Kind == "guest" {
		holders, err := notifications.Holders(ctx, q, tenantID, c.PropertyID, authz.ChatGuest)
		if err != nil {
			return ChatMessage{}, err
		}
		for _, h := range holders {
			if !(from.kind == "user" && h == from.id) && !slices.Contains(live, h) {
				live = append(live, h)
			}
		}
	}
	others := s.message(ctx, tenantID, row, viewer{})
	if err := realtime.Publish(ctx, q, tenantID, live, EventMessage, others); err != nil {
		return ChatMessage{}, err
	}
	mine := s.message(ctx, tenantID, row, from)
	if from.kind == "user" {
		if err := realtime.Publish(ctx, q, tenantID, []uuid.UUID{from.id}, EventMessage, mine); err != nil {
			return ChatMessage{}, err
		}
	}

	preview := "📷 Foto"
	if row.Body != nil {
		preview = *row.Body
	}
	pushTitle := row.SenderName
	if c.Kind == "internal" && c.Title != nil {
		pushTitle = row.SenderName + " · " + *c.Title
	}
	route := "/chat/" + c.ID.String()
	for _, u := range push {
		if err := notifications.EnqueueUserPush(ctx, q, tenantID, notifications.UserPush{
			UserID: u, Type: notifications.TypeChatMessage, Title: pushTitle, Body: preview,
			Data: map[string]string{"app_route": route, "route": "/chat?c=" + c.ID.String(), "conversation_id": c.ID.String()},
		}); err != nil {
			return ChatMessage{}, err
		}
	}
	if c.Kind == "guest" && from.kind == "user" && guestInside && c.ReservationID != nil {
		if err := guestaccess.PushGuest(ctx, q, tenantID, *c.ReservationID, row.SenderName, preview, "/guest/chat"); err != nil {
			return ChatMessage{}, err
		}
	}
	return mine, nil
}

// Read marca la conversacion como leida para el actor.
func (s *Service) Read(ctx context.Context, actor authz.Actor, id uuid.UUID) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		c, err := load(ctx, q, actor, id, false)
		if err != nil {
			return err
		}
		if !c.Member {
			if err := join(ctx, q, id, actor.UserID); err != nil {
				return err
			}
		}
		_, err = q.Exec(ctx, `UPDATE conversation_participants SET last_read_at = $3 WHERE conversation_id = $1 AND user_id = $2`,
			id, actor.UserID, s.now())
		return err
	})
}

// Mute silencia (o no) el push de la conversacion para el actor.
func (s *Service) Mute(ctx context.Context, actor authz.Actor, id uuid.UUID, muted bool) (Conversation, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Conversation, error) {
		c, err := load(ctx, q, actor, id, false)
		if err != nil {
			return Conversation{}, err
		}
		if !c.Member {
			if err := join(ctx, q, id, actor.UserID); err != nil {
				return Conversation{}, err
			}
		}
		if _, err := q.Exec(ctx, `UPDATE conversation_participants SET muted = $3 WHERE conversation_id = $1 AND user_id = $2`,
			id, actor.UserID, muted); err != nil {
			return Conversation{}, err
		}
		return s.get(ctx, q, actor, id)
	})
}

// AddParticipant suma a alguien a un grupo interno. Puede hacerlo un
// participante, o quien administra usuarios (tambien para unirse el mismo).
func (s *Service) AddParticipant(ctx context.Context, actor authz.Actor, id uuid.UUID, in ParticipantAdd) (Conversation, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Conversation, error) {
		admin := actor.Permissions[authz.UsersManage]
		rows, err := q.Query(ctx, convSelect+` WHERE c.id = $2`, actor.UserID, id)
		if err != nil {
			return Conversation{}, err
		}
		c, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[convRow])
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !c.Member && !admin) {
			return Conversation{}, ErrNotFound
		}
		if err != nil {
			return Conversation{}, err
		}
		if c.Kind != "internal" {
			return Conversation{}, ErrGuestOnly
		}
		var n int
		if err := q.QueryRow(ctx, `
			SELECT count(*) FROM shared.memberships m JOIN shared.users u ON u.id = m.user_id AND u.is_active
			WHERE m.tenant_id = $1 AND m.status = 'active' AND m.user_id = $2`, actor.TenantID, in.UserID).Scan(&n); err != nil {
			return Conversation{}, err
		}
		if n == 0 {
			return Conversation{}, ErrUnknownUser.WithField("body.user_id")
		}
		tag, err := q.Exec(ctx, `
			INSERT INTO conversation_participants (conversation_id, participant_type, user_id)
			VALUES ($1, 'user', $2) ON CONFLICT (conversation_id, user_id) WHERE user_id IS NOT NULL DO NOTHING`, id, in.UserID)
		if err != nil {
			return Conversation{}, err
		}
		if tag.RowsAffected() == 0 {
			return Conversation{}, ErrAlreadyThere
		}
		// Un grupo de dos que crece deja de ser 1:1: le ponemos nombre.
		if _, err := q.Exec(ctx, `UPDATE conversations SET updated_at = now() WHERE id = $1`, id); err != nil {
			return Conversation{}, err
		}
		return s.get(ctx, q, actor, id)
	})
}

// RemoveParticipant saca a alguien de un grupo interno: uno mismo, quien lo
// creo o quien administra usuarios.
func (s *Service) RemoveParticipant(ctx context.Context, actor authz.Actor, id, user uuid.UUID) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		c, err := load(ctx, q, actor, id, false)
		if err != nil {
			return err
		}
		if c.Kind != "internal" {
			return ErrGuestOnly
		}
		var createdBy *uuid.UUID
		if err := q.QueryRow(ctx, `SELECT created_by FROM conversations WHERE id = $1`, id).Scan(&createdBy); err != nil {
			return err
		}
		if user != actor.UserID && !actor.Permissions[authz.UsersManage] && (createdBy == nil || *createdBy != actor.UserID) {
			return ErrNotAllowed
		}
		tag, err := q.Exec(ctx, `DELETE FROM conversation_participants WHERE conversation_id = $1 AND user_id = $2`, id, user)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotMember
		}
		return nil
	})
}

// ── Huesped ───────────────────────────────────────────────

// Ensure crea (si falta) la conversacion de la reserva con el huesped
// principal, quienes tienen chat.guest y un saludo. Devuelve su id.
func Ensure(ctx context.Context, q database.Querier, tenantID, reservationID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	var guestID *uuid.UUID
	var guestName, tenantName string
	var propertyID uuid.UUID
	err := q.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO conversations (kind, title, reservation_id)
			SELECT 'guest', coalesce(g.full_name, r.code), r.id
			FROM reservations r LEFT JOIN guests g ON g.id = r.primary_guest_id
			WHERE r.id = $1
			ON CONFLICT (reservation_id) WHERE kind = 'guest' DO NOTHING
			RETURNING id)
		SELECT ins.id, r.primary_guest_id, coalesce(g.full_name, ''), r.property_id,
		       (SELECT name FROM shared.tenants WHERE id = $2)
		FROM ins, reservations r LEFT JOIN guests g ON g.id = r.primary_guest_id
		WHERE r.id = $1`, reservationID, tenantID).Scan(&id, &guestID, &guestName, &propertyID, &tenantName)
	if errors.Is(err, pgx.ErrNoRows) {
		// Ya existia (o la reserva no existe).
		err = q.QueryRow(ctx, `SELECT id FROM conversations WHERE reservation_id = $1 AND kind = 'guest'`, reservationID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrNotFound
		}
		if err != nil {
			return uuid.Nil, err
		}
		_, err = q.Exec(ctx, `
			INSERT INTO conversation_participants (conversation_id, participant_type, guest_id)
			SELECT $1, 'guest', primary_guest_id FROM reservations WHERE id = $2 AND primary_guest_id IS NOT NULL
			ON CONFLICT (conversation_id, guest_id) WHERE guest_id IS NOT NULL DO NOTHING`, id, reservationID)
		return id, err
	}
	if err != nil {
		return uuid.Nil, err
	}
	if guestID != nil {
		if _, err := q.Exec(ctx, `INSERT INTO conversation_participants (conversation_id, participant_type, guest_id) VALUES ($1, 'guest', $2)`,
			id, *guestID); err != nil {
			return uuid.Nil, err
		}
	}
	staff, err := notifications.Holders(ctx, q, tenantID, &propertyID, authz.ChatGuest)
	if err != nil {
		return uuid.Nil, err
	}
	if len(staff) > 0 {
		if _, err := q.Exec(ctx, `
			INSERT INTO conversation_participants (conversation_id, participant_type, user_id, last_read_at)
			SELECT $1, 'user', u, now() FROM unnest($2::uuid[]) AS u
			ON CONFLICT (conversation_id, user_id) WHERE user_id IS NOT NULL DO NOTHING`, id, staff); err != nil {
			return uuid.Nil, err
		}
	}
	hello := fmt.Sprintf("¡Hola%s! Este es el chat con %s. Escríbenos lo que necesites durante tu estancia; te respondemos por aquí.",
		firstName(guestName), tenantName)
	if _, err := q.Exec(ctx, `
		INSERT INTO messages (conversation_id, sender_type, body) VALUES ($1, 'system', $2)`, id, hello); err != nil {
		return uuid.Nil, err
	}
	_, err = q.Exec(ctx, `UPDATE conversations SET last_message_at = now() WHERE id = $1`, id)
	return id, err
}

func firstName(full string) string {
	f := strings.Fields(full)
	if len(f) == 0 {
		return ""
	}
	return ", " + f[0]
}

// GuestView es la conversacion del huesped (la crea si hace falta).
func (s *Service) GuestView(ctx context.Context, g Guest) (GuestChat, error) {
	if !g.Enabled {
		return GuestChat{}, ErrFeature
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (GuestChat, error) {
		id, err := Ensure(ctx, q, g.TenantID, g.ReservationID)
		if err != nil {
			return GuestChat{}, err
		}
		var archived bool
		var lastRead *time.Time
		if err := q.QueryRow(ctx, `
			SELECT c.is_archived, cp.last_read_at FROM conversations c
			LEFT JOIN conversation_participants cp ON cp.conversation_id = c.id AND cp.guest_id = $2
			WHERE c.id = $1`, id, g.GuestID).Scan(&archived, &lastRead); err != nil {
			return GuestChat{}, err
		}
		page, err := s.page(ctx, q, g.TenantID, id, "", 100, guestViewer(g.GuestID))
		if err != nil {
			return GuestChat{}, err
		}
		unread := 0
		for _, m := range page.Items {
			if !m.Mine && (lastRead == nil || m.CreatedAt.After(*lastRead)) {
				unread++
			}
		}
		people, err := participants(ctx, q, g.TenantID, []uuid.UUID{id})
		if err != nil {
			return GuestChat{}, err
		}
		staff := []ChatParticipant{}
		for _, p := range people[id] {
			if p.Type == "user" {
				staff = append(staff, p)
			}
		}
		return GuestChat{ID: id, Title: g.TenantName, Participants: staff, Messages: page.Items, Unread: unread, IsArchived: archived}, nil
	})
}

// GuestSend manda un mensaje del huesped (solo texto).
func (s *Service) GuestSend(ctx context.Context, g Guest, in MessageCreate) (ChatMessage, error) {
	if !g.Enabled {
		return ChatMessage{}, ErrFeature
	}
	body := strings.TrimSpace(in.Body)
	if body == "" {
		return ChatMessage{}, ErrEmpty
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (ChatMessage, error) {
		id, err := Ensure(ctx, q, g.TenantID, g.ReservationID)
		if err != nil {
			return ChatMessage{}, err
		}
		rows, err := q.Query(ctx, convSelect+` WHERE c.id = $2 FOR UPDATE OF c`, uuid.Nil, id)
		if err != nil {
			return ChatMessage{}, err
		}
		c, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[convRow])
		if err != nil {
			return ChatMessage{}, err
		}
		if c.IsArchived {
			return ChatMessage{}, ErrArchived
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO conversation_participants (conversation_id, participant_type, guest_id) VALUES ($1, 'guest', $2)
			ON CONFLICT (conversation_id, guest_id) WHERE guest_id IS NOT NULL DO NOTHING`, id, g.GuestID); err != nil {
			return ChatMessage{}, err
		}
		var msgID uuid.UUID
		if err := q.QueryRow(ctx, `
			INSERT INTO messages (conversation_id, sender_type, sender_guest_id, body, created_at)
			VALUES ($1, 'guest', $2, $3, $4) RETURNING id`, id, g.GuestID, body, s.now()).Scan(&msgID); err != nil {
			return ChatMessage{}, err
		}
		return s.deliver(ctx, q, g.TenantID, c, msgID, guestViewer(g.GuestID))
	})
}

// GuestRead marca como leido lo que el huesped ya vio.
func (s *Service) GuestRead(ctx context.Context, g Guest) error {
	if !g.Enabled {
		return ErrFeature
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		id, err := Ensure(ctx, q, g.TenantID, g.ReservationID)
		if err != nil {
			return err
		}
		_, err = q.Exec(ctx, `UPDATE conversation_participants SET last_read_at = $3 WHERE conversation_id = $1 AND guest_id = $2`,
			id, g.GuestID, s.now())
		return err
	})
}

// ── Reservas y trabajos ───────────────────────────────────

// Subscribe crea la conversacion al confirmar y la archiva al cancelar.
func (s *Service) Subscribe(bus *events.Bus) {
	bus.Subscribe(reservations.EventConfirmed, s.onConfirmed)
	bus.Subscribe(reservations.EventCancelled, s.onCancelled)
}

func reservationOf(e events.Event) (reservations.ReservationEvent, bool) {
	switch d := e.Data.(type) {
	case reservations.ReservationEvent:
		return d, true
	case *reservations.ReservationEvent:
		if d != nil {
			return *d, true
		}
	}
	return reservations.ReservationEvent{}, false
}

// Enabled dice si la empresa tiene la bandera guest_chat.
func Enabled(ctx context.Context, q database.Querier, tenantID uuid.UUID) (bool, error) {
	var on bool
	err := q.QueryRow(ctx, `SELECT coalesce((features->>'guest_chat')::bool, false) FROM shared.tenants WHERE id = $1`, tenantID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return on, err
}

func (s *Service) onConfirmed(ctx context.Context, q database.Querier, e events.Event) error {
	ev, ok := reservationOf(e)
	if !ok {
		return nil
	}
	on, err := Enabled(ctx, q, e.TenantID)
	if err != nil || !on {
		return err
	}
	_, err = Ensure(ctx, q, e.TenantID, ev.Reservation.ID)
	return err
}

func (s *Service) onCancelled(ctx context.Context, q database.Querier, e events.Event) error {
	ev, ok := reservationOf(e)
	if !ok {
		return nil
	}
	_, err := q.Exec(ctx, `
		UPDATE conversations SET is_archived = true, archived_at = now(), updated_at = now()
		WHERE reservation_id = $1 AND kind = 'guest' AND NOT is_archived`, ev.Reservation.ID)
	return err
}

// Archive archiva las conversaciones de huespedes que salieron hace mas de
// 24 h (o cuya reserva se cancelo). Devuelve cuantas.
func Archive(ctx context.Context, q database.Querier, now time.Time) (int, error) {
	tag, err := q.Exec(ctx, `
		UPDATE conversations c SET is_archived = true, archived_at = $1, updated_at = now()
		FROM reservations r
		WHERE r.id = c.reservation_id AND c.kind = 'guest' AND NOT c.is_archived
		  AND (r.status IN ('cancelled', 'no_show')
		       OR (r.status = 'checked_out' AND coalesce(r.checked_out_at, r.updated_at) < $1::timestamptz - interval '24 hours'))`, now)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// HandleJob corre chat_archive (una vez por empresa).
func (s *Service) HandleJob(ctx context.Context, job jobs.Running) error {
	if job.Type != JobArchive {
		return fmt.Errorf("trabajo desconocido %s", job.Type)
	}
	if job.TenantID == nil {
		return s.db.InTx(ctx, func(q database.Querier) error {
			rows, err := q.Query(ctx, `SELECT id FROM shared.tenants WHERE is_active ORDER BY slug`)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := jobs.Enqueue(ctx, q, jobs.Job{
					Type: JobArchive, TenantID: &id, DedupeKey: JobArchive + ":" + id.String(), MaxAttempts: 3,
				}); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		n, err := Archive(ctx, q, s.now())
		if n > 0 {
			s.log.InfoContext(ctx, "Conversaciones de huespedes archivadas", "count", n)
		}
		return err
	})
}
