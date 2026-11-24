// Package reservations es el modulo de reservas y hospitalidad de la fase 4
// (APP/docs/plans/fase04.md, parte de Persona A): canales de venta,
// calendarios iCal, huespedes con documento, reservas con su ciclo de vida,
// depositos e inspecciones antes y despues de la estancia.
//
// Huespedes, depositos e inspecciones viven en este paquete (y no en modulos
// aparte como sugeria el reparto) porque comparten la transaccion y el estado
// de la reserva: el check-in exige huespedes registrados y deposito recibido,
// y devolver el deposito exige la inspeccion de salida.
package reservations

import (
	"context"
	"crypto/rand"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/ical"
	"github.com/smorareqs/argos-service/internal/core/storage"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/maintenance"
)

// Eventos que publica el modulo. Data: ReservationEvent.
const (
	// EventConfirmed: la reserva quedo confirmada (al crearla con huesped o al
	// confirmarla). B crea accesos del huesped, programa alarmas y la limpieza
	// de salida.
	EventConfirmed = "reservation.confirmed"
	// EventUpdated: cambiaron fechas, horas o canal de una reserva viva (B
	// reprograma alarmas y la limpieza). Data.Previous trae la anterior.
	EventUpdated = "reservation.updated"
	// EventCancelled: se cancelo o no llego (Data.Reservation.Status dice cual).
	EventCancelled = "reservation.cancelled"
	// EventCheckedIn: el huesped entro (check-in formal o automatico).
	EventCheckedIn = "reservation.checked_in"
	// EventCheckedOut: el huesped salio (manual o por el trabajo de estados).
	EventCheckedOut = "reservation.checked_out"
)

// ReservationEvent es el Data de los eventos de reservas.
type ReservationEvent struct {
	Reservation ReservationRead
	// Previous es la reserva antes del cambio (solo en EventUpdated).
	Previous *ReservationRead
}

// AccessInfo es el acceso del huesped a la app que va en el correo.
type AccessInfo struct {
	Link string // argos://guest?token=...
	Code string // codigo para entrar con su correo
}

// GuestAccessIssuer emite (o renueva) el acceso del huesped principal a la
// app al confirmar. Lo registra el modulo guest_access de Persona B con
// SetGuestAccess y corre en la transaccion de la confirmacion. Sin el, el
// correo no trae enlace ni codigo de la app.
type GuestAccessIssuer func(ctx context.Context, q database.Querier, r ReservationRead, g GuestRead) (*AccessInfo, error)

// Errores del modulo.
var (
	ErrReservationNotFound = apperr.New(apperr.KindNotFound, "La reserva no existe.")
	ErrChannelNotFound     = apperr.New(apperr.KindNotFound, "El canal no existe.")
	ErrFeedNotFound        = apperr.New(apperr.KindNotFound, "El calendario no existe.")
	ErrGuestNotFound       = apperr.New(apperr.KindNotFound, "El huésped no existe.")
	ErrPropertyNotFound    = apperr.New(apperr.KindNotFound, "La propiedad no existe.")
	ErrInspectionNotFound  = apperr.New(apperr.KindNotFound, "La inspección no existe.")
	ErrFindingNotFound     = apperr.New(apperr.KindNotFound, "El hallazgo no existe.")
	ErrDocumentNotFound    = apperr.New(apperr.KindNotFound, "El documento no existe o ya se borró.")

	ErrChannelNameTaken    = apperr.New(apperr.KindValidation, "Ya hay un canal con ese nombre.")
	ErrUnknownChannel      = apperr.New(apperr.KindValidation, "Ese canal no existe o está inactivo.")
	ErrFeedTaken           = apperr.New(apperr.KindValidation, "La propiedad ya tiene un calendario de ese canal.")
	ErrFeedURL             = apperr.New(apperr.KindValidation, "Pega la dirección https:// que te da el canal para exportar el calendario.")
	ErrInvalidDate         = apperr.New(apperr.KindValidation, "La fecha no es válida.")
	ErrDatesOrder          = apperr.New(apperr.KindValidation, "La salida debe ser después de la entrada.")
	ErrInvalidTime         = apperr.New(apperr.KindValidation, "Usa el formato HH:MM.")
	ErrGuestRequired       = apperr.New(apperr.KindValidation, "Indica el huésped principal.")
	ErrGuestBlacklisted    = apperr.New(apperr.KindValidation, "Este huésped está en lista negra.")
	ErrDocumentTaken       = apperr.New(apperr.KindValidation, "Ya hay un huésped con ese documento.")
	ErrDocumentPair        = apperr.New(apperr.KindValidation, "Indica el tipo y el número del documento.")
	ErrInvalidDocument     = apperr.New(apperr.KindValidation, "El número de documento no es válido.")
	ErrInvalidFiles        = apperr.New(apperr.KindValidation, "Alguna foto no se subió bien. Vuelve a adjuntarla.")
	ErrTransition          = apperr.New(apperr.KindValidation, "La reserva no está en un estado que permita esto.")
	ErrReasonRequired      = apperr.New(apperr.KindValidation, "Escribe el motivo.")
	ErrTooEarly            = apperr.New(apperr.KindValidation, "Todavía no es la fecha de entrada.")
	ErrGuestsPending       = apperr.New(apperr.KindValidation, "Faltan datos de huéspedes para el check-in.")
	ErrDepositPending      = apperr.New(apperr.KindValidation, "Registra el depósito antes del check-in.")
	ErrNoEmail             = apperr.New(apperr.KindValidation, "El huésped principal no tiene correo.")
	ErrPrimaryGuest        = apperr.New(apperr.KindValidation, "No se puede quitar al huésped principal; marca a otro como principal primero.")
	ErrNoDeposit           = apperr.New(apperr.KindValidation, "Esta reserva no lleva depósito.")
	ErrDepositExceeds      = apperr.New(apperr.KindValidation, "El monto supera lo que queda del depósito.")
	ErrNeedsInspection     = apperr.New(apperr.KindValidation, "Primero registra la inspección de salida.")
	ErrChargesFirst        = apperr.New(apperr.KindValidation, "La inspección tiene cargos al depósito: registra primero lo que se retiene.")
	ErrNoCharges           = apperr.New(apperr.KindValidation, "Para retener, la inspección de salida debe tener un hallazgo con cargo al depósito.")
	ErrInspectionTaken     = apperr.New(apperr.KindValidation, "Esta reserva ya tiene esa inspección.")
	ErrInspectionTiming    = apperr.New(apperr.KindValidation, "La inspección previa es antes del check-in y la de salida después del check-out.")
	ErrDepositSettled      = apperr.New(apperr.KindValidation, "El depósito ya se liquidó; no se pueden cambiar los hallazgos.")
	ErrBlacklistPermission = apperr.New(apperr.KindPermissionDenied, "Solo quien administra huéspedes cambia la lista negra.")
	ErrOverlapGeneric      = apperr.New(apperr.KindValidation, "Las fechas se cruzan con otra reserva de la propiedad.")
)

// Service contiene los casos de uso de reservas.
type Service struct {
	db          *database.DB
	storage     *storage.Service
	maintenance *maintenance.Service
	bus         *events.Bus
	log         *slog.Logger
	access      GuestAccessIssuer
	fetcher     *ical.Fetcher
	webBase     string
	now         func() time.Time
}

// Options configura el servicio.
type Options struct {
	// AllowPrivateFeeds deja leer calendarios en direcciones privadas
	// (localhost, red interna). Solo en local y pruebas.
	AllowPrivateFeeds bool
	// WebBaseURL arma los enlaces al panel en los correos (fase 6).
	WebBaseURL string
}

// NewService crea el servicio.
func NewService(db *database.DB, st *storage.Service, maint *maintenance.Service, bus *events.Bus, log *slog.Logger, opts Options) *Service {
	return &Service{
		db: db, storage: st, maintenance: maint, bus: bus, log: log, now: time.Now,
		fetcher: ical.NewFetcher(opts.AllowPrivateFeeds), webBase: strings.TrimRight(opts.WebBaseURL, "/"),
	}
}

// SetGuestAccess registra la emision de accesos del huesped (Persona B).
func (s *Service) SetGuestAccess(fn GuestAccessIssuer) {
	s.access = fn
}

// SetClock cambia el reloj (pruebas).
func (s *Service) SetClock(now func() time.Time) {
	s.now = now
}

// --- Contexto de la empresa ----------------------------------------------------

// tenantInfo son los datos de la empresa del schema activo que usan las
// reservas: zona horaria, moneda y horas por defecto.
type tenantInfo struct {
	ID           uuid.UUID
	Name         string
	Timezone     string
	Currency     string
	CheckinTime  string
	CheckoutTime string
	Instructions string
	Today        string
}

func (s *Service) tenant(ctx context.Context, q database.Querier) (tenantInfo, error) {
	var t tenantInfo
	err := q.QueryRow(ctx, `
		SELECT id, name, timezone, trim(currency),
		       coalesce(nullif(settings->>'checkin_time', ''), '15:00'),
		       coalesce(nullif(settings->>'checkout_time', ''), '11:00'),
		       coalesce(settings->>'guest_instructions', ''),
		       to_char(($1::timestamptz AT TIME ZONE timezone)::date, 'YYYY-MM-DD')
		FROM shared.tenants WHERE schema_name = current_schema()`, s.now()).
		Scan(&t.ID, &t.Name, &t.Timezone, &t.Currency, &t.CheckinTime, &t.CheckoutTime, &t.Instructions, &t.Today)
	return t, err
}

// visibility es lo que el actor puede ver de las reservas.
type visibility struct {
	all       bool
	ids       []uuid.UUID
	todayOnly bool // seguridad: solo lo de hoy y lo que esta en curso
	today     string
	contact   bool // guests.read: correo, telefono y documento
}

func (s *Service) visibility(ctx context.Context, q database.Querier, actor authz.Actor, today string) (visibility, error) {
	scope, err := authz.ResolvePropertyScope(ctx, q, actor)
	if err != nil {
		return visibility{}, err
	}
	all, ids := scope.SQLArgs()
	return visibility{
		all: all, ids: ids, today: today, contact: actor.Permissions[authz.GuestsRead],
		todayOnly: actor.Permissions[authz.SecurityDashboard] &&
			!actor.Permissions[authz.ReservationsManage] && !actor.Permissions[authz.ReservationsCheckin],
	}, nil
}

// visibleSQL filtra reservas (alias r) por alcance; usa $1 a $4.
const visibleSQL = `($1::bool OR r.property_id = ANY($2::uuid[]))
	AND (NOT $3::bool OR r.status = 'checked_in'
	     OR (r.status NOT IN ('cancelled', 'no_show') AND (r.check_in_date = $4::date OR r.check_out_date = $4::date)))`

func (v visibility) args() []any {
	return []any{v.all, v.ids, v.todayOnly, v.today}
}

// --- Utilidades ----------------------------------------------------------------

const codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// newCode genera un codigo de reserva ARG-XXXXXX sin caracteres ambiguos.
func newCode() (string, error) {
	var b strings.Builder
	b.WriteString("ARG-")
	limit := big.NewInt(int64(len(codeAlphabet)))
	for range 6 {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		b.WriteByte(codeAlphabet[n.Int64()])
	}
	return b.String(), nil
}

func validDate(d string) bool {
	_, err := time.Parse(time.DateOnly, d)
	return err == nil
}

func validClock(t string) bool {
	_, err := time.Parse("15:04", t)
	return err == nil && len(t) == 5
}

func trimmed(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return nil
	}
	return &v
}

// actorID es el autor de un cambio (nil si lo hace el sistema).
func actorID(actor authz.Actor) *uuid.UUID {
	if actor.UserID == uuid.Nil {
		return nil
	}
	id := actor.UserID
	return &id
}
