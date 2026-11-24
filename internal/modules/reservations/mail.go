package reservations

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/core/mail"
)

// Correos al huesped (fase04.md §3): confirmacion con instrucciones y acceso a
// la app, recordatorio el dia anterior, cancelacion y movimientos del
// deposito. Se encolan en la transaccion del cambio; si el huesped no tiene
// correo no se manda nada (sent = false).

// guestContact es el huesped principal con lo que necesita el correo.
type guestContact struct {
	Name, Email     string
	Address, Zone   *string
	PrimaryGuestRef GuestRead
}

func primaryContact(ctx context.Context, q database.Querier, r ReservationRead) (*guestContact, error) {
	if r.PrimaryGuestID == nil {
		return nil, nil
	}
	g, err := guestByID(ctx, q, *r.PrimaryGuestID)
	if err != nil {
		return nil, err
	}
	if g.Email == nil {
		return nil, nil
	}
	c := &guestContact{Name: g.FullName, Email: *g.Email, PrimaryGuestRef: g}
	err = q.QueryRow(ctx, `SELECT address, zone FROM properties WHERE id = $1`, r.PropertyID).Scan(&c.Address, &c.Zone)
	return c, err
}

// stayData son los datos comunes de los correos de una reserva.
func stayData(t tenantInfo, r ReservationRead, c *guestContact) map[string]any {
	place := r.PropertyName
	if c.Address != nil && *c.Address != "" {
		place += " · " + *c.Address
	} else if c.Zone != nil && *c.Zone != "" {
		place += " · " + *c.Zone
	}
	return map[string]any{
		"TenantName": t.Name, "GuestName": firstName(c.Name), "Code": r.Code, "PropertyName": r.PropertyName,
		"Place": place, "CheckIn": longDate(r.CheckInDate), "CheckOut": longDate(r.CheckOutDate),
		"CheckInTime": r.CheckInTime, "CheckOutTime": r.CheckOutTime, "Nights": r.Nights,
	}
}

func (s *Service) sendConfirmation(ctx context.Context, q database.Querier, t tenantInfo, r ReservationRead) (bool, error) {
	c, err := primaryContact(ctx, q, r)
	if err != nil || c == nil {
		return false, err
	}
	data := stayData(t, r, c)
	data["Instructions"] = instructions(t.Instructions, c.Name, r)
	if s.access != nil {
		info, err := s.access(ctx, q, r, c.PrimaryGuestRef)
		if err != nil {
			return false, err
		}
		if info != nil {
			data["AppLink"], data["AppCode"] = info.Link, info.Code
		}
	}
	return true, jobs.EnqueueEmail(ctx, q, &t.ID, jobs.Email{To: c.Email, Template: mail.TemplateReservationConfirmed, Data: data})
}

func (s *Service) sendCancellation(ctx context.Context, q database.Querier, t tenantInfo, r ReservationRead) (bool, error) {
	c, err := primaryContact(ctx, q, r)
	if err != nil || c == nil {
		return false, err
	}
	return true, jobs.EnqueueEmail(ctx, q, &t.ID, jobs.Email{
		To: c.Email, Template: mail.TemplateReservationCancelled, Data: stayData(t, r, c),
	})
}

func (s *Service) sendDeposit(ctx context.Context, q database.Querier, t tenantInfo, r ReservationRead, tx DepositTxCreate, d DepositRead) (bool, error) {
	c, err := primaryContact(ctx, q, r)
	if err != nil || c == nil {
		return false, err
	}
	data := stayData(t, r, c)
	data["Kind"] = tx.Type
	data["Amount"] = money(r.Currency, tx.Amount)
	data["Balance"] = money(r.Currency, d.Balance)
	data["Received"] = money(r.Currency, d.Received)
	data["HasBalance"] = d.Balance > 0
	return true, jobs.EnqueueEmail(ctx, q, &t.ID, jobs.Email{To: c.Email, Template: mail.TemplateDepositUpdate, Data: data})
}

// --- Recordatorio del dia anterior ----------------------------------------------------

// JobReminder manda el recordatorio de llegada (dia anterior, 18:00 de la
// empresa). Payload: reminderPayload.
const JobReminder = "reservation_reminder"

type reminderPayload struct {
	ReservationID uuid.UUID `json:"reservation_id"`
	CheckInDate   string    `json:"check_in_date"`
}

// scheduleReminder programa el recordatorio si aun no paso su hora. Si la
// fecha cambia se programa otro; el viejo se descarta al correr.
func (s *Service) scheduleReminder(ctx context.Context, q database.Querier, tenantID uuid.UUID, r ReservationRead) error {
	var tz string
	if err := q.QueryRow(ctx, `SELECT timezone FROM shared.tenants WHERE id = $1`, tenantID).Scan(&tz); err != nil {
		return err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	day, err := time.ParseInLocation(time.DateOnly, r.CheckInDate, loc)
	if err != nil {
		return err
	}
	at := day.AddDate(0, 0, -1).Add(18 * time.Hour)
	if !at.After(s.now()) {
		return nil
	}
	return jobs.Enqueue(ctx, q, jobs.Job{
		Type: JobReminder, TenantID: &tenantID, ScheduledAt: at,
		Payload:   reminderPayload{ReservationID: r.ID, CheckInDate: r.CheckInDate},
		DedupeKey: fmt.Sprintf("%s:%s:%s", JobReminder, r.ID, r.CheckInDate),
	})
}

// RunReminder manda el recordatorio si la reserva sigue confirmada para esa
// fecha.
func (s *Service) RunReminder(ctx context.Context, q database.Querier, p reminderPayload) (bool, error) {
	r, err := reservationByID(ctx, q, p.ReservationID, false)
	if errors.Is(err, ErrReservationNotFound) {
		return false, nil
	}
	if err != nil || r.Status != "confirmed" || r.CheckInDate != p.CheckInDate {
		return false, err
	}
	t, err := s.tenant(ctx, q)
	if err != nil {
		return false, err
	}
	c, err := primaryContact(ctx, q, r)
	if err != nil || c == nil {
		return false, err
	}
	data := stayData(t, r, c)
	data["Instructions"] = instructions(t.Instructions, c.Name, r)
	if err := jobs.EnqueueEmail(ctx, q, &t.ID, jobs.Email{To: c.Email, Template: mail.TemplateReservationReminder, Data: data}); err != nil {
		return false, err
	}
	return true, addEvent(ctx, q, r.ID, "email_sent", nil, map[string]any{"template": "reminder"})
}

// instructions llena las variables que ofrece la configuracion (W-O1):
// {{huesped}}, {{propiedad}}, {{fecha_entrada}} y {{hora_entrada}}.
func instructions(text, guest string, r ReservationRead) string {
	return strings.TrimSpace(strings.NewReplacer(
		"{{huesped}}", firstName(guest), "{{propiedad}}", r.PropertyName,
		"{{fecha_entrada}}", longDate(r.CheckInDate), "{{hora_entrada}}", r.CheckInTime,
	).Replace(text))
}

// --- Formatos ------------------------------------------------------------------------

var (
	weekdaysES = [...]string{"domingo", "lunes", "martes", "miércoles", "jueves", "viernes", "sábado"}
	monthsLong = [...]string{
		"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre",
	}
)

// longDate convierte "2026-12-05" en "sábado 5 de diciembre".
func longDate(day string) string {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day
	}
	return fmt.Sprintf("%s %d de %s", weekdaysES[t.Weekday()], t.Day(), monthsLong[t.Month()-1])
}

// money formatea un monto: "Q 1,250.00" en quetzales, "USD 80.00" en otras.
func money(currency string, v float64) string {
	prefix := currency + " "
	if currency == "GTQ" {
		prefix = "Q "
	}
	cents := int64(math.Round(v * 100))
	whole, frac := cents/100, cents%100
	digits := fmt.Sprintf("%d", whole)
	var b strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	return fmt.Sprintf("%s%s.%02d", prefix, b.String(), frac)
}

func firstName(full string) string {
	if f := strings.Fields(full); len(f) > 0 {
		return f[0]
	}
	return full
}
