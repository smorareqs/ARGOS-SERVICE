package reservations

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/core/mail"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// EventRequested se publica cuando entra una reserva por la API publica (sitio
// web del cliente). Data es ReservationEvent. Persona B: push al admin.
const EventRequested = "reservation.requested"

// ErrNoDirectChannel indica que la empresa no tiene el canal Directo activo.
var ErrNoDirectChannel = apperr.New(apperr.KindValidation,
	"La empresa no tiene el canal Directo activo: actívalo en Reservas › Canales para recibir reservas del sitio web.")

// PublicReservation es el cuerpo de POST /public/reservations (fase06.md §4.1):
// la propiedad va por su codigo y el huesped principal es obligatorio.
type PublicReservation struct {
	_            struct{}    `additionalProperties:"true"`
	PropertyCode string      `json:"property_code" minLength:"1" maxLength:"32" doc:"Codigo de la propiedad en ARGOS (p. ej. GAR-101)."`
	CheckInDate  string      `json:"check_in_date" format:"date"`
	CheckOutDate string      `json:"check_out_date" format:"date" doc:"Exclusiva: el huesped sale ese dia."`
	Adults       *int        `json:"adults,omitempty" minimum:"1" maximum:"50" doc:"Por defecto 1."`
	Children     int         `json:"children,omitempty" minimum:"0" maximum:"50"`
	TotalAmount  *float64    `json:"total_amount,omitempty" minimum:"0" doc:"En la moneda de la empresa."`
	Guest        GuestCreate `json:"guest"`
	ExternalRef  *string     `json:"external_ref,omitempty" maxLength:"255" doc:"Referencia del sitio (numero de pedido)."`
	Notes        *string     `json:"notes,omitempty" maxLength:"4000"`
}

// PublicReservationRead es la respuesta: lo justo para que el sitio confirme
// al visitante que la solicitud llego.
type PublicReservationRead struct {
	ID           uuid.UUID `json:"id" format:"uuid"`
	Code         string    `json:"code" doc:"ARG-XXXXXX"`
	Status       string    `json:"status" doc:"Siempre pending: la empresa la confirma en ARGOS."`
	PropertyCode string    `json:"property_code"`
	PropertyName string    `json:"property_name"`
	CheckInDate  string    `json:"check_in_date"`
	CheckOutDate string    `json:"check_out_date"`
	Nights       int       `json:"nights"`
}

// CreateFromAPI registra la reserva que manda el sitio web del cliente con su
// clave API: queda por confirmar (source api), avisa por correo a quien
// administra reservas y publica reservation.requested. ctx ya trae el schema
// de la empresa de la clave.
func (s *Service) CreateFromAPI(ctx context.Context, tenantID uuid.UUID, keyName string, in PublicReservation) (PublicReservationRead, error) {
	actor := authz.Actor{TenantID: tenantID, Permissions: map[authz.Permission]bool{
		authz.ReservationsManage: true, authz.GuestsManage: true, authz.GuestsRead: true,
	}}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (PublicReservationRead, error) {
		t, err := s.tenant(ctx, q)
		if err != nil {
			return PublicReservationRead{}, err
		}
		propertyID, err := propertyByCode(ctx, q, in.PropertyCode)
		if err != nil {
			return PublicReservationRead{}, err
		}
		var channelID uuid.UUID
		err = q.QueryRow(ctx, `
			SELECT id FROM booking_channels WHERE kind = 'direct' AND is_active ORDER BY created_at LIMIT 1`).Scan(&channelID)
		if errors.Is(err, pgx.ErrNoRows) {
			return PublicReservationRead{}, ErrNoDirectChannel
		}
		if err != nil {
			return PublicReservationRead{}, err
		}
		guest := in.Guest
		row, err := s.newRow(ctx, q, actor, t, ReservationCreate{
			PropertyID: propertyID, ChannelID: channelID, CheckInDate: in.CheckInDate, CheckOutDate: in.CheckOutDate,
			Adults: in.Adults, Children: in.Children, TotalAmount: in.TotalAmount, Guest: &guest,
			ExternalRef: in.ExternalRef, Notes: in.Notes,
		})
		if err != nil {
			return PublicReservationRead{}, err
		}
		row.source, row.status = "api", "pending"
		r, err := s.insertWithGuest(ctx, q, actor, row)
		if err != nil {
			return PublicReservationRead{}, err
		}
		if err := s.notifyRequested(ctx, q, t, keyName, r); err != nil {
			return PublicReservationRead{}, err
		}
		return PublicReservationRead{
			ID: r.ID, Code: r.Code, Status: r.Status, PropertyCode: r.PropertyCode, PropertyName: r.PropertyName,
			CheckInDate: r.CheckInDate, CheckOutDate: r.CheckOutDate, Nights: r.Nights,
		}, nil
	})
}

func propertyByCode(ctx context.Context, q database.Querier, code string) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		SELECT id FROM properties WHERE upper(code) = upper($1) AND deleted_at IS NULL AND is_active`,
		strings.TrimSpace(code)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrPropertyNotFound.WithField("body.property_code")
	}
	return id, err
}

// notifyRequested escribe a quien puede confirmar reservas y publica el evento.
func (s *Service) notifyRequested(ctx context.Context, q database.Querier, t tenantInfo, keyName string, r ReservationRead) error {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT u.email, u.full_name
		FROM shared.memberships m
		JOIN shared.users u ON u.id = m.user_id AND u.is_active
		JOIN shared.role_permissions rp ON rp.role_id = m.role_id AND rp.permission = $2
		WHERE m.tenant_id = $1 AND m.status = 'active'`, t.ID, string(authz.ReservationsManage))
	if err != nil {
		return err
	}
	type person struct{ email, name string }
	var people []person
	for rows.Next() {
		var p person
		if err := rows.Scan(&p.email, &p.name); err != nil {
			rows.Close()
			return err
		}
		people = append(people, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	guest := ""
	if r.GuestName != nil {
		guest = *r.GuestName
	}
	for _, p := range people {
		if err := jobs.EnqueueEmail(ctx, q, &t.ID, jobs.Email{To: p.email, Template: mail.TemplateReservationRequested, Data: map[string]any{
			"Name": firstWord(p.name), "Guest": guest, "Property": r.PropertyCode + " · " + r.PropertyName,
			"Range": shortDate(r.CheckInDate) + " al " + shortDate(r.CheckOutDate), "Nights": nightsLabel(r.Nights), "Code": r.Code,
			"Link": s.webBase + "/reservas?ver=" + r.Code, "KeyName": keyName,
		}}); err != nil {
			return err
		}
	}
	return s.bus.Publish(ctx, q, events.Event{Type: EventRequested, TenantID: t.ID, Data: ReservationEvent{Reservation: r}})
}

func firstWord(name string) string {
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	return name
}

func nightsLabel(n int) string {
	if n == 1 {
		return "1 noche"
	}
	return strconv.Itoa(n) + " noches"
}
