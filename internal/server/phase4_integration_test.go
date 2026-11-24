//go:build integration

package server_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/storage"
	"github.com/smorareqs/argos-service/internal/modules/reservations"
)

// Pruebas de la fase 4 de Persona A (fase04.md §7): ciclo de una venta directa
// con registro, deposito e inspeccion; alcance de seguridad y propietario;
// documentos del huesped y su retencion; iCal; estados automaticos e Inicio.

// uploadIn sube un archivo de cualquier modulo al almacen en memoria y lo confirma.
func (o *opsEnv) uploadIn(token, module, kind, contentType, filename string) string {
	o.t.Helper()
	res := o.do(http.MethodPost, "/api/v1/files/presign-upload", token, map[string]any{
		"module": module, "kind": kind, "content_type": contentType, "size": 4096, "filename": filename,
	})
	expect(o.t, res, http.StatusOK)
	id := res.body["file_id"].(string)
	var key string
	if err := testDB.Pool().QueryRow(context.Background(), `SELECT s3_key FROM tenant_garrent.files WHERE id = $1`, id).Scan(&key); err != nil {
		o.t.Fatal(err)
	}
	o.svc.Storage.Backend().(*storage.Memory).Put(key, storage.ObjectInfo{Size: 4096, ContentType: contentType})
	expect(o.t, o.do(http.MethodPost, "/api/v1/files/"+id+"/confirm", token, nil), http.StatusOK)
	return id
}

// channel devuelve el id del canal de ese tipo (direct, booking, airbnb).
func (o *opsEnv) channel(kind string) string {
	o.t.Helper()
	res := o.do(http.MethodGet, "/api/v1/channels", o.admin, nil)
	expect(o.t, res, http.StatusOK)
	for _, c := range items(res) {
		if c["kind"] == kind {
			return c["id"].(string)
		}
	}
	o.t.Fatalf("no hay canal %s", kind)
	return ""
}

// reserve crea una reserva como admin y devuelve su cuerpo.
func (o *opsEnv) reserve(body map[string]any) map[string]any {
	o.t.Helper()
	res := o.do(http.MethodPost, "/api/v1/reservations", o.admin, body)
	expect(o.t, res, http.StatusCreated)
	return res.body
}

func message(res response) string {
	out, _ := res.body["detail"].(string)
	list, _ := res.body["errors"].([]any)
	for _, e := range list {
		if m, ok := e.(map[string]any)["message"].(string); ok {
			out += " " + m
		}
	}
	return out
}

func timeline(body map[string]any) []string {
	out := []string{}
	for _, e := range body["events"].([]any) {
		out = append(out, e.(map[string]any)["type"].(string))
	}
	return out
}

func queuedEmails(t *testing.T, template string) int {
	t.Helper()
	var n int
	if err := testDB.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM shared.jobs WHERE type = 'send_email' AND payload->>'template' = $1`, template).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// fase04.md §3 y §8: venta directa con registro, deposito, inspeccion de
// salida, retencion por un hallazgo y devolucion del resto.
func TestReservationLifecycle(t *testing.T) {
	e := setup(t)
	o := e.ops()
	prop := o.property("TB-1102", nil)
	today := todayGT(t)
	var mu sync.Mutex
	published := []string{}
	for _, kind := range []string{reservations.EventConfirmed, reservations.EventCheckedIn, reservations.EventCheckedOut} {
		e.svc.Events.Subscribe(kind, func(_ context.Context, _ database.Querier, ev events.Event) error {
			mu.Lock()
			defer mu.Unlock()
			published = append(published, ev.Type)
			return nil
		})
	}
	direct := o.channel("direct")

	// Alta con huesped nuevo: queda confirmada y manda el correo.
	r := o.reserve(map[string]any{
		"property_id": prop, "channel_id": direct, "check_in_date": today, "check_out_date": addDays(today, 3),
		"total_amount": 2400, "deposit_amount": 1200, "guest": map[string]any{"full_name": "Lucía Pérez", "email": "Lucia@Correo.com"},
	})
	rid, code := r["id"].(string), r["code"].(string)
	if r["status"] != "confirmed" || !strings.HasPrefix(code, "ARG-") || r["deposit_status"] != "pending" || r["guest_email"] != "lucia@correo.com" {
		t.Fatalf("alta inesperada: %v %v %v %v", r["status"], code, r["deposit_status"], r["guest_email"])
	}
	guest := r["guests"].([]any)[0].(map[string]any)
	if guest["registered"] != false || !slices.Contains(toStrings(guest["missing"]), "id_front") {
		t.Fatalf("registro inicial: %v", guest)
	}
	if queuedEmails(t, "reservation_confirmed") != 1 {
		t.Fatal("la confirmación debe encolar el correo al huésped")
	}
	// Una reserva a futuro programa el recordatorio del día anterior.
	o.reserve(map[string]any{
		"property_id": prop, "channel_id": direct, "check_in_date": addDays(today, 20), "check_out_date": addDays(today, 22),
		"guest": map[string]any{"full_name": "Mario Ruiz", "email": "mario@correo.com"},
	})
	var reminders int
	if err := testDB.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM shared.jobs WHERE type = 'reservation_reminder'`).Scan(&reminders); err != nil || reminders != 1 {
		t.Fatalf("recordatorios = %d (%v)", reminders, err)
	}

	// Fechas que se cruzan: 422 con la reserva que estorba; la base tambien lo impide.
	res := o.do(http.MethodPost, "/api/v1/reservations", o.admin, map[string]any{
		"property_id": prop, "channel_id": direct, "check_in_date": addDays(today, 1), "check_out_date": addDays(today, 2),
	})
	expect(t, res, http.StatusUnprocessableEntity)
	if field(res) != "body.check_in_date" || !strings.Contains(message(res), code) {
		t.Fatalf("solapamiento: %s %s", field(res), message(res))
	}
	_, err := testDB.Pool().Exec(context.Background(), `
		INSERT INTO tenant_garrent.reservations (property_id, channel_id, code, status, check_in_date, check_out_date,
		       check_in_time, check_out_time, currency)
		VALUES ($1, $2, 'ARG-TEST01', 'pending', $3::date + 1, $3::date + 2, '15:00', '11:00', 'GTQ')`, prop, direct, today)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23P01" {
		t.Fatalf("la base debe rechazar el cruce: %v", err)
	}

	// Check-in sin registro: dice a quien le falta.
	res = o.do(http.MethodPost, "/api/v1/reservations/"+rid+"/check-in", o.super, nil)
	expect(t, res, http.StatusUnprocessableEntity)
	if !strings.Contains(message(res), "Lucía Pérez") {
		t.Fatalf("check-in sin registro: %s", message(res))
	}
	// Registro en el check-in (supervisión): documento y fotos.
	gid := guest["id"].(string)
	res = o.do(http.MethodPatch, "/api/v1/guests/"+gid, o.super, map[string]any{"document_type": "dpi", "document_number": "2456 78901 0101"})
	expect(t, res, http.StatusOK)
	if res.body["document_number"] != "2456789010101" {
		t.Fatalf("documento normalizado: %v", res.body["document_number"])
	}
	expect(t, o.do(http.MethodPatch, "/api/v1/guests/"+gid, o.super, map[string]any{"blacklisted": true}), http.StatusForbidden)
	for _, kind := range []string{"id_front", "id_back"} {
		file := o.uploadIn(o.super, "guests", kind, "image/jpeg", "dpi-"+kind+".jpg")
		expect(t, o.do(http.MethodPost, "/api/v1/guests/"+gid+"/documents", o.super, map[string]any{"kind": kind, "file_id": file}), http.StatusCreated)
	}
	// Falta el deposito; supervision no lo registra.
	res = o.do(http.MethodPost, "/api/v1/reservations/"+rid+"/check-in", o.super, nil)
	expect(t, res, http.StatusUnprocessableEntity)
	if !strings.Contains(message(res), "depósito") {
		t.Fatalf("check-in sin depósito: %s", message(res))
	}
	deposit := func(token string, body map[string]any, status int) map[string]any {
		t.Helper()
		res := o.do(http.MethodPost, "/api/v1/reservations/"+rid+"/deposit/transactions", token, body)
		expect(t, res, status)
		return res.body
	}
	deposit(o.super, map[string]any{"type": "received", "amount": 1200, "method": "cash"}, http.StatusForbidden)
	deposit(o.admin, map[string]any{"type": "received", "amount": 1500, "method": "cash"}, http.StatusUnprocessableEntity)
	if d := deposit(o.admin, map[string]any{"type": "received", "amount": 1200, "method": "cash"}, http.StatusCreated); d["status"] != "received" {
		t.Fatalf("depósito recibido: %v", d["status"])
	}
	res = o.do(http.MethodPost, "/api/v1/reservations/"+rid+"/check-in", o.super, nil)
	expect(t, res, http.StatusOK)
	if res.body["status"] != "checked_in" {
		t.Fatalf("check-in: %v", res.body["status"])
	}

	// Check-out: las fotos del documento vencen a los 90 dias.
	res = o.do(http.MethodPost, "/api/v1/reservations/"+rid+"/check-out", o.super, nil)
	expect(t, res, http.StatusOK)
	var expiry string
	if err := testDB.Pool().QueryRow(context.Background(), `
		SELECT to_char(min(expires_at)::date, 'YYYY-MM-DD') FROM tenant_garrent.files WHERE entity_id = $1 AND status = 'ready'`,
		gid).Scan(&expiry); err != nil || expiry < addDays(today, 89) || expiry > addDays(today, 91) {
		t.Fatalf("vencimiento de las fotos = %s (%v)", expiry, err)
	}

	// Devolver sin inspeccion: 422. Inspeccion de salida con hallazgo con cargo e incidencia.
	deposit(o.admin, map[string]any{"type": "returned", "amount": 1200, "method": "cash"}, http.StatusUnprocessableEntity)
	expect(t, o.do(http.MethodPost, "/api/v1/reservations/"+rid+"/inspections", o.super, map[string]any{"kind": "pre_arrival"}),
		http.StatusUnprocessableEntity)
	photo := o.uploadIn(o.super, "reservations", "inspection_photo", "image/jpeg", "sala.jpg")
	res = o.do(http.MethodPost, "/api/v1/reservations/"+rid+"/inspections", o.super, map[string]any{
		"kind": "post_departure", "notes": "Todo en orden salvo la sala.", "photo_file_ids": []string{photo},
	})
	expect(t, res, http.StatusCreated)
	insp := res.body["inspections"].([]any)[0].(map[string]any)
	if len(insp["photos"].([]any)) != 1 {
		t.Fatal("la inspección debe traer su foto")
	}
	res = o.do(http.MethodPost, "/api/v1/inspections/"+insp["id"].(string)+"/findings", o.super, map[string]any{
		"area": "Sala", "description": "Mancha de vino en el sofá", "severity": "medium", "estimated_cost": 300,
		"charge_to_deposit": true, "create_ticket": true,
	})
	expect(t, res, http.StatusCreated)
	finding := res.body["inspections"].([]any)[0].(map[string]any)["findings"].([]any)[0].(map[string]any)
	if finding["ticket_number"] == nil {
		t.Fatal("el hallazgo debe abrir su incidencia")
	}
	if tickets := items(o.do(http.MethodGet, "/api/v1/tickets?q=Inspección", o.admin, nil)); len(tickets) != 1 {
		t.Fatalf("incidencias del hallazgo = %d", len(tickets))
	}
	// Con cargos: primero se retiene, luego se devuelve el resto.
	deposit(o.admin, map[string]any{"type": "returned", "amount": 900, "method": "transfer"}, http.StatusUnprocessableEntity)
	if d := deposit(o.admin, map[string]any{"type": "retained", "amount": 300, "method": "other"}, http.StatusCreated); d["status"] != "retained_partial" {
		t.Fatalf("retención: %v", d["status"])
	}
	deposit(o.admin, map[string]any{"type": "returned", "amount": 1000, "method": "transfer"}, http.StatusUnprocessableEntity)
	d := deposit(o.admin, map[string]any{"type": "returned", "amount": 900, "method": "transfer"}, http.StatusCreated)
	if d["status"] != "retained_partial" || d["balance"].(float64) != 0 || d["retained"].(float64) != 300 {
		t.Fatalf("liquidación: %v", d)
	}
	if queuedEmails(t, "deposit_update") != 3 {
		t.Fatalf("correos del depósito = %d", queuedEmails(t, "deposit_update"))
	}
	// Liquidado: los hallazgos ya no cambian.
	expect(t, o.do(http.MethodDelete, "/api/v1/inspections/"+insp["id"].(string)+"/findings/"+finding["id"].(string), o.super, nil),
		http.StatusUnprocessableEntity)

	detail := o.do(http.MethodGet, "/api/v1/reservations/"+rid, o.admin, nil)
	expect(t, detail, http.StatusOK)
	for _, want := range []string{"created", "confirmed", "deposit_received", "checked_in", "checked_out", "inspection", "finding", "deposit_retained", "deposit_returned"} {
		if !slices.Contains(timeline(detail.body), want) {
			t.Errorf("falta %s en la línea de tiempo: %v", want, timeline(detail.body))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{reservations.EventConfirmed, reservations.EventCheckedIn, reservations.EventCheckedOut} {
		if !slices.Contains(published, want) {
			t.Errorf("no se publicó %s: %v", want, published)
		}
	}
}

func toStrings(v any) []string {
	out := []string{}
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}

// fase04.md §3 y §7: seguridad solo ve lo de hoy y lo que esta en curso; el
// propietario solo lo suyo y sin contacto; limpieza nada; sin la bandera, 403.
func TestReservationScope(t *testing.T) {
	e := setup(t)
	o := e.ops()
	a, b := o.property("TB-1102", nil), o.property("CY-08C", nil)
	expect(t, o.do(http.MethodPut, "/api/v1/properties/"+a+"/owners", o.admin,
		map[string]any{"user_ids": []string{o.ids["ricardo@garrent.gt"].String()}}), http.StatusOK)
	today := todayGT(t)
	direct := o.channel("direct")
	now := o.reserve(map[string]any{
		"property_id": a, "channel_id": direct, "check_in_date": today, "check_out_date": addDays(today, 2),
		"guest": map[string]any{"full_name": "Ana Gómez", "email": "ana@correo.com"},
	})
	later := o.reserve(map[string]any{
		"property_id": b, "channel_id": direct, "check_in_date": addDays(today, 7), "check_out_date": addDays(today, 9),
		"guest": map[string]any{"full_name": "Pablo Díaz", "email": "pablo@correo.com"},
	})

	if got := ids(o.do(http.MethodGet, "/api/v1/reservations", o.security, nil)); !slices.Equal(got, []string{now["id"].(string)}) {
		t.Fatalf("seguridad ve %v", got)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/reservations/"+later["id"].(string), o.security, nil), http.StatusNotFound)

	res := o.do(http.MethodGet, "/api/v1/reservations", o.owner, nil)
	if got := ids(res); !slices.Equal(got, []string{now["id"].(string)}) || items(res)[0]["guest_email"] != nil {
		t.Fatalf("propietario ve %v", items(res))
	}
	detail := o.do(http.MethodGet, "/api/v1/reservations/"+now["id"].(string), o.owner, nil)
	expect(t, detail, http.StatusOK)
	if g := detail.body["guests"].([]any)[0].(map[string]any); g["email"] != nil || g["full_name"] != "Ana Gómez" {
		t.Fatalf("el propietario no ve contacto ni documento: %v", g)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/reservations/"+later["id"].(string), o.owner, nil), http.StatusNotFound)
	expect(t, o.do(http.MethodGet, "/api/v1/reservations", o.cleaner, nil), http.StatusForbidden)
	if got := ids(o.do(http.MethodGet, "/api/v1/reservations", o.super, nil)); len(got) != 2 {
		t.Fatalf("supervisión ve %v", got)
	}

	if err := e.svc.Tenants.SetFeatures(context.Background(), o.tenant, map[string]bool{"reservations": false}); err != nil {
		t.Fatal(err)
	}
	res = o.do(http.MethodGet, "/api/v1/reservations", o.admin, nil)
	expect(t, res, http.StatusForbidden)
	if !strings.Contains(message(res), "no está activo") {
		t.Fatalf("sin la bandera: %s", message(res))
	}
}

// fase04.md §7: solo guests.documents obtiene la URL del documento, cada
// consulta queda en la bitacora, y a los 90 dias se borra con el numero
// conservado.
func TestGuestDocumentsAndRetention(t *testing.T) {
	e := setup(t)
	o := e.ops()
	res := o.do(http.MethodPost, "/api/v1/guests", o.admin, map[string]any{
		"full_name": "Lucía Pérez", "email": "lucia@correo.com", "document_type": "dpi", "document_number": "2456789010101",
	})
	expect(t, res, http.StatusCreated)
	gid := res.body["id"].(string)
	dup := o.do(http.MethodPost, "/api/v1/guests", o.admin, map[string]any{"full_name": "Otra", "document_type": "dpi", "document_number": "2456789010101"})
	expect(t, dup, http.StatusUnprocessableEntity)
	if field(dup) != "body.document_number" {
		t.Fatalf("documento repetido: %s", field(dup))
	}
	file := o.uploadIn(o.admin, "guests", "id_front", "image/jpeg", "frente.jpg")
	expect(t, o.do(http.MethodPost, "/api/v1/guests/"+gid+"/documents", o.admin, map[string]any{"kind": "id_front", "file_id": file}), http.StatusCreated)

	expect(t, o.do(http.MethodGet, "/api/v1/files/"+file+"/url", o.admin, nil), http.StatusNotFound)
	expect(t, o.do(http.MethodGet, "/api/v1/guests/"+gid+"/documents/"+file+"/url", o.super, nil), http.StatusForbidden)
	res = o.do(http.MethodGet, "/api/v1/guests/"+gid+"/documents/"+file+"/url", o.admin, nil)
	expect(t, res, http.StatusOK)
	var views int
	if err := testDB.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM tenant_garrent.audit_log WHERE action = 'guest.document_viewed'`).Scan(&views); err != nil || views != 1 {
		t.Fatalf("consultas en bitácora = %d (%v)", views, err)
	}

	// Lista negra: solo guests.manage, y bloquea reservas nuevas.
	expect(t, o.do(http.MethodPatch, "/api/v1/guests/"+gid, o.admin, map[string]any{"blacklisted": true, "blacklist_reason": "Daños en 2025"}), http.StatusOK)
	prop := o.property("TB-1102", nil)
	today := todayGT(t)
	res = o.do(http.MethodPost, "/api/v1/reservations", o.admin, map[string]any{
		"property_id": prop, "channel_id": o.channel("direct"), "check_in_date": today, "check_out_date": addDays(today, 2), "primary_guest_id": gid,
	})
	expect(t, res, http.StatusUnprocessableEntity)
	if !strings.Contains(message(res), "Daños en 2025") {
		t.Fatalf("lista negra: %s", message(res))
	}

	// Retencion: vence, se borra de S3 y queda el registro.
	if _, err := testDB.Pool().Exec(context.Background(),
		`UPDATE tenant_garrent.files SET expires_at = now() - interval '1 day' WHERE id = $1`, file); err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.Files.Retention(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("retención = %d (%v)", n, err)
	}
	if err := e.svc.Files.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := testDB.Pool().QueryRow(context.Background(),
		`SELECT status FROM tenant_garrent.files WHERE id = $1 AND deleted_at IS NOT NULL`, file).Scan(&status); err != nil || status != "deleted" {
		t.Fatalf("el registro debe quedar deleted: %s (%v)", status, err)
	}
	res = o.do(http.MethodGet, "/api/v1/guests/"+gid, o.admin, nil)
	expect(t, res, http.StatusOK)
	if res.body["document_number"] != "2456789010101" || len(res.body["documents"].([]any)) != 0 {
		t.Fatalf("tras la retención: %v / %v", res.body["document_number"], res.body["documents"])
	}
	expect(t, o.do(http.MethodGet, "/api/v1/guests/"+gid+"/documents/"+file+"/url", o.admin, nil), http.StatusNotFound)
}

// fase04.md §3: el iCal importa estancias por completar, las mueve, cancela
// las que desaparecen, respeta lo corregido a mano y reporta cruces.
func TestIcalSync(t *testing.T) {
	e := setup(t)
	o := e.ops()
	prop := o.property("TB-1102", nil)
	today := todayGT(t)
	ics := func(day string) string { return strings.ReplaceAll(day, "-", "") }
	var mu sync.Mutex
	feed := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cal.ics" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "text/calendar")
		_, _ = fmt.Fprint(w, "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n"+strings.Join(feed, "")+"END:VCALENDAR\r\n")
	}))
	t.Cleanup(srv.Close)
	ev := func(uid, from, to, summary string) string {
		return fmt.Sprintf("BEGIN:VEVENT\r\nUID:%s\r\nDTSTART;VALUE=DATE:%s\r\nDTEND;VALUE=DATE:%s\r\nSUMMARY:%s\r\nEND:VEVENT\r\n", uid, ics(from), ics(to), summary)
	}
	set := func(list ...string) {
		mu.Lock()
		defer mu.Unlock()
		feed = list
	}
	pull := func() map[string]any {
		t.Helper()
		res := o.do(http.MethodPost, "/api/v1/ical-feeds/sync", o.admin, nil)
		expect(t, res, http.StatusOK)
		return items(res)[0]
	}
	airbnb := o.channel("airbnb")

	expect(t, o.do(http.MethodPost, "/api/v1/properties/"+prop+"/ical-feeds", o.admin, map[string]any{"channel_id": airbnb, "url": "ftp://x/cal.ics"}),
		http.StatusUnprocessableEntity)
	res := o.do(http.MethodPost, "/api/v1/properties/"+prop+"/ical-feeds", o.admin, map[string]any{"channel_id": airbnb, "url": srv.URL + "/cal.ics"})
	expect(t, res, http.StatusCreated)
	feedID := res.body["id"].(string)

	set(ev("a@airbnb", addDays(today, 5), addDays(today, 8), "Reserved"), ev("b@airbnb", addDays(today, 10), addDays(today, 12), "Reserved"),
		ev("blk@airbnb", addDays(today, 20), addDays(today, 25), "Airbnb (Not available)"),
		ev("old@airbnb", addDays(today, -10), addDays(today, -7), "Reserved"))
	if r := pull(); r["ok"] != true || r["created"].(float64) != 2 || r["events"].(float64) != 2 {
		t.Fatalf("primera lectura: %v", r)
	}
	res = o.do(http.MethodGet, "/api/v1/reservations?incomplete=true", o.admin, nil)
	if list := items(res); len(list) != 2 || list[0]["source"] != "ical" || list[0]["status"] != "pending" || list[0]["incomplete"] != true {
		t.Fatalf("importadas: %v", list)
	}
	a := items(res)[0]["id"].(string)

	// Se mueve una y desaparece la otra.
	set(ev("a@airbnb", addDays(today, 6), addDays(today, 9), "Reserved"))
	if r := pull(); r["updated"].(float64) != 1 || r["cancelled"].(float64) != 1 {
		t.Fatalf("segunda lectura: %v", r)
	}
	// Corregida a mano, la sincronizacion ya no la mueve.
	expect(t, o.do(http.MethodPatch, "/api/v1/reservations/"+a, o.admin, map[string]any{"check_out_date": addDays(today, 10)}), http.StatusOK)
	set(ev("a@airbnb", addDays(today, 7), addDays(today, 9), "Reserved"))
	if r := pull(); r["updated"].(float64) != 0 {
		t.Fatalf("no debe pisar lo corregido: %v", r)
	}
	if res := o.do(http.MethodGet, "/api/v1/reservations/"+a, o.admin, nil); res.body["check_in_date"] != addDays(today, 6) {
		t.Fatalf("fechas tras corregir: %v", res.body["check_in_date"])
	}
	// Un evento que se cruza con una venta directa no se importa y queda en el calendario.
	direct := o.reserve(map[string]any{
		"property_id": prop, "channel_id": o.channel("direct"), "check_in_date": addDays(today, 14),
		"check_out_date": addDays(today, 16), "guest": map[string]any{"full_name": "Eva Soto"},
	})
	set(ev("a@airbnb", addDays(today, 6), addDays(today, 10), "Reserved"), ev("c@airbnb", addDays(today, 15), addDays(today, 17), "Reserved"))
	if r := pull(); len(r["conflicts"].([]any)) != 1 || r["created"].(float64) != 0 {
		t.Fatalf("cruce: %v", r)
	}
	feeds := items(o.do(http.MethodGet, "/api/v1/ical-feeds", o.admin, nil))
	if msg, _ := feeds[0]["last_error"].(string); !strings.Contains(msg, direct["code"].(string)) {
		t.Fatalf("el calendario debe decir con qué se cruza: %q", msg)
	}
	// Una URL que falla deja el mensaje para la persona.
	expect(t, o.do(http.MethodPatch, "/api/v1/ical-feeds/"+feedID, o.admin, map[string]any{"url": srv.URL + "/no-existe.ics"}), http.StatusOK)
	if r := pull(); r["ok"] != false || r["error"] != "El canal respondió 404." {
		t.Fatalf("error de lectura: %v", r)
	}
}

// fase04.md §3: Booking.com y Airbnb no tienen check-in formal; el trabajo de
// estados marca la entrada y la salida a su hora. El Inicio cuenta llegadas y
// pinta la propiedad ocupada.
func TestAutoStatusAndDashboard(t *testing.T) {
	e := setup(t)
	o := e.ops()
	prop := o.property("TB-1102", nil)
	today := todayGT(t)
	checkedOut := 0
	e.svc.Events.Subscribe(reservations.EventCheckedOut, func(context.Context, database.Querier, events.Event) error {
		checkedOut++
		return nil
	})
	r := o.reserve(map[string]any{
		"property_id": prop, "channel_id": o.channel("booking"), "check_in_date": today, "check_out_date": addDays(today, 1),
		"check_in_time": "00:00", "check_out_time": "11:00", "guest": map[string]any{"full_name": "Hans Müller", "email": "hans@correo.de"},
	})
	run := func() reservations.AutoResult {
		t.Helper()
		var out reservations.AutoResult
		if err := testDB.InTx(garrentCtx(), func(q database.Querier) error {
			var err error
			out, err = e.svc.Reservations.RunAutoStatus(garrentCtx(), q, o.tenant)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := run(); got.CheckedIn != 1 || got.CheckedOut != 0 {
		t.Fatalf("primera pasada: %+v", got)
	}
	dash := o.do(http.MethodGet, "/api/v1/dashboard", o.admin, nil)
	expect(t, dash, http.StatusOK)
	block := dash.body["reservations"].(map[string]any)
	if block["available"] != true || block["arrivals"].(float64) != 1 || block["in_house"].(float64) != 1 {
		t.Fatalf("Inicio: %v", block)
	}
	for _, p := range dash.body["pins"].([]any) {
		if pin := p.(map[string]any); pin["id"] == prop && pin["status"] != "occupied" {
			t.Fatalf("pin: %v", pin)
		}
	}
	e.svc.Reservations.SetClock(func() time.Time { return time.Now().Add(48 * time.Hour) })
	defer e.svc.Reservations.SetClock(time.Now)
	if got := run(); got.CheckedOut != 1 || checkedOut != 1 {
		t.Fatalf("segunda pasada: %+v (%d eventos)", got, checkedOut)
	}
	res := o.do(http.MethodGet, "/api/v1/reservations/"+r["id"].(string), o.admin, nil)
	if res.body["status"] != "checked_out" {
		t.Fatalf("estado final: %v", res.body["status"])
	}
}

// fase04.md §3: cancelar pide motivo y avisa al huesped; no-show solo desde
// el dia de entrada; confirmar una importada requiere huesped.
func TestCancelNoShowAndConfirm(t *testing.T) {
	e := setup(t)
	o := e.ops()
	prop := o.property("TB-1102", nil)
	today := todayGT(t)
	direct := o.channel("direct")
	future := o.reserve(map[string]any{
		"property_id": prop, "channel_id": direct, "check_in_date": addDays(today, 5),
		"check_out_date": addDays(today, 7), "guest": map[string]any{"full_name": "Ana Gómez", "email": "ana@correo.com"},
	})
	id := future["id"].(string)
	expect(t, o.do(http.MethodPost, "/api/v1/reservations/"+id+"/no-show", o.admin, map[string]any{}), http.StatusUnprocessableEntity)
	res := o.do(http.MethodPost, "/api/v1/reservations/"+id+"/cancel", o.admin, map[string]any{})
	expect(t, res, http.StatusUnprocessableEntity)
	if field(res) != "body.reason" {
		t.Fatalf("cancelar sin motivo: %s", field(res))
	}
	res = o.do(http.MethodPost, "/api/v1/reservations/"+id+"/cancel", o.admin, map[string]any{"reason": "Lo pidió el huésped"})
	expect(t, res, http.StatusOK)
	if res.body["status"] != "cancelled" || queuedEmails(t, "reservation_cancelled") != 1 {
		t.Fatalf("cancelada: %v", res.body["status"])
	}
	// Las fechas quedan libres.
	o.reserve(map[string]any{"property_id": prop, "channel_id": direct, "check_in_date": addDays(today, 5), "check_out_date": addDays(today, 7)})

	// Por confirmar: sin huesped no se confirma; al agregarlo, si.
	pending := o.reserve(map[string]any{"property_id": prop, "channel_id": direct, "check_in_date": addDays(today, 10), "check_out_date": addDays(today, 12)})
	pid := pending["id"].(string)
	if pending["status"] != "pending" || pending["incomplete"] != true {
		t.Fatalf("sin huésped: %v", pending["status"])
	}
	expect(t, o.do(http.MethodPost, "/api/v1/reservations/"+pid+"/confirm", o.admin, map[string]any{}), http.StatusUnprocessableEntity)
	res = o.do(http.MethodPost, "/api/v1/reservations/"+pid+"/guests", o.admin, map[string]any{"guest": map[string]any{"full_name": "Mario Ruiz", "email": "mario@correo.com"}})
	expect(t, res, http.StatusOK)
	if res.body["incomplete"] != false {
		t.Fatal("con huésped ya no está incompleta")
	}
	res = o.do(http.MethodPost, "/api/v1/reservations/"+pid+"/confirm", o.admin, map[string]any{"send_email": false})
	expect(t, res, http.StatusOK)
	if res.body["status"] != "confirmed" || queuedEmails(t, "reservation_confirmed") != 1 {
		t.Fatalf("confirmar sin correo: %v (%d correos)", res.body["status"], queuedEmails(t, "reservation_confirmed"))
	}
	expect(t, o.do(http.MethodPost, "/api/v1/reservations/"+pid+"/resend", o.admin, nil), http.StatusOK)
	if queuedEmails(t, "reservation_confirmed") != 2 {
		t.Fatal("reenviar debe encolar otro correo")
	}
}
