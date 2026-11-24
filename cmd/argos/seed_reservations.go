package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/app"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/optional"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/properties"
	"github.com/smorareqs/argos-service/internal/modules/reservations"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
)

// Semilla de la fase 4 (Persona A): canales, huespedes, reservas en todos los
// estados (pasadas con deposito e inspeccion, en casa, llegadas y salidas de
// hoy, proximas, importadas por completar, canceladas y no-show) y
// calendarios iCal que apuntan a /dev/ical de la API local. Pasa por los
// servicios; solo las fechas de lo pasado se ajustan con SQL. Las fotos del
// documento son tarjetas generadas (no personas reales). Si la empresa ya
// tiene reservas, no se toca.

type demoGuest struct {
	key, name, email, docType, doc, nat, blacklist string
}

type demoRes struct {
	prop, channel, guest string
	from, to             int
	flow                 string // ota, confirmed, deposit_early, in_house, past_clean, past_damage, pending_ical, cancelled, no_show
	ref, summary         string
	total                float64
}

type demoFeed struct {
	prop, channel, file string
}

type demoReservations struct {
	deposit float64 // deposito por defecto del canal Directo
	agency  string
	guests  []demoGuest
	res     []demoRes
	feeds   []demoFeed
}

var demoReservationData = map[string]demoReservations{
	"garrent": {
		deposit: 1200, agency: "Maya Tours",
		guests: []demoGuest{
			{"lucia", "Lucía Pérez", "lucia.perez@correo.gt", "dpi", "2456789010101", "GT", ""},
			{"emily", "Emily Carter", "emily.carter@mail.com", "passport", "568901234", "US", ""},
			{"andres", "Andrés Molina", "andres.molina@correo.gt", "dpi", "1987654320101", "GT", ""},
			{"hans", "Hans Müller", "hans.mueller@mail.de", "passport", "C4R7T2K9", "DE", ""},
			{"carlos", "Carlos Mendoza", "carlos.mendoza@correo.gt", "dpi", "3012457890101", "GT", ""},
			{"sophie", "Sophie Martin", "sophie.martin@mail.fr", "passport", "21AB34567", "FR", ""},
			{"diego", "Diego Ramírez", "diego.ramirez@correo.mx", "passport", "G43215678", "MX", ""},
			{"valeria", "Valeria Ruiz", "valeria.ruiz@correo.gt", "dpi", "2765432100101", "GT", ""},
			{"mario", "Mario Ruiz", "mario.ruiz@correo.gt", "", "", "GT", ""},
			{"ana", "Ana Gómez", "ana.gomez@correo.sv", "passport", "A12345678", "SV", ""},
			{"james", "James Wilson", "james.wilson@mail.com", "", "", "US", ""},
			{"mj", "María José Pineda", "mj.pineda@correo.gt", "dpi", "2109876540101", "GT", ""},
			{"roberto", "Roberto Herrera", "roberto.herrera@correo.gt", "", "", "GT", ""},
			{"laura", "Laura Castillo", "laura.castillo@correo.gt", "dpi", "2987123450101", "GT", ""},
			{"camila", "Camila Torres", "camila.torres@correo.co", "passport", "AX9087654", "CO", ""},
			{"esteban", "Esteban Ortiz", "esteban.ortiz@correo.gt", "", "", "GT", ""},
			{"peter", "Peter Novak", "peter.novak@mail.cz", "passport", "45123987", "CZ", ""},
			{"rodrigo", "Rodrigo Quan", "rodrigo.quan@correo.gt", "dpi", "2233445560101", "GT", "Daños en la sala y fiesta no autorizada (marzo 2026)."},
		},
		res: []demoRes{
			{prop: "TB-1102", channel: "direct", guest: "lucia", from: -12, to: -8, flow: "past_clean", total: 3600},
			{prop: "AN-SEB", channel: "airbnb", guest: "emily", from: -9, to: -5, flow: "ota", total: 4400},
			{prop: "CY-PH1", channel: "direct", guest: "andres", from: -6, to: -2, flow: "past_damage", total: 6800},
			{prop: "MT-PLY", channel: "booking", guest: "hans", from: -4, to: -1, flow: "ota", total: 5100},
			{prop: "TB-504", channel: "direct", guest: "carlos", from: -2, to: 2, flow: "in_house", total: 3200},
			{prop: "AT-VIS", channel: "airbnb", guest: "sophie", from: -1, to: 4, flow: "ota", ref: "ab-atvis-HMX4Q2", summary: "Reserved", total: 6250},
			{prop: "CY-12A", channel: "booking", guest: "diego", from: -3, to: 1, flow: "ota", total: 3400},
			{prop: "AN-CAR", channel: "direct", guest: "valeria", from: -3, to: 0, flow: "in_house", total: 2700},
			{prop: "TB-1102", channel: "direct", guest: "mario", from: 0, to: 3, flow: "confirmed", total: 2700},
			{prop: "AN-H03", channel: "agency", guest: "ana", from: 0, to: 2, flow: "confirmed", total: 900},
			{prop: "CY-08C", channel: "airbnb", guest: "james", from: 0, to: 5, flow: "confirmed", ref: "ab-cy08c-HMT9P4", summary: "Reserved", total: 4000},
			{prop: "TB-207", channel: "direct", guest: "mj", from: 2, to: 6, flow: "deposit_early", total: 2800},
			{prop: "AN-SEB", channel: "booking", from: 3, to: 7, flow: "pending_ical", ref: "bk-anseb-88213", summary: "CLOSED - Not available"},
			{prop: "TB-504", channel: "airbnb", guest: "roberto", from: 5, to: 9, flow: "confirmed", ref: "ab-tb504-HMB2N8", summary: "Reserved", total: 3600},
			{prop: "CY-PH1", channel: "direct", guest: "laura", from: 8, to: 12, flow: "confirmed", total: 6800},
			{prop: "AN-H05", channel: "agency", guest: "camila", from: 4, to: 6, flow: "confirmed", total: 900},
			{prop: "CY-12A", channel: "direct", guest: "esteban", from: 10, to: 13, flow: "cancelled", total: 2550},
			{prop: "AN-H03", channel: "booking", guest: "peter", from: -5, to: -3, flow: "no_show", total: 900},
		},
		feeds: []demoFeed{
			{"AN-SEB", "booking", "garrent-an-seb-booking.ics"},
			{"AT-VIS", "airbnb", "garrent-at-vis-airbnb.ics"},
			{"TB-504", "airbnb", "garrent-tb-504-airbnb.ics"},
			{"CY-08C", "airbnb", "garrent-cy-08c-airbnb.ics"},
			{"AN-CAR", "booking", "garrent-an-car-booking.ics"}, // no existe: muestra el error del canal
		},
	},
	"costa_limpia": {
		deposit: 800,
		guests: []demoGuest{
			{"karla", "Karla Estrada", "karla.estrada@correo.gt", "dpi", "2654789120501", "GT", ""},
			{"tomas", "Tomás Reyes", "tomas.reyes@correo.sv", "passport", "B98765432", "SV", ""},
			{"jenny", "Jennifer Brooks", "jenny.brooks@mail.com", "", "", "US", ""},
			{"oscar", "Óscar Velásquez", "oscar.velasquez@correo.gt", "dpi", "1876543210501", "GT", ""},
		},
		res: []demoRes{
			{prop: "MB-3B", channel: "direct", guest: "karla", from: -5, to: -1, flow: "past_clean", total: 2400},
			{prop: "MB-7C", channel: "booking", guest: "tomas", from: -1, to: 3, flow: "ota", total: 3100},
			{prop: "MT-SOL", channel: "direct", guest: "oscar", from: 0, to: 4, flow: "confirmed", total: 5200},
			{prop: "MB-5A", channel: "booking", from: 2, to: 6, flow: "pending_ical", ref: "bk-mb5a-77120", summary: "CLOSED - Not available"},
			{prop: "MB-3B", channel: "airbnb", guest: "jenny", from: 6, to: 9, flow: "confirmed", total: 2250},
		},
		feeds: []demoFeed{{"MB-5A", "booking", "costa_limpia-mb-5a-booking.ics"}},
	},
}

// seedReservations siembra la fase 4 en cada empresa de demostracion.
func seedReservations(ctx context.Context, services app.Services, db *database.DB, ids map[string]tenants.Tenant) error {
	for slug, data := range demoReservationData {
		t := ids[slug]
		admin, err := services.Auth.UserByEmail(ctx, demoOperations[slug].admin)
		if err != nil {
			return err
		}
		s := resSeeder{
			services: services, db: db, ctx: database.WithTenantSchema(ctx, t.SchemaName), tenant: t,
			admin:  authz.Actor{UserID: admin.ID, TenantID: t.ID, Permissions: allPermissions()},
			guests: map[string]uuid.UUID{}, channels: map[string]uuid.UUID{}, props: map[string]uuid.UUID{},
		}
		if err := s.run(data); err != nil {
			return fmt.Errorf("reservas de %s: %w", slug, err)
		}
	}
	fmt.Println("Canales, huéspedes, reservas y calendarios iCal de demostracion listos.")
	return nil
}

type resSeeder struct {
	services app.Services
	db       *database.DB
	ctx      context.Context
	tenant   tenants.Tenant
	admin    authz.Actor
	today    string
	guests   map[string]uuid.UUID
	channels map[string]uuid.UUID
	props    map[string]uuid.UUID
}

func (s *resSeeder) run(data demoReservations) error {
	existing, _, err := s.services.Reservations.Reservations(s.ctx, s.admin, reservations.ReservationFilter{Limit: 1})
	if err != nil || len(existing) > 0 {
		return err
	}
	if err := s.load(data); err != nil {
		return err
	}
	for _, g := range data.guests {
		if err := s.guest(g); err != nil {
			return fmt.Errorf("huésped %s: %w", g.name, err)
		}
	}
	for _, r := range data.res {
		if err := s.reservation(r, data.deposit); err != nil {
			return fmt.Errorf("reserva %s %+d: %w", r.prop, r.from, err)
		}
	}
	if err := s.db.InTx(s.ctx, func(q database.Querier) error {
		_, err := s.services.Reservations.RunAutoStatus(s.ctx, q, s.tenant.ID)
		return err
	}); err != nil {
		return err
	}
	if err := s.backdate(); err != nil {
		return err
	}
	base := strings.TrimRight(envOr("ARGOS_SEED_ICAL_BASE", "http://api:8000"), "/")
	for _, f := range data.feeds {
		if _, err := s.services.Reservations.CreateFeed(s.ctx, s.admin, s.props[f.prop],
			reservations.FeedCreate{ChannelID: s.channels[f.channel], URL: base + "/dev/ical/" + f.file}); err != nil {
			return fmt.Errorf("calendario %s: %w", f.file, err)
		}
	}
	return nil
}

// load prepara canales (deposito de Directo y la agencia) y propiedades.
func (s *resSeeder) load(data demoReservations) error {
	if err := s.db.InTx(s.ctx, func(q database.Querier) error {
		return q.QueryRow(s.ctx, `SELECT to_char((now() AT TIME ZONE timezone)::date, 'YYYY-MM-DD') FROM shared.tenants WHERE id = $1`,
			s.tenant.ID).Scan(&s.today)
	}); err != nil {
		return err
	}
	channels, err := s.services.Reservations.Channels(s.ctx, s.admin)
	if err != nil {
		return err
	}
	for _, c := range channels {
		s.channels[c.Kind] = c.ID
		if c.Kind == "direct" {
			if _, err := s.services.Reservations.UpdateChannel(s.ctx, s.admin, c.ID, reservations.ChannelUpdate{
				DefaultDepositAmount: optional.Of(data.deposit),
			}); err != nil {
				return err
			}
		}
	}
	if data.agency != "" {
		c, err := s.services.Reservations.CreateChannel(s.ctx, s.admin, reservations.ChannelCreate{Name: data.agency, Kind: "agency"})
		if err != nil {
			return err
		}
		s.channels["agency"] = c.ID
	}
	list, _, err := s.services.Properties.List(s.ctx, s.admin, properties.ListFilter{Limit: 500})
	for _, p := range list {
		s.props[p.Code] = p.ID
	}
	return err
}

func (s *resSeeder) guest(g demoGuest) error {
	in := reservations.GuestCreate{FullName: g.name, Email: optStr(g.email), Nationality: optStr(g.nat)}
	if g.doc != "" {
		in.DocumentType, in.DocumentNumber = &g.docType, &g.doc
	}
	created, err := s.services.Reservations.CreateGuest(s.ctx, s.admin, in)
	if err != nil {
		return err
	}
	s.guests[g.key] = created.ID
	if g.blacklist == "" {
		return nil
	}
	black := true
	_, err = s.services.Reservations.UpdateGuest(s.ctx, s.admin, created.ID, reservations.GuestUpdate{
		Blacklisted: &black, BlacklistReason: optional.Of(g.blacklist),
	})
	return err
}

func (s *resSeeder) reservation(r demoRes, deposit float64) error {
	in := reservations.ReservationCreate{
		PropertyID: s.props[r.prop], ChannelID: s.channels[r.channel], CheckInDate: addDaysTo(s.today, r.from),
		CheckOutDate: addDaysTo(s.today, r.to), Adults: ptr(2), ExternalRef: optStr(r.ref), SendEmail: ptr(false),
	}
	if r.total > 0 {
		in.TotalAmount = &r.total
	}
	if r.guest != "" {
		id := s.guests[r.guest]
		in.PrimaryGuestID = &id
	}
	d, err := s.services.Reservations.Create(s.ctx, s.admin, in)
	if err != nil {
		return err
	}
	if r.ref != "" {
		if err := s.db.InTx(s.ctx, func(q database.Querier) error {
			_, err := q.Exec(s.ctx, `UPDATE reservations SET source = 'ical', external_summary = $2 WHERE id = $1`, d.ID, r.summary)
			return err
		}); err != nil {
			return err
		}
	}
	return s.flow(r, d, deposit)
}

// flow lleva la reserva al estado de la demo por los mismos pasos de la web.
func (s *resSeeder) flow(r demoRes, d reservations.ReservationDetail, deposit float64) error {
	svc, id := s.services.Reservations, d.ID
	switch r.flow {
	case "deposit_early":
		return s.deposit(id, "received", deposit, "transfer")
	case "cancelled":
		_, err := svc.Cancel(s.ctx, s.admin, id, reservations.Reason{Reason: "Lo pidió el huésped"})
		return err
	case "no_show":
		_, err := svc.NoShow(s.ctx, s.admin, id, reservations.Reason{Reason: "No llegó ni avisó"})
		return err
	case "in_house", "past_clean", "past_damage":
		return s.stay(r, d, deposit)
	}
	return nil
}

// stay registra documento, deposito y check-in; las pasadas tambien salida,
// inspeccion y liquidacion del deposito.
func (s *resSeeder) stay(r demoRes, d reservations.ReservationDetail, deposit float64) error {
	svc, id := s.services.Reservations, d.ID
	if err := s.documents(*d.PrimaryGuestID, d.CheckOutDate); err != nil {
		return err
	}
	if err := s.deposit(id, "received", deposit, "cash"); err != nil {
		return err
	}
	if _, err := svc.CheckIn(s.ctx, s.admin, id); err != nil {
		return err
	}
	if r.flow == "in_house" {
		return nil
	}
	if _, err := svc.CheckOut(s.ctx, s.admin, id); err != nil {
		return err
	}
	notes := "Sin novedades: la propiedad quedó como se entregó."
	if r.flow == "past_damage" {
		notes = "Todo en orden salvo la sala."
	}
	det, err := svc.CreateInspection(s.ctx, s.admin, id, reservations.InspectionCreate{Kind: "post_departure", Notes: &notes})
	if err != nil {
		return err
	}
	if r.flow == "past_clean" {
		return s.deposit(id, "returned", deposit, "transfer")
	}
	cost := 350.0
	if _, err := svc.AddFinding(s.ctx, s.admin, det.Inspections[0].ID, reservations.FindingCreate{
		Area: "Sala", Description: "Mancha de vino en el sofá; requiere limpieza profesional.", Severity: "medium",
		EstimatedCost: &cost, ChargeToDeposit: true, CreateTicket: true,
	}); err != nil {
		return err
	}
	if err := s.deposit(id, "retained", cost, "other"); err != nil {
		return err
	}
	return s.deposit(id, "returned", deposit-cost, "transfer")
}

func (s *resSeeder) deposit(id uuid.UUID, kind string, amount float64, method string) error {
	_, err := s.services.Reservations.AddDepositTx(s.ctx, s.admin, id, reservations.DepositTxCreate{Type: kind, Amount: amount, Method: method})
	return err
}

// documents sube dos tarjetas generadas como frente y reverso del documento.
func (s *resSeeder) documents(guestID uuid.UUID, checkOut string) error {
	for i, kind := range []string{"id_front", "id_back"} {
		body, err := demoCard(i == 0)
		if err != nil {
			return err
		}
		name := "documento-" + strings.TrimPrefix(kind, "id_") + ".jpg"
		key, err := s.services.Storage.BuildKey(s.tenant.ID, "guests", kind, name)
		if err != nil {
			return err
		}
		if err := s.services.Storage.PutObject(s.ctx, s.tenant.ID, key, "image/jpeg", body); err != nil {
			return err
		}
		if err := s.db.InTx(s.ctx, func(q database.Querier) error {
			_, err := q.Exec(s.ctx, `
				INSERT INTO files (s3_key, module, kind, entity_type, entity_id, content_type, size_bytes, uploaded_by_user_id,
				                   original_name, status, expires_at)
				VALUES ($1, 'guests', $2, 'guest', $3, 'image/jpeg', $4, $5, $6, 'ready', ($7::date + 90)::timestamptz)`,
				key, kind, guestID, len(body), s.admin.UserID, name, checkOut)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// backdate fecha lo pasado: entradas y salidas a su hora, y la linea de
// tiempo alrededor de esas fechas.
func (s *resSeeder) backdate() error {
	return s.db.InTx(s.ctx, func(q database.Querier) error {
		_, err := q.Exec(s.ctx, `
			WITH tz AS (SELECT timezone AS name FROM shared.tenants WHERE schema_name = current_schema())
			UPDATE reservations r SET
			       created_at = least(r.created_at, ((r.check_in_date - 18) + time '10:00') AT TIME ZONE (SELECT name FROM tz)),
			       checked_in_at = CASE WHEN r.checked_in_at IS NULL THEN NULL
			                       ELSE (r.check_in_date + r.check_in_time + interval '40 minutes') AT TIME ZONE (SELECT name FROM tz) END,
			       checked_out_at = CASE WHEN r.checked_out_at IS NULL OR r.check_out_date > current_date THEN r.checked_out_at
			                        ELSE (r.check_out_date + r.check_out_time - interval '25 minutes') AT TIME ZONE (SELECT name FROM tz) END`)
		if err != nil {
			return err
		}
		_, err = q.Exec(s.ctx, `
			UPDATE reservation_events e SET created_at = CASE
			         WHEN e.type IN ('created', 'confirmed') THEN r.created_at + interval '2 minutes'
			         WHEN e.type = 'checked_in' THEN coalesce(r.checked_in_at, e.created_at)
			         WHEN e.type IN ('checked_out', 'inspection', 'finding') THEN coalesce(r.checked_out_at, e.created_at) + interval '30 minutes'
			         WHEN e.type IN ('deposit_returned', 'deposit_retained') THEN coalesce(r.checked_out_at, e.created_at) + interval '1 day'
			         WHEN e.type = 'deposit_received' THEN coalesce(r.checked_in_at - interval '1 hour', e.created_at)
			         ELSE e.created_at END
			FROM reservations r WHERE r.id = e.reservation_id AND r.check_in_date < current_date`)
		if err != nil {
			return err
		}
		_, err = q.Exec(s.ctx, `
			UPDATE deposit_transactions t SET created_at = e.created_at
			FROM reservation_events e
			WHERE e.reservation_id = t.reservation_id AND e.type = 'deposit_' || t.type`)
		return err
	})
}

// demoCard dibuja una tarjeta de identificacion generica (sin datos reales).
func demoCard(front bool) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, 640, 400))
	fill := func(r image.Rectangle, c color.RGBA) { draw.Draw(img, r, &image.Uniform{c}, image.Point{}, draw.Src) }
	fill(img.Bounds(), color.RGBA{0xf1, 0xef, 0xe8, 0xff})
	fill(image.Rect(0, 0, 640, 64), color.RGBA{0x0b, 0x4f, 0x58, 0xff})
	lines := color.RGBA{0x9a, 0xa3, 0xa2, 0xff}
	if front {
		fill(image.Rect(32, 96, 208, 316), color.RGBA{0xc9, 0xcf, 0xcd, 0xff})
		for i, w := range []int{300, 240, 280, 180, 220} {
			fill(image.Rect(240, 110+i*40, 240+w, 124+i*40), lines)
		}
	} else {
		for i := range 6 {
			fill(image.Rect(32, 100+i*36, 608, 112+i*36), lines)
		}
		fill(image.Rect(32, 330, 400, 360), color.RGBA{0x30, 0x33, 0x33, 0xff})
	}
	var buf bytes.Buffer
	err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80})
	return buf.Bytes(), err
}

func addDaysTo(day string, n int) string {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, n).Format(time.DateOnly)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
