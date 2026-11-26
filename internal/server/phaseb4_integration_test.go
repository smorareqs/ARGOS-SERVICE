//go:build integration

package server_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smorareqs/argos-service/internal/core/database"
)

// Pruebas de la fase 4 de Persona B (fase04.md §7): acceso del huesped con
// enlace y codigo, su alcance, parqueos con cruces, areas comunes bajo
// concurrencia, problema reportado por el huesped y alarmas de salida.

// accessFromEmail lee el enlace y el codigo del ultimo correo de confirmacion
// encolado para ese correo.
func accessFromEmail(t *testing.T, email string) (link, code string) {
	t.Helper()
	if err := testDB.Pool().QueryRow(context.Background(), `
		SELECT payload->'data'->>'AppLink', payload->'data'->>'AppCode' FROM shared.jobs
		WHERE type = 'send_email' AND payload->>'template' = 'reservation_confirmed' AND payload->>'to' = $1
		ORDER BY created_at DESC LIMIT 1`, email).Scan(&link, &code); err != nil {
		t.Fatal(err)
	}
	return link, code
}

func TestGuestParkingAreasAndSecurity(t *testing.T) {
	e := setup(t)
	o := e.ops()
	if err := e.svc.Tenants.SetFeatures(context.Background(), o.tenant,
		map[string]bool{"reservations": true, "parking": true, "shared_areas": true}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	building := o.do(http.MethodPost, "/api/v1/buildings", o.admin, map[string]any{"name": "Torre Botánica", "zone": "Zona 10"})
	expect(t, building, http.StatusCreated)
	bid := building.body["id"].(string)
	p1 := o.property("TB-504", map[string]any{"building_id": bid})
	p2 := o.property("TB-702", map[string]any{"building_id": bid})
	today := todayGT(t)
	direct := o.channel("direct")

	ra := o.reserve(map[string]any{
		"property_id": p1, "channel_id": direct, "check_in_date": today, "check_out_date": addDays(today, 3),
		"guest": map[string]any{"full_name": "Valeria Gómez", "email": "valeria@correo.com"}, "send_email": true,
	})
	rb := o.reserve(map[string]any{
		"property_id": p2, "channel_id": direct, "check_in_date": today, "check_out_date": addDays(today, 3),
		"guest": map[string]any{"full_name": "Emily Carter", "email": "emily@correo.com"}, "send_email": true,
	})

	// --- Acceso del huesped ---------------------------------------------------
	linkA, codeA := accessFromEmail(t, "valeria@correo.com")
	if !strings.HasPrefix(linkA, "argos://guest?token=") || len(codeA) != 9 {
		t.Fatalf("acceso en el correo: %q %q", linkA, codeA)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/guest/auth", "", map[string]any{"code": codeA, "email": "otra@correo.com"}), http.StatusUnauthorized)
	expect(t, o.do(http.MethodPost, "/api/v1/guest/auth", "", map[string]any{"token": "no-existe"}), http.StatusUnauthorized)
	expect(t, o.do(http.MethodPost, "/api/v1/guest/auth", "", map[string]any{}), http.StatusUnprocessableEntity)
	auth := o.do(http.MethodPost, "/api/v1/guest/auth", "", map[string]any{"code": strings.ToLower(codeA), "email": " Valeria@Correo.com "})
	expect(t, auth, http.StatusOK)
	guestA := auth.body["access_token"].(string)
	if me := auth.body["me"].(map[string]any); me["reservation_code"] != ra["code"] || me["features"].(map[string]any)["parking"] != true {
		t.Fatalf("me: %v", me)
	}
	linkB, _ := accessFromEmail(t, "emily@correo.com")
	authB := o.do(http.MethodPost, "/api/v1/guest/auth", "", map[string]any{"token": strings.TrimPrefix(linkB, "argos://guest?token=")})
	expect(t, authB, http.StatusOK)
	guestB := authB.body["access_token"].(string)

	res := o.do(http.MethodGet, "/api/v1/guest/reservation", guestA, nil)
	expect(t, res, http.StatusOK)
	if res.body["code"] != ra["code"] || res.body["property_code"] != "TB-504" || res.body["building"] != "Torre Botánica" {
		t.Fatalf("mi reserva: %v", res.body)
	}
	// El token del huesped no sirve en la API del personal ni al reves.
	expect(t, o.do(http.MethodGet, "/api/v1/properties", guestA, nil), http.StatusUnauthorized)
	expect(t, o.do(http.MethodGet, "/api/v1/guest/me", o.admin, nil), http.StatusUnauthorized)
	expect(t, o.do(http.MethodGet, "/api/v1/guest/me", "", nil), http.StatusUnauthorized)

	// --- Parqueos ---------------------------------------------------------------
	space := o.do(http.MethodPost, "/api/v1/parking/spaces", o.admin, map[string]any{"building_id": bid, "code": "s2-01", "level": "Sótano 2", "covered": true})
	expect(t, space, http.StatusCreated)
	sid := space.body["id"].(string)
	if space.body["code"] != "S2-01" || space.body["building"] != "Torre Botánica" {
		t.Fatalf("espacio: %v", space.body)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/parking/spaces", o.admin, map[string]any{"building_id": bid, "code": "S2-01"}), http.StatusUnprocessableEntity)
	expect(t, o.do(http.MethodGet, "/api/v1/parking/spaces", o.security, nil), http.StatusOK)
	expect(t, o.do(http.MethodPost, "/api/v1/parking/spaces", o.security, map[string]any{"building_id": bid, "code": "X"}), http.StatusForbidden)
	stays := o.do(http.MethodGet, "/api/v1/parking/assignable-stays", o.admin, nil)
	expect(t, stays, http.StatusOK)
	if len(stays.list) != 2 {
		t.Fatalf("estancias sin parqueo: %v", stays.list)
	}
	bad := o.do(http.MethodPost, "/api/v1/parking/assignments", o.admin, map[string]any{"space_id": sid, "reservation_code": ra["code"], "plate": "??"})
	expect(t, bad, http.StatusUnprocessableEntity)
	if !strings.Contains(message(bad), "P123ABC") {
		t.Errorf("placa: %s", message(bad))
	}
	assigned := o.do(http.MethodPost, "/api/v1/parking/assignments", o.admin, map[string]any{"space_id": sid, "reservation_code": ra["code"], "plate": "p 123 abc"})
	expect(t, assigned, http.StatusCreated)
	if assigned.body["plate"] != "P123ABC" || assigned.body["from"] != today || assigned.body["to"] != addDays(today, 3) {
		t.Fatalf("asignacion: %v", assigned.body)
	}
	clash := o.do(http.MethodPost, "/api/v1/parking/assignments", o.admin, map[string]any{"space_id": sid, "reservation_code": rb["code"], "plate": "P987XYZ"})
	expect(t, clash, http.StatusConflict)
	if !strings.Contains(message(clash), "Ocupado por P123ABC (Valeria Gómez)") {
		t.Errorf("cruce: %s", message(clash))
	}
	// La BD tambien rechaza el cruce aunque alguien salte la validacion.
	if _, err := testDB.Pool().Exec(ctx, `
		INSERT INTO tenant_garrent.parking_assignments (spot_id, reservation_id, vehicle_plate, from_date, to_date)
		VALUES ($1, $2, 'P000AAA', $3, $4)`, sid, rb["id"], addDays(today, 1), addDays(today, 2)); err == nil || !strings.Contains(err.Error(), "ex_parking_assignments_overlap") {
		t.Fatalf("la BD debe rechazar el cruce: %v", err)
	}
	gp := o.do(http.MethodGet, "/api/v1/guest/parking", guestA, nil)
	expect(t, gp, http.StatusOK)
	if len(gp.list) != 1 || gp.list[0].(map[string]any)["space_code"] != "S2-01" {
		t.Fatalf("mi parqueo: %v", gp.list)
	}
	if gpb := o.do(http.MethodGet, "/api/v1/guest/parking", guestB, nil); len(gpb.list) != 0 {
		t.Fatalf("el huesped B no ve el parqueo de A: %v", gpb.list)
	}

	// --- Areas comunes ------------------------------------------------------------
	area := o.do(http.MethodPost, "/api/v1/shared-areas", o.admin, map[string]any{
		"building_id": bid, "name": "Piscina de la terraza", "capacity": 1, "opens": "07:00", "closes": "21:00", "block": 60,
		"max_per_day": 2, "advance_hours": 0, "photo": "beachPool",
	})
	expect(t, area, http.StatusCreated)
	aid := area.body["id"].(string)
	saved := area.body
	saved["closes"] = "06:00"
	bad = o.do(http.MethodPut, "/api/v1/shared-areas/"+aid, o.admin, saved)
	expect(t, bad, http.StatusUnprocessableEntity)
	if loc := bad.body["errors"].([]any)[0].(map[string]any)["location"]; loc != "body.closes" {
		t.Errorf("campo: %v", loc)
	}
	tomorrow := addDays(today, 1)
	ga := o.do(http.MethodGet, "/api/v1/guest/shared-areas", guestA, nil)
	expect(t, ga, http.StatusOK)
	if len(ga.body["areas"].([]any)) != 1 {
		t.Fatalf("areas del edificio: %v", ga.body)
	}
	// Capacidad 1 y dos huespedes a la vez: solo uno entra.
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, tok := range []string{guestA, guestB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = o.do(http.MethodPost, "/api/v1/guest/shared-areas/"+aid+"/bookings", tok, map[string]any{"date": tomorrow, "start": "10:00", "people": 1}).status
		}()
	}
	wg.Wait()
	if !(codes[0] == http.StatusCreated && codes[1] == http.StatusUnprocessableEntity) && !(codes[1] == http.StatusCreated && codes[0] == http.StatusUnprocessableEntity) {
		t.Fatalf("concurrencia con capacidad 1: %v", codes)
	}
	avail := o.do(http.MethodGet, "/api/v1/guest/shared-areas/"+aid+"/availability?date="+tomorrow, guestA, nil)
	expect(t, avail, http.StatusOK)
	for _, s := range avail.body["slots"].([]any) {
		if sl := s.(map[string]any); sl["start"] == "10:00" && sl["left"] != float64(0) {
			t.Errorf("bloque lleno: %v", sl)
		}
	}
	expect(t, o.do(http.MethodPost, "/api/v1/guest/shared-areas/"+aid+"/bookings", guestA,
		map[string]any{"date": addDays(today, 5), "start": "10:00", "people": 1}), http.StatusUnprocessableEntity) // fuera de la estancia
	expect(t, o.do(http.MethodPost, "/api/v1/guest/shared-areas/"+aid+"/bookings", guestA,
		map[string]any{"date": tomorrow, "start": "21:00", "people": 1}), http.StatusUnprocessableEntity) // cerrado
	staff := o.do(http.MethodPost, "/api/v1/shared-areas/bookings", o.super, map[string]any{
		"area_id": aid, "date": tomorrow, "start": "12:00", "guest": "Emily Carter|TB-702", "people": 1,
	})
	expect(t, staff, http.StatusCreated)
	if staff.body["by_staff"] != true || staff.body["property_code"] != "TB-702" || staff.body["end"] != "13:00" {
		t.Fatalf("reserva del personal: %v", staff.body)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/shared-areas/bookings", o.super, map[string]any{
		"area_id": aid, "date": tomorrow, "start": "14:00", "guest": "Nadie|TB-702", "people": 1,
	}), http.StatusUnprocessableEntity)
	// El huesped A no cancela lo de B (404) y si lo suyo.
	expect(t, o.do(http.MethodDelete, "/api/v1/guest/bookings/"+staff.body["id"].(string), guestA, nil), http.StatusNotFound)
	mine := o.do(http.MethodGet, "/api/v1/guest/bookings", guestB, nil)
	expect(t, mine, http.StatusOK)
	if len(mine.list) == 0 {
		t.Fatal("B debe ver su bloque")
	}
	expect(t, o.do(http.MethodDelete, "/api/v1/guest/bookings/"+staff.body["id"].(string), guestB, nil), http.StatusNoContent)
	list := o.do(http.MethodGet, "/api/v1/shared-areas/bookings?from="+today+"&to="+addDays(today, 7), o.security, nil)
	expect(t, list, http.StatusOK)
	if len(list.list) != 1 {
		t.Fatalf("bloques confirmados: %v", list.list)
	}
	inHouse := o.do(http.MethodGet, "/api/v1/reservations/in-house", o.super, nil)
	expect(t, inHouse, http.StatusOK)
	if len(inHouse.list) != 2 {
		t.Fatalf("en casa: %v", inHouse.list)
	}

	// --- Problema reportado por el huesped -----------------------------------------
	tk := o.do(http.MethodPost, "/api/v1/guest/tickets", guestA, map[string]any{"title": "No sale agua caliente", "urgency": "high"})
	expect(t, tk, http.StatusCreated)
	detail := o.do(http.MethodGet, "/api/v1/tickets/"+tk.body["id"].(string), o.admin, nil)
	expect(t, detail, http.StatusOK)
	if detail.body["reporter_name"] != "Valeria Gómez (huésped)" || detail.body["property_code"] != "TB-504" {
		t.Fatalf("incidencia del huesped: %v %v", detail.body["reporter_name"], detail.body["property_code"])
	}
	expect(t, o.do(http.MethodPost, "/api/v1/guest/device", guestA, map[string]any{"token": "fcm-token-valeria"}), http.StatusNoContent)

	// --- Seguridad: tablero y alarmas ---------------------------------------------
	dash := o.do(http.MethodGet, "/api/v1/security/dashboard", o.security, nil)
	expect(t, dash, http.StatusOK)
	counts := dash.body["counts"].(map[string]any)
	if counts["arrivals"] != float64(2) || counts["plates"] != float64(1) {
		t.Fatalf("tablero: %v", counts)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/security/dashboard", o.cleaner, nil), http.StatusForbidden)
	var scheduled int
	if err := testDB.Pool().QueryRow(ctx, `SELECT count(*) FROM tenant_garrent.security_alarms WHERE reservation_id = $1 AND status = 'scheduled'`, ra["id"]).Scan(&scheduled); err != nil || scheduled != 2 {
		t.Fatalf("alarmas programadas = %d (%v)", scheduled, err)
	}
	// Cancelar B cancela sus alarmas y su parqueo.
	expect(t, o.do(http.MethodPost, "/api/v1/reservations/"+rb["id"].(string)+"/cancel", o.admin, map[string]any{"reason": "Cambio de planes"}), http.StatusOK)
	var live int
	if err := testDB.Pool().QueryRow(ctx, `SELECT count(*) FROM tenant_garrent.security_alarms WHERE reservation_id = $1 AND status <> 'cancelled'`, rb["id"]).Scan(&live); err != nil || live != 0 {
		t.Fatalf("alarmas de B = %d (%v)", live, err)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/guest/me", guestB, nil), http.StatusUnauthorized)

	tctx := database.WithTenantSchema(ctx, "tenant_garrent")
	run := func(at time.Time) (int, int) {
		t.Helper()
		e.svc.Security.SetClock(func() time.Time { return at })
		var fired, late int
		if err := testDB.InTx(tctx, func(q database.Querier) error {
			var err error
			fired, late, err = e.svc.Security.Run(tctx, q, o.tenant)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return fired, late
	}
	defer e.svc.Security.SetClock(time.Now)
	var fireAt time.Time
	if err := testDB.Pool().QueryRow(ctx, `SELECT min(fire_at) FROM tenant_garrent.security_alarms WHERE reservation_id = $1`, ra["id"]).Scan(&fireAt); err != nil {
		t.Fatal(err)
	}
	if f, l := run(fireAt.Add(-time.Minute)); f != 0 || l != 0 {
		t.Fatalf("antes de la hora: %d %d", f, l)
	}
	if f, _ := run(fireAt.Add(time.Minute)); f != 1 {
		t.Fatalf("la previa debe sonar: %d", f)
	}
	var notified int
	if err := testDB.Pool().QueryRow(ctx, `SELECT count(*) FROM tenant_garrent.notifications WHERE type = 'checkout_alarm' AND recipient_user_id = $1`,
		o.ids["sofia@garrent.gt"]).Scan(&notified); err != nil || notified != 1 {
		t.Fatalf("aviso a seguridad = %d (%v)", notified, err)
	}
	if f, l := run(fireAt.Add(61 * time.Minute)); f != 1 || l != 1 {
		t.Fatalf("la exacta suena y la previa se escala: %d %d", f, l)
	}
	if _, l := run(fireAt.Add(62 * time.Minute)); l != 0 {
		t.Fatal("la alerta se levanta una sola vez")
	}
	alarms := o.do(http.MethodGet, "/api/v1/security/alarms", o.security, nil)
	expect(t, alarms, http.StatusOK)
	if len(alarms.list) != 2 {
		t.Fatalf("alarmas sonando: %v", alarms.list)
	}
	first := alarms.list[0].(map[string]any)
	if plates := first["plates"].([]any); len(plates) != 1 || plates[0] != "P123ABC" {
		t.Errorf("placas en la alarma: %v", first["plates"])
	}
	att := o.do(http.MethodPost, "/api/v1/security/alarms/"+first["id"].(string)+"/attend", o.security, map[string]any{"notes": "Entregó llaves"})
	expect(t, att, http.StatusOK)
	if att.body["status"] != "attended" || att.body["attended_by_name"] == nil {
		t.Fatalf("atendida: %v", att.body)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/security/alarms/"+first["id"].(string)+"/attend", o.security, nil), http.StatusUnprocessableEntity)
	if rest := o.do(http.MethodGet, "/api/v1/security/alarms", o.security, nil); len(rest.list) != 0 {
		t.Fatalf("atender una marca las dos: %v", rest.list)
	}
	var openAlerts int
	if err := testDB.Pool().QueryRow(ctx, `SELECT count(*) FROM tenant_garrent.alerts WHERE type = 'checkout_alarm_unattended' AND status <> 'resolved'`).Scan(&openAlerts); err != nil || openAlerts != 0 {
		t.Fatalf("alerta sin resolver = %d (%v)", openAlerts, err)
	}

	// Reenviar la confirmacion revoca el acceso anterior.
	expect(t, o.do(http.MethodPost, "/api/v1/reservations/"+ra["id"].(string)+"/resend", o.admin, nil), http.StatusOK)
	expect(t, o.do(http.MethodGet, "/api/v1/guest/me", guestA, nil), http.StatusUnauthorized)

	// Sin la bandera, parqueos responde 403.
	if err := e.svc.Tenants.SetFeatures(ctx, o.tenant, map[string]bool{"reservations": true}); err != nil {
		t.Fatal(err)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/parking/spaces", o.admin, nil), http.StatusForbidden)
}
