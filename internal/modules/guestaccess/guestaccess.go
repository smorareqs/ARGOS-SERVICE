// Package guestaccess es la entrada del huesped a la app (fase04.md §3-5,
// Persona B): enlace magico y codigo, su sesion y lo que puede hacer con ella.
//
//   - Issue (reservations.SetGuestAccess) corre al confirmar o reenviar la
//     reserva: revoca los accesos anteriores y crea dos nuevos, el enlace
//     argos://guest?token=... y un codigo de 8 caracteres que se usa con el
//     correo del huesped. Solo se guarda el hash; vencen 24 h despues de la
//     salida.
//   - Auth cambia el enlace (o codigo + correo) por un JWT typ=guest con la
//     empresa, la reserva, el huesped y el acceso. Dura lo mismo que el acceso,
//     sin refresh: se vuelve a entrar con el mismo codigo.
//   - Resolve valida ese JWT en cada peticion: firma, acceso no revocado ni
//     vencido y reserva viva. El alcance del huesped es exactamente su reserva
//     y la propiedad de esa reserva.
package guestaccess

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/ratelimit"
	"github.com/smorareqs/argos-service/internal/core/security"
	"github.com/smorareqs/argos-service/internal/modules/maintenance"
	"github.com/smorareqs/argos-service/internal/modules/notifications"
	"github.com/smorareqs/argos-service/internal/modules/parking"
	"github.com/smorareqs/argos-service/internal/modules/reservations"
	"github.com/smorareqs/argos-service/internal/modules/sharedareas"
)

// TokenGuest es el tipo del JWT del huesped.
const TokenGuest security.TokenType = "guest"

// LinkPrefix es el enlace magico que abre la app.
const LinkPrefix = "argos://guest?token="

// Errores del modulo.
var (
	ErrInvalidAccess = apperr.New(apperr.KindAuthentication, "El enlace o el código ya no sirve. Pide uno nuevo a la recepción.")
	ErrCredentials   = apperr.New(apperr.KindAuthentication, "El código o el correo no coinciden con una reserva activa.")
	ErrMissing       = apperr.New(apperr.KindValidation, "Escribe tu código y tu correo, o abre el enlace del correo.")
	ErrTooMany       = apperr.New(apperr.KindTooManyRequests, "Demasiados intentos. Espera un minuto e inténtalo de nuevo.")
	ErrFeature       = apperr.New(apperr.KindPermissionDenied, "Esta función no está activa para tu estancia.")
	ErrAreaNotFound  = apperr.New(apperr.KindNotFound, "El área no existe.")
)

// Principal es el huesped de la peticion.
type Principal struct {
	TenantID      uuid.UUID
	TenantSchema  string
	TenantName    string
	ReservationID uuid.UUID
	GuestID       uuid.UUID
	AccessID      uuid.UUID
	ExpiresAt     time.Time
	Features      map[string]bool
}

// GuestAuthInput es el cuerpo de POST /guest/auth.
type GuestAuthInput struct {
	_     struct{} `additionalProperties:"true"`
	Token string   `json:"token,omitempty" maxLength:"128" doc:"El token del enlace argos://guest?token=..."`
	Code  string   `json:"code,omitempty" maxLength:"16" doc:"El codigo del correo (junto con email)."`
	Email string   `json:"email,omitempty" maxLength:"320"`
}

// GuestSession es la respuesta de POST /guest/auth.
type GuestSession struct {
	AccessToken string    `json:"access_token"`
	TokenType   string    `json:"token_type" example:"bearer"`
	ExpiresAt   time.Time `json:"expires_at" doc:"Hasta 24 h despues de la salida. Sin refresh."`
	Me          GuestMe   `json:"me"`
}

// Me es GET /guest/me.
type GuestMe struct {
	GuestID         uuid.UUID       `json:"guest_id" format:"uuid"`
	Name            string          `json:"name"`
	Email           *string         `json:"email" nullable:"true"`
	TenantName      string          `json:"tenant_name"`
	ReservationID   uuid.UUID       `json:"reservation_id" format:"uuid"`
	ReservationCode string          `json:"reservation_code"`
	ExpiresAt       time.Time       `json:"expires_at"`
	Features        map[string]bool `json:"features" doc:"parking, shared_areas, guest_chat."`
}

// GuestReservation es GET /guest/reservation: lo que el huesped ve de su estancia.
type GuestReservation struct {
	ID              uuid.UUID `json:"id" format:"uuid"`
	Code            string    `json:"code"`
	Status          string    `json:"status"`
	PropertyCode    string    `json:"property_code"`
	PropertyName    string    `json:"property_name"`
	PropertyAddress *string   `json:"property_address" nullable:"true"`
	Building        *string   `json:"building" nullable:"true"`
	Lat             *float64  `json:"lat" nullable:"true"`
	Lng             *float64  `json:"lng" nullable:"true"`
	CheckInDate     string    `json:"check_in_date" format:"date"`
	CheckOutDate    string    `json:"check_out_date" format:"date"`
	CheckInTime     string    `json:"check_in_time"`
	CheckOutTime    string    `json:"check_out_time"`
	Nights          int       `json:"nights"`
	Adults          int       `json:"adults"`
	Children        int       `json:"children"`
	GuestName       string    `json:"guest_name"`
	Instructions    string    `json:"instructions" doc:"Lo que publica la empresa en Configuracion."`
	ContactName     string    `json:"contact_name"`
	ContactPhone    *string   `json:"contact_phone" nullable:"true"`
	ContactEmail    *string   `json:"contact_email" nullable:"true"`
	ParkingCount    int       `json:"parking_count"`
	BookingsCount   int       `json:"bookings_count"`
}

// GuestAreas es GET /guest/shared-areas.
type GuestAreas struct {
	Areas    []sharedareas.AreaRead    `json:"areas"`
	Bookings []sharedareas.BookingRead `json:"bookings" doc:"Mis bloques confirmados."`
}

// GuestTicketInput es el cuerpo de POST /guest/tickets.
type GuestTicketInput struct {
	_           struct{} `additionalProperties:"true"`
	Title       string   `json:"title" minLength:"3" maxLength:"160"`
	Description string   `json:"description,omitempty" maxLength:"4000"`
	Urgency     string   `json:"urgency,omitempty" enum:"low,medium,high,critical"`
}

// GuestTicketCreated es la respuesta de POST /guest/tickets.
type GuestTicketCreated struct {
	ID     uuid.UUID `json:"id" format:"uuid"`
	Number int       `json:"number"`
	Title  string    `json:"title"`
}

// GuestDeviceInput es el cuerpo de POST /guest/device.
type GuestDeviceInput struct {
	_     struct{} `additionalProperties:"true"`
	Token string   `json:"token" minLength:"10" maxLength:"512" doc:"Token FCM del telefono."`
}

// Service contiene los casos de uso del huesped.
type Service struct {
	db      *database.DB
	tokens  *security.TokenManager
	maint   *maintenance.Service
	areas   *sharedareas.Service
	limiter *ratelimit.Limiter
	log     *slog.Logger
	now     func() time.Time
}

// NewService crea el servicio.
func NewService(db *database.DB, tokens *security.TokenManager, maint *maintenance.Service, areas *sharedareas.Service, log *slog.Logger) *Service {
	return &Service{db: db, tokens: tokens, maint: maint, areas: areas, limiter: ratelimit.PerMinute(10), log: log, now: time.Now}
}

// SetClock cambia el reloj (pruebas).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// SetLimiter cambia el limite de intentos de entrada (pruebas).
func (s *Service) SetLimiter(l *ratelimit.Limiter) { s.limiter = l }

func hash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func randomCode(n int) (string, error) {
	var b strings.Builder
	for range n {
		i, err := rand.Int(rand.Reader, big.NewInt(int64(len(codeAlphabet))))
		if err != nil {
			return "", err
		}
		b.WriteByte(codeAlphabet[i.Int64()])
	}
	return b.String(), nil
}

// normalizeCode quita espacios y guiones y pasa a mayusculas.
func normalizeCode(c string) string {
	return strings.NewReplacer(" ", "", "-", "").Replace(strings.ToUpper(strings.TrimSpace(c)))
}

// Issue es el GuestAccessIssuer de reservas: revoca los accesos anteriores
// del huesped en esa reserva y crea el enlace y el codigo nuevos.
func (s *Service) Issue(ctx context.Context, q database.Querier, r reservations.ReservationRead, g reservations.GuestRead) (*reservations.AccessInfo, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	code, err := randomCode(8)
	if err != nil {
		return nil, err
	}
	if _, err := q.Exec(ctx, `
		UPDATE guest_access_tokens SET revoked_at = now() WHERE reservation_id = $1 AND guest_id = $2 AND revoked_at IS NULL`,
		r.ID, g.ID); err != nil {
		return nil, err
	}
	if _, err := q.Exec(ctx, `
		WITH t AS (SELECT timezone FROM shared.tenants WHERE schema_name = current_schema())
		INSERT INTO guest_access_tokens (reservation_id, guest_id, token_hash, kind, expires_at)
		SELECT $1, $2, x.hash, x.kind, ((r.check_out_date + r.check_out_time) AT TIME ZONE t.timezone) + interval '24 hours'
		FROM reservations r, t, (VALUES ($3, 'magic'), ($4, 'code')) AS x(hash, kind)
		WHERE r.id = $1`, r.ID, g.ID, hash(token), hash(code)); err != nil {
		return nil, err
	}
	return &reservations.AccessInfo{Link: LinkPrefix + token, Code: code[:4] + "-" + code[4:]}, nil
}

// DemoCode da a una reserva un codigo fijo de entrada para el huesped
// principal (solo la semilla de demostracion; nunca en produccion). Devuelve
// el correo con el que se entra.
func DemoCode(ctx context.Context, q database.Querier, reservationID uuid.UUID, code string) (string, error) {
	var email string
	err := q.QueryRow(ctx, `
		WITH t AS (SELECT timezone FROM shared.tenants WHERE schema_name = current_schema())
		INSERT INTO guest_access_tokens (reservation_id, guest_id, token_hash, kind, expires_at)
		SELECT r.id, r.primary_guest_id, $2, 'code', ((r.check_out_date + r.check_out_time) AT TIME ZONE t.timezone) + interval '24 hours'
		FROM reservations r, t WHERE r.id = $1 AND r.primary_guest_id IS NOT NULL
		ON CONFLICT (token_hash) DO NOTHING
		RETURNING (SELECT email FROM guests WHERE id = guest_access_tokens.guest_id)`,
		reservationID, hash(normalizeCode(code))).Scan(&email)
	return email, err
}

// Auth cambia el enlace o el codigo y correo por la sesion del huesped. La
// empresa no viene en la peticion: se busca el acceso en las empresas con la
// bandera reservations (por hash, con indice unico).
func (s *Service) Auth(ctx context.Context, ip string, in GuestAuthInput) (GuestSession, error) {
	token, code, email := strings.TrimSpace(in.Token), normalizeCode(in.Code), strings.ToLower(strings.TrimSpace(in.Email))
	if token == "" && (code == "" || email == "") {
		return GuestSession{}, ErrMissing
	}
	if ok, retry := s.limiter.Allow("guest-auth:" + ip); !ok {
		return GuestSession{}, ErrTooMany.WithRetryAfter(retry)
	}
	kind, secret := "magic", token
	if token == "" {
		kind, secret = "code", code
	}
	type tenant struct {
		ID     uuid.UUID
		Schema string
	}
	var list []tenant
	if err := s.db.InTx(ctx, func(q database.Querier) error {
		rows, err := q.Query(ctx, `
			SELECT id, schema_name FROM shared.tenants
			WHERE is_active AND coalesce((features->>'reservations')::bool, false) ORDER BY created_at`)
		if err != nil {
			return err
		}
		list, err = pgx.CollectRows(rows, pgx.RowToStructByPos[tenant])
		return err
	}); err != nil {
		return GuestSession{}, err
	}
	for _, t := range list {
		tctx := database.WithTenantSchema(ctx, t.Schema)
		var accessID uuid.UUID
		var expires time.Time
		err := s.db.InTx(tctx, func(q database.Querier) error {
			err := q.QueryRow(tctx, `
				UPDATE guest_access_tokens a SET last_used_at = now()
				FROM reservations r, guests g
				WHERE a.token_hash = $1 AND a.kind = $2 AND a.revoked_at IS NULL AND a.expires_at > $4
				  AND r.id = a.reservation_id AND r.status NOT IN ('cancelled', 'no_show')
				  AND g.id = a.guest_id AND ($2 = 'magic' OR lower(g.email) = $3)
				RETURNING a.id, a.expires_at`, hash(secret), kind, email, s.now()).Scan(&accessID, &expires)
			return err
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return GuestSession{}, err
		}
		return s.session(tctx, t.ID, t.Schema, accessID, expires)
	}
	if kind == "magic" {
		return GuestSession{}, ErrInvalidAccess
	}
	return GuestSession{}, ErrCredentials
}

func (s *Service) session(ctx context.Context, tenantID uuid.UUID, schema string, accessID uuid.UUID, expires time.Time) (GuestSession, error) {
	p, err := s.principal(ctx, tenantID, schema, accessID)
	if err != nil {
		return GuestSession{}, err
	}
	ttl := expires.Sub(s.now())
	jwt, err := s.tokens.CreateToken(p.GuestID.String(), TokenGuest, ttl, map[string]any{
		"tid": tenantID.String(), "rid": p.ReservationID.String(), "aid": accessID.String(),
	})
	if err != nil {
		return GuestSession{}, err
	}
	me, err := s.Me(ctx, p)
	if err != nil {
		return GuestSession{}, err
	}
	return GuestSession{AccessToken: jwt, TokenType: "bearer", ExpiresAt: expires, Me: me}, nil
}

// principal arma el huesped de un acceso vigente (ctx con el schema).
func (s *Service) principal(ctx context.Context, tenantID uuid.UUID, schema string, accessID uuid.UUID) (Principal, error) {
	p := Principal{TenantID: tenantID, TenantSchema: schema, AccessID: accessID, Features: map[string]bool{}}
	err := s.db.InTx(ctx, func(q database.Querier) error {
		var features map[string]any
		err := q.QueryRow(ctx, `
			SELECT a.reservation_id, a.guest_id, a.expires_at, t.name, t.features
			FROM guest_access_tokens a
			JOIN reservations r ON r.id = a.reservation_id
			JOIN shared.tenants t ON t.id = $2 AND t.schema_name = current_schema() AND t.is_active
			WHERE a.id = $1 AND a.revoked_at IS NULL AND a.expires_at > $3 AND r.status NOT IN ('cancelled', 'no_show')`,
			accessID, tenantID, s.now()).Scan(&p.ReservationID, &p.GuestID, &p.ExpiresAt, &p.TenantName, &features)
		for k, v := range features {
			if b, ok := v.(bool); ok && b {
				p.Features[k] = true
			}
		}
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrInvalidAccess
	}
	return p, err
}

// Resolve valida el JWT del huesped y devuelve su principal (401 si el
// acceso se revoco, vencio o la reserva se cancelo).
func (s *Service) Resolve(ctx context.Context, token string) (Principal, error) {
	claims, err := s.tokens.Decode(token)
	if err != nil || claims["type"] != string(TokenGuest) {
		return Principal{}, ErrInvalidAccess
	}
	str := func(k string) string { v, _ := claims[k].(string); return v }
	tenantID, err1 := uuid.Parse(str("tid"))
	accessID, err2 := uuid.Parse(str("aid"))
	if err1 != nil || err2 != nil {
		return Principal{}, ErrInvalidAccess
	}
	var schema string
	if err := s.db.InTx(ctx, func(q database.Querier) error {
		return q.QueryRow(ctx, `SELECT schema_name FROM shared.tenants WHERE id = $1 AND is_active`, tenantID).Scan(&schema)
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Principal{}, ErrInvalidAccess
		}
		return Principal{}, err
	}
	p, err := s.principal(database.WithTenantSchema(ctx, schema), tenantID, schema, accessID)
	if err != nil {
		return Principal{}, err
	}
	if str("rid") != p.ReservationID.String() || str("sub") != p.GuestID.String() {
		return Principal{}, ErrInvalidAccess
	}
	return p, nil
}

// Me es lo que la app muestra arriba (ctx con el schema del huesped).
func (s *Service) Me(ctx context.Context, p Principal) (GuestMe, error) {
	me := GuestMe{GuestID: p.GuestID, TenantName: p.TenantName, ReservationID: p.ReservationID, ExpiresAt: p.ExpiresAt, Features: map[string]bool{}}
	for _, f := range []string{"parking", "shared_areas", "guest_chat"} {
		me.Features[f] = p.Features[f]
	}
	err := s.db.InTx(ctx, func(q database.Querier) error {
		return q.QueryRow(ctx, `
			SELECT g.full_name, g.email, r.code FROM guests g, reservations r WHERE g.id = $1 AND r.id = $2`,
			p.GuestID, p.ReservationID).Scan(&me.Name, &me.Email, &me.ReservationCode)
	})
	return me, err
}

// Reservation es la estancia del huesped con las instrucciones de la empresa.
func (s *Service) Reservation(ctx context.Context, p Principal) (GuestReservation, error) {
	var v GuestReservation
	err := s.db.InTx(ctx, func(q database.Querier) error {
		var text string
		err := q.QueryRow(ctx, `
			SELECT r.id, r.code, r.status, p.code, p.name, coalesce(p.address, b.address), b.name, coalesce(p.lat, b.lat)::float8, coalesce(p.lng, b.lng)::float8,
			       to_char(r.check_in_date, 'YYYY-MM-DD'), to_char(r.check_out_date, 'YYYY-MM-DD'),
			       to_char(r.check_in_time, 'HH24:MI'), to_char(r.check_out_time, 'HH24:MI'),
			       r.check_out_date - r.check_in_date, r.adults, r.children, g.full_name,
			       coalesce(t.settings->>'guest_instructions', ''), t.name,
			       nullif(coalesce(t.settings->>'contact_phone', t.settings->>'phone', ''), ''),
			       nullif(coalesce(t.settings->>'contact_email', ''), ''),
			       (SELECT count(*) FROM parking_assignments pa WHERE pa.reservation_id = r.id),
			       (SELECT count(*) FROM shared_area_bookings k WHERE k.reservation_id = r.id AND k.status = 'confirmed' AND k.ends_at > now())
			FROM reservations r
			JOIN properties p ON p.id = r.property_id
			LEFT JOIN buildings b ON b.id = p.building_id
			JOIN guests g ON g.id = $2
			JOIN shared.tenants t ON t.schema_name = current_schema()
			WHERE r.id = $1`, p.ReservationID, p.GuestID).Scan(
			&v.ID, &v.Code, &v.Status, &v.PropertyCode, &v.PropertyName, &v.PropertyAddress, &v.Building, &v.Lat, &v.Lng,
			&v.CheckInDate, &v.CheckOutDate, &v.CheckInTime, &v.CheckOutTime, &v.Nights, &v.Adults, &v.Children, &v.GuestName,
			&text, &v.ContactName, &v.ContactPhone, &v.ContactEmail, &v.ParkingCount, &v.BookingsCount)
		if err != nil {
			return err
		}
		first, _, _ := strings.Cut(v.GuestName, " ")
		v.Instructions = strings.TrimSpace(strings.NewReplacer(
			"{{huesped}}", first, "{{propiedad}}", v.PropertyName,
			"{{fecha_entrada}}", v.CheckInDate, "{{hora_entrada}}", v.CheckInTime,
		).Replace(text))
		return nil
	})
	return v, err
}

// Parking son los espacios de la estancia (bandera parking).
func (s *Service) Parking(ctx context.Context, p Principal) ([]parking.AssignmentRead, error) {
	if !p.Features["parking"] {
		return nil, ErrFeature
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]parking.AssignmentRead, error) {
		return parking.ForReservation(ctx, q, p.ReservationID)
	})
}

// stay es la reserva del huesped como la necesita areas comunes.
func stay(ctx context.Context, q database.Querier, p Principal) (sharedareas.Stay, error) {
	st := sharedareas.Stay{ReservationID: p.ReservationID, GuestID: &p.GuestID}
	err := q.QueryRow(ctx, `
		SELECT g.full_name, r.property_id, pr.building_id, to_char(r.check_in_date, 'YYYY-MM-DD'), to_char(r.check_out_date, 'YYYY-MM-DD')
		FROM reservations r JOIN properties pr ON pr.id = r.property_id JOIN guests g ON g.id = $2
		WHERE r.id = $1`, p.ReservationID, p.GuestID).Scan(&st.GuestName, &st.PropertyID, &st.BuildingID, &st.CheckIn, &st.CheckOut)
	return st, err
}

// Areas son las areas de su edificio y sus bloques (bandera shared_areas).
func (s *Service) Areas(ctx context.Context, p Principal) (GuestAreas, error) {
	if !p.Features["shared_areas"] {
		return GuestAreas{}, ErrFeature
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (GuestAreas, error) {
		st, err := stay(ctx, q, p)
		if err != nil {
			return GuestAreas{}, err
		}
		areas, err := sharedareas.AreasFor(ctx, q, st.PropertyID, st.BuildingID)
		if err != nil {
			return GuestAreas{}, err
		}
		bookings, err := sharedareas.ForReservation(ctx, q, p.ReservationID)
		return GuestAreas{Areas: areas, Bookings: bookings}, err
	})
}

// visibleArea confirma que el area es del edificio o la propiedad del huesped.
func visibleArea(ctx context.Context, q database.Querier, p Principal, areaID uuid.UUID) error {
	st, err := stay(ctx, q, p)
	if err != nil {
		return err
	}
	areas, err := sharedareas.AreasFor(ctx, q, st.PropertyID, st.BuildingID)
	if err != nil {
		return err
	}
	for _, a := range areas {
		if a.ID == areaID {
			return nil
		}
	}
	return ErrAreaNotFound
}

// AreaAvailability son los bloques del dia de un area de su edificio.
func (s *Service) Availability(ctx context.Context, p Principal, areaID uuid.UUID, date string) (sharedareas.AreaAvailability, error) {
	if !p.Features["shared_areas"] {
		return sharedareas.AreaAvailability{}, ErrFeature
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (sharedareas.AreaAvailability, error) {
		if err := visibleArea(ctx, q, p, areaID); err != nil {
			return sharedareas.AreaAvailability{}, err
		}
		return s.areas.Availability(ctx, q, areaID, date, true)
	})
}

// Book aparta un bloque y manda el push de confirmacion al telefono.
func (s *Service) Book(ctx context.Context, p Principal, areaID uuid.UUID, in sharedareas.GuestBookingCreate) (sharedareas.BookingRead, error) {
	if !p.Features["shared_areas"] {
		return sharedareas.BookingRead{}, ErrFeature
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (sharedareas.BookingRead, error) {
		if err := visibleArea(ctx, q, p, areaID); err != nil {
			return sharedareas.BookingRead{}, err
		}
		st, err := stay(ctx, q, p)
		if err != nil {
			return sharedareas.BookingRead{}, err
		}
		b, err := s.areas.BookTx(ctx, q, areaID, in.Date, in.Start, in.People, st, nil)
		if err != nil {
			return sharedareas.BookingRead{}, err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO reservation_events (reservation_id, type, actor_guest_id, data) VALUES ($1, 'area_booked', $2, $3)`,
			p.ReservationID, p.GuestID, map[string]any{"area": b.AreaName, "date": b.Date, "start": b.Start, "people": b.People}); err != nil {
			return sharedareas.BookingRead{}, err
		}
		return b, s.push(ctx, q, p, "Reserva confirmada", fmt.Sprintf("%s, %s de %s a %s.", b.AreaName, b.Date, b.Start, b.End), "/guest/areas")
	})
}

// Bookings son sus bloques confirmados.
func (s *Service) Bookings(ctx context.Context, p Principal) ([]sharedareas.BookingRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]sharedareas.BookingRead, error) {
		return sharedareas.ForReservation(ctx, q, p.ReservationID)
	})
}

// CancelBooking cancela uno de sus bloques hasta 1 h antes.
func (s *Service) CancelBooking(ctx context.Context, p Principal, id uuid.UUID) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		return s.areas.CancelForGuest(ctx, q, p.ReservationID, id)
	})
}

// Ticket reporta un problema en la propiedad de su reserva.
func (s *Service) Ticket(ctx context.Context, p Principal, in GuestTicketInput) (GuestTicketCreated, error) {
	urgency := in.Urgency
	if urgency == "" {
		urgency = "medium"
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (GuestTicketCreated, error) {
		var propertyID uuid.UUID
		if err := q.QueryRow(ctx, `SELECT property_id FROM reservations WHERE id = $1`, p.ReservationID).Scan(&propertyID); err != nil {
			return GuestTicketCreated{}, err
		}
		t, err := s.maint.ReportFromGuest(ctx, q, p.TenantID, propertyID, p.GuestID, in.Title, in.Description, urgency)
		if err != nil {
			return GuestTicketCreated{}, err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO reservation_events (reservation_id, type, actor_guest_id, data) VALUES ($1, 'guest_ticket', $2, $3)`,
			p.ReservationID, p.GuestID, map[string]any{"ticket_id": t.ID, "number": t.Number, "title": t.Title}); err != nil {
			return GuestTicketCreated{}, err
		}
		return GuestTicketCreated{ID: t.ID, Number: t.Number, Title: t.Title}, nil
	})
}

// Device guarda el token FCM del telefono del huesped.
func (s *Service) Device(ctx context.Context, p Principal, in GuestDeviceInput) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		_, err := q.Exec(ctx, `UPDATE guest_access_tokens SET fcm_token = $2 WHERE id = $1`, p.AccessID, strings.TrimSpace(in.Token))
		return err
	})
}

// push manda un push a los telefonos del huesped en esa reserva.
func (s *Service) push(ctx context.Context, q database.Querier, p Principal, title, body, route string) error {
	return PushGuest(ctx, q, p.TenantID, p.ReservationID, title, body, route)
}

// PushGuest encola un push a los telefonos registrados de una reserva.
func PushGuest(ctx context.Context, q database.Querier, tenantID, reservationID uuid.UUID, title, body, route string) error {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT fcm_token FROM guest_access_tokens
		WHERE reservation_id = $1 AND fcm_token IS NOT NULL AND revoked_at IS NULL AND expires_at > now()`, reservationID)
	if err != nil {
		return err
	}
	tokens, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, t := range tokens {
		if err := notifications.EnqueueGuestPush(ctx, q, tenantID, notifications.GuestPush{
			Token: t, Title: title, Body: body, Data: map[string]string{"app_route": route, "type": "guest"},
		}); err != nil {
			return err
		}
	}
	return nil
}
