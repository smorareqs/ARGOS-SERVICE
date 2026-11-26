//go:build integration

package server_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/chat"
)

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Pruebas de la fase 6 de Persona B (fase06.md §2, §4.2 y §5): chat interno
// y con huespedes, cerraduras con su codigo y la retencion.

func jobsOfType(t *testing.T, typ string) int {
	t.Helper()
	var n int
	if err := testDB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM shared.jobs WHERE type = $1`, typ).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestChatInternalAndGuest(t *testing.T) {
	e := setup(t)
	o := e.ops()
	if err := e.svc.Tenants.SetFeatures(context.Background(), o.tenant,
		map[string]bool{"reservations": true, "guest_chat": true}); err != nil {
		t.Fatal(err)
	}
	maria, luis, jorge := o.ids["maria@garrent.gt"].String(), o.ids["luis@garrent.gt"].String(), o.ids["jorge@garrent.gt"].String()

	// --- Interno -------------------------------------------------------------
	expect(t, o.do(http.MethodGet, "/api/v1/chat/conversations", o.owner, nil), http.StatusForbidden)
	people := o.do(http.MethodGet, "/api/v1/chat/people", o.cleaner, nil)
	expect(t, people, http.StatusOK)
	names := map[string]bool{}
	for _, p := range people.list {
		names[p.(map[string]any)["name"].(string)] = true
	}
	if len(people.list) < 3 || names[""] {
		t.Fatalf("personas: %v", people.list)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/chat/conversations", o.cleaner, map[string]any{"user_ids": []string{maria}}), http.StatusUnprocessableEntity)
	dm := o.do(http.MethodPost, "/api/v1/chat/conversations", o.cleaner, map[string]any{"kind": "internal", "user_ids": []string{luis}})
	expect(t, dm, http.StatusCreated)
	dmID := dm.body["id"].(string)
	if dm.body["kind"] != "internal" || dm.body["title"] == "" || len(dm.body["participants"].([]any)) != 2 {
		t.Fatalf("1:1: %v", dm.body)
	}
	again := o.do(http.MethodPost, "/api/v1/chat/conversations", o.cleaner, map[string]any{"user_ids": []string{luis}})
	expect(t, again, http.StatusCreated)
	if again.body["id"] != dmID {
		t.Fatalf("la 1:1 se reutiliza: %v", again.body)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/chat/conversations/"+dmID+"/messages", o.cleaner, map[string]any{"body": "   "}), http.StatusUnprocessableEntity)
	sent := o.do(http.MethodPost, "/api/v1/chat/conversations/"+dmID+"/messages", o.cleaner, map[string]any{"body": "¿Me pasas la escalera?"})
	expect(t, sent, http.StatusCreated)
	if sent.body["mine"] != true || sent.body["sender_type"] != "user" || sent.body["body"] != "¿Me pasas la escalera?" {
		t.Fatalf("mensaje: %v", sent.body)
	}
	if jobsOfType(t, "send_user_push") != 1 {
		t.Fatalf("push a Luis")
	}
	list := o.do(http.MethodGet, "/api/v1/chat/conversations?kind=internal", o.maint, nil)
	expect(t, list, http.StatusOK)
	convs := items(list)
	if len(convs) != 1 || convs[0]["unread"] != float64(1) || convs[0]["last_message"].(map[string]any)["mine"] != false {
		t.Fatalf("lista de Luis: %v", convs)
	}
	msgs := o.do(http.MethodGet, "/api/v1/chat/conversations/"+dmID+"/messages", o.maint, nil)
	expect(t, msgs, http.StatusOK)
	if len(items(msgs)) != 1 || msgs.body["next_cursor"] != nil {
		t.Fatalf("mensajes: %v", msgs.body)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/chat/conversations/"+dmID+"/read", o.maint, nil), http.StatusNoContent)
	if c := items(o.do(http.MethodGet, "/api/v1/chat/conversations", o.maint, nil)); c[0]["unread"] != float64(0) {
		t.Fatalf("leida: %v", c)
	}
	// Quien no participa no la ve.
	expect(t, o.do(http.MethodGet, "/api/v1/chat/conversations/"+dmID+"/messages", o.security, nil), http.StatusNotFound)
	expect(t, o.do(http.MethodPost, "/api/v1/chat/conversations/"+dmID+"/messages", o.security, map[string]any{"body": "hola"}), http.StatusNotFound)
	// Paginacion hacia atras.
	for _, b := range []string{"uno", "dos", "tres"} {
		expect(t, o.do(http.MethodPost, "/api/v1/chat/conversations/"+dmID+"/messages", o.maint, map[string]any{"body": b}), http.StatusCreated)
	}
	page := o.do(http.MethodGet, "/api/v1/chat/conversations/"+dmID+"/messages?limit=2", o.cleaner, nil)
	if got := items(page); len(got) != 2 || got[0]["body"] != "dos" || got[1]["body"] != "tres" || page.body["next_cursor"] == nil {
		t.Fatalf("pagina 1: %v", page.body)
	}
	page = o.do(http.MethodGet, "/api/v1/chat/conversations/"+dmID+"/messages?limit=2&before="+page.body["next_cursor"].(string), o.cleaner, nil)
	if got := items(page); len(got) != 2 || got[0]["body"] != "¿Me pasas la escalera?" || got[1]["body"] != "uno" {
		t.Fatalf("pagina 2: %v", page.body)
	}

	// Grupo: el admin crea, suma y saca; un colaborador no saca a otros.
	group := o.do(http.MethodPost, "/api/v1/chat/conversations", o.admin, map[string]any{"title": "Turno mañana", "user_ids": []string{maria, luis}})
	expect(t, group, http.StatusCreated)
	gid := group.body["id"].(string)
	if group.body["title"] != "Turno mañana" || len(group.body["participants"].([]any)) != 3 {
		t.Fatalf("grupo: %v", group.body)
	}
	added := o.do(http.MethodPost, "/api/v1/chat/conversations/"+gid+"/participants", o.cleaner, map[string]any{"user_id": jorge})
	expect(t, added, http.StatusOK)
	expect(t, o.do(http.MethodPost, "/api/v1/chat/conversations/"+gid+"/participants", o.cleaner, map[string]any{"user_id": jorge}), http.StatusConflict)
	expect(t, o.do(http.MethodDelete, "/api/v1/chat/conversations/"+gid+"/participants/"+luis, o.cleaner, nil), http.StatusForbidden)
	expect(t, o.do(http.MethodDelete, "/api/v1/chat/conversations/"+gid+"/participants/"+jorge, o.super, nil), http.StatusNoContent)
	expect(t, o.do(http.MethodGet, "/api/v1/chat/conversations/"+gid, o.super, nil), http.StatusNotFound)
	muted := o.do(http.MethodPost, "/api/v1/chat/conversations/"+gid+"/mute", o.maint, map[string]any{"muted": true})
	expect(t, muted, http.StatusOK)
	if muted.body["muted"] != true {
		t.Fatalf("silenciada: %v", muted.body)
	}
	pushes := jobsOfType(t, "send_user_push")
	expect(t, o.do(http.MethodPost, "/api/v1/chat/conversations/"+gid+"/messages", o.admin, map[string]any{"body": "Buen día"}), http.StatusCreated)
	if jobsOfType(t, "send_user_push") != pushes+1 {
		t.Fatalf("Luis silencio el grupo: solo María recibe push")
	}

	// --- Huesped ---------------------------------------------------------------
	prop := o.property("CH-101", nil)
	today := todayGT(t)
	r := o.reserve(map[string]any{
		"property_id": prop, "channel_id": o.channel("direct"), "check_in_date": today, "check_out_date": addDays(today, 2),
		"guest": map[string]any{"full_name": "Valeria Gómez", "email": "valeria@correo.com"}, "send_email": true,
	})
	guestList := o.do(http.MethodGet, "/api/v1/chat/conversations?kind=guest", o.super, nil)
	expect(t, guestList, http.StatusOK)
	gc := items(guestList)
	if len(gc) != 1 || gc[0]["title"] != "Valeria Gómez" || gc[0]["reservation"].(map[string]any)["code"] != r["code"] ||
		gc[0]["last_message"].(map[string]any)["sender_type"] != "system" {
		t.Fatalf("conversación del huésped: %v", gc)
	}
	guestConv := gc[0]["id"].(string)
	if c := items(o.do(http.MethodGet, "/api/v1/chat/conversations?kind=guest", o.cleaner, nil)); len(c) != 0 {
		t.Fatalf("la limpieza no ve chats de huéspedes: %v", c)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/chat/conversations/"+guestConv+"/messages", o.cleaner, nil), http.StatusNotFound)

	_, code := accessFromEmail(t, "valeria@correo.com")
	auth := o.do(http.MethodPost, "/api/v1/guest/auth", "", map[string]any{"code": code, "email": "valeria@correo.com"})
	expect(t, auth, http.StatusOK)
	guest := auth.body["access_token"].(string)
	if f := auth.body["me"].(map[string]any)["features"].(map[string]any); f["guest_chat"] != true || f["smart_locks"] != false {
		t.Fatalf("banderas del huésped: %v", f)
	}
	gv := o.do(http.MethodGet, "/api/v1/guest/chat", guest, nil)
	expect(t, gv, http.StatusOK)
	if gv.body["id"] != guestConv || gv.body["title"] != "Garrent" || len(gv.body["messages"].([]any)) != 1 || gv.body["unread"] != float64(1) {
		t.Fatalf("chat del huésped: %v", gv.body)
	}
	if !strings.Contains(gv.body["messages"].([]any)[0].(map[string]any)["body"].(string), "Valeria") {
		t.Fatalf("saludo: %v", gv.body["messages"])
	}
	expect(t, o.do(http.MethodPost, "/api/v1/guest/chat/messages", guest, map[string]any{"body": ""}), http.StatusUnprocessableEntity)
	gs := o.do(http.MethodPost, "/api/v1/guest/chat/messages", guest, map[string]any{"body": "¿A qué hora puedo entrar?"})
	expect(t, gs, http.StatusCreated)
	if gs.body["mine"] != true || gs.body["sender_type"] != "guest" || gs.body["sender_name"] != "Valeria Gómez" {
		t.Fatalf("mensaje del huésped: %v", gs.body)
	}
	if c := items(o.do(http.MethodGet, "/api/v1/chat/conversations?kind=guest", o.super, nil)); c[0]["unread"] != float64(1) {
		t.Fatalf("no leído para el supervisor: %v", c)
	}
	reply := o.do(http.MethodPost, "/api/v1/chat/conversations/"+guestConv+"/messages", o.super, map[string]any{"body": "Desde las 15:00."})
	expect(t, reply, http.StatusCreated)
	expect(t, o.do(http.MethodPost, "/api/v1/guest/chat/read", guest, nil), http.StatusNoContent)
	gv = o.do(http.MethodGet, "/api/v1/guest/chat", guest, nil)
	msgsG := gv.body["messages"].([]any)
	if len(msgsG) != 3 || gv.body["unread"] != float64(0) || msgsG[2].(map[string]any)["mine"] != false || len(gv.body["participants"].([]any)) < 1 {
		t.Fatalf("chat del huésped con respuesta: %v", gv.body)
	}

	// Al cancelar se archiva: el personal ya no escribe y el huesped pierde acceso.
	expect(t, o.do(http.MethodPost, "/api/v1/reservations/"+r["id"].(string)+"/cancel", o.admin, map[string]any{"reason": "Cambio de planes"}), http.StatusOK)
	expect(t, o.do(http.MethodPost, "/api/v1/chat/conversations/"+guestConv+"/messages", o.super, map[string]any{"body": "¿Sigues?"}), http.StatusUnprocessableEntity)
	expect(t, o.do(http.MethodGet, "/api/v1/guest/chat", guest, nil), http.StatusUnauthorized)
	if c := items(o.do(http.MethodGet, "/api/v1/chat/conversations?kind=guest", o.admin, nil)); len(c) != 1 || c[0]["is_archived"] != true {
		t.Fatalf("archivada: %v", c)
	}

	// Sin la bandera el huesped no tiene chat.
	if err := e.svc.Tenants.SetFeatures(context.Background(), o.tenant, map[string]bool{"reservations": true}); err != nil {
		t.Fatal(err)
	}
	r2 := o.reserve(map[string]any{
		"property_id": prop, "channel_id": o.channel("direct"), "check_in_date": addDays(today, 5), "check_out_date": addDays(today, 6),
		"guest": map[string]any{"full_name": "Tomás Reyes", "email": "tomas@correo.com"}, "send_email": true,
	})
	_ = r2
	_, code2 := accessFromEmail(t, "tomas@correo.com")
	auth2 := o.do(http.MethodPost, "/api/v1/guest/auth", "", map[string]any{"code": code2, "email": "tomas@correo.com"})
	expect(t, auth2, http.StatusOK)
	expect(t, o.do(http.MethodGet, "/api/v1/guest/chat", auth2.body["access_token"].(string), nil), http.StatusForbidden)

	// Job de archivo: no falla y deja las de reservas vivas sin archivar.
	ctx := database.WithTenantSchema(context.Background(), "tenant_garrent")
	if err := testDB.InTx(ctx, func(q database.Querier) error {
		_, err := chat.Archive(ctx, q, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLocksAndRetention(t *testing.T) {
	e := setup(t)
	o := e.ops()
	bg := context.Background()
	if err := e.svc.Tenants.SetFeatures(bg, o.tenant, map[string]bool{"reservations": true, "smart_locks": true}); err != nil {
		t.Fatal(err)
	}
	prop := o.property("LK-201", nil)
	bad := o.property("LK-202", nil)
	expect(t, o.do(http.MethodPost, "/api/v1/locks/devices", o.super, map[string]any{"property_id": prop, "name": "Puerta", "external_id": "nx-1"}), http.StatusForbidden)
	dev := o.do(http.MethodPost, "/api/v1/locks/devices", o.admin, map[string]any{
		"property_id": prop, "name": "Puerta principal", "external_id": "nx-1", "credentials": "secreto",
	})
	expect(t, dev, http.StatusCreated)
	if dev.body["has_credentials"] != true || dev.body["provider"] != "nexxt" || dev.body["credentials"] != nil {
		t.Fatalf("cerradura: %v", dev.body)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/locks/devices", o.admin, map[string]any{"property_id": prop, "name": "Otra", "external_id": "nx-1"}), http.StatusConflict)
	expect(t, o.do(http.MethodPost, "/api/v1/locks/devices", o.admin, map[string]any{"property_id": bad, "name": "Rota", "external_id": "fail-1"}), http.StatusCreated)
	if l := o.do(http.MethodGet, "/api/v1/locks/devices", o.admin, nil); len(l.list) != 2 {
		t.Fatalf("cerraduras: %v", l.list)
	}

	today := todayGT(t)
	direct := o.channel("direct")
	r := o.reserve(map[string]any{
		"property_id": prop, "channel_id": direct, "check_in_date": today, "check_out_date": addDays(today, 2),
		"guest": map[string]any{"full_name": "Valeria Gómez", "email": "valeria@correo.com"}, "send_email": true,
	})
	rid := r["id"].(string)
	if jobsOfType(t, "lock_provision") != 1 {
		t.Fatalf("lock_provision encolado al confirmar")
	}
	tctx := database.WithTenantSchema(bg, "tenant_garrent")
	codes, err := e.svc.Locks.Provision(tctx, o.tenant, mustUUID(t, rid))
	if err != nil || len(codes) != 1 || codes[0].Status != "active" || len(codes[0].CodeHint) != 2 {
		t.Fatalf("código: %v %v", codes, err)
	}
	// Repetirlo no crea otro.
	if again, err := e.svc.Locks.Provision(tctx, o.tenant, mustUUID(t, rid)); err != nil || len(again) != 1 {
		t.Fatalf("idempotente: %v %v", again, err)
	}
	listed := o.do(http.MethodGet, "/api/v1/locks/codes?reservation_id="+rid, o.super, nil)
	expect(t, listed, http.StatusOK)
	if len(listed.list) != 1 || listed.list[0].(map[string]any)["code"] != nil {
		t.Fatalf("códigos sin el código completo: %v", listed.list)
	}

	_, code := accessFromEmail(t, "valeria@correo.com")
	auth := o.do(http.MethodPost, "/api/v1/guest/auth", "", map[string]any{"code": code, "email": "valeria@correo.com"})
	expect(t, auth, http.StatusOK)
	guest := auth.body["access_token"].(string)
	gl := o.do(http.MethodGet, "/api/v1/guest/lock", guest, nil)
	expect(t, gl, http.StatusOK)
	pin, _ := gl.body["code"].(string)
	if gl.body["available"] != true || len(pin) != 6 || gl.body["device_name"] != "Puerta principal" || !strings.HasSuffix(pin, codes[0].CodeHint) {
		t.Fatalf("código del huésped: %v", gl.body)
	}

	// Cambiar la salida crea un código nuevo y revoca el viejo.
	expect(t, o.do(http.MethodPatch, "/api/v1/reservations/"+rid, o.admin, map[string]any{"check_out_date": addDays(today, 3)}), http.StatusOK)
	moved, err := e.svc.Locks.Provision(tctx, o.tenant, mustUUID(t, rid))
	if err != nil || len(moved) != 2 {
		t.Fatalf("fechas nuevas: %v %v", moved, err)
	}
	statuses := map[string]int{}
	for _, c := range moved {
		statuses[c.Status]++
	}
	if statuses["active"] != 1 || statuses["revoked"] != 1 {
		t.Fatalf("estados: %v", statuses)
	}

	// Una cerradura que falla deja el código en failed y avisa una vez.
	rb := o.reserve(map[string]any{
		"property_id": bad, "channel_id": direct, "check_in_date": addDays(today, 4), "check_out_date": addDays(today, 6),
		"guest": map[string]any{"full_name": "Emily Carter", "email": "emily@correo.com"},
	})
	failed, err := e.svc.Locks.Provision(tctx, o.tenant, mustUUID(t, rb["id"].(string)))
	if err == nil || len(failed) != 1 || failed[0].Status != "failed" || failed[0].LastError == nil {
		t.Fatalf("falla: %v %v", failed, err)
	}
	_, _ = e.svc.Locks.Provision(tctx, o.tenant, mustUUID(t, rb["id"].(string)))
	var notices int
	if err := testDB.Pool().QueryRow(bg, `SELECT count(*) FROM tenant_garrent.notifications WHERE type = 'lock_failed'`).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if notices == 0 {
		t.Fatalf("aviso de cerradura con falla")
	}
	retried := o.do(http.MethodPost, "/api/v1/locks/reservations/"+rb["id"].(string)+"/provision", o.admin, nil)
	expect(t, retried, http.StatusOK)
	if retried.list[0].(map[string]any)["attempts"] != float64(3) {
		t.Fatalf("reintento: %v", retried.list)
	}

	// Check-out o cancelar revoca.
	expect(t, o.do(http.MethodPost, "/api/v1/reservations/"+rid+"/cancel", o.admin, map[string]any{"reason": "Prueba"}), http.StatusOK)
	if jobsOfType(t, "lock_revoke") != 1 {
		t.Fatalf("lock_revoke encolado")
	}
	if err := e.svc.Locks.Revoke(tctx, mustUUID(t, rid)); err != nil {
		t.Fatal(err)
	}
	var live int
	if err := testDB.Pool().QueryRow(bg, `SELECT count(*) FROM tenant_garrent.lock_access_codes WHERE reservation_id = $1 AND status <> 'revoked'`, rid).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Fatalf("códigos vivos tras cancelar: %d", live)
	}

	// --- Retencion ---------------------------------------------------------------
	if _, err := testDB.Pool().Exec(bg, `
		INSERT INTO tenant_garrent.notifications (recipient_user_id, type, title, body, data, channels, read_at, created_at)
		VALUES ($1, 'alert', 'Viejo', '', '{}', '["inapp"]', now() - interval '200 days', now() - interval '210 days'),
		       ($1, 'alert', 'Reciente', '', '{}', '["inapp"]', now() - interval '10 days', now() - interval '11 days')`,
		o.ids["ana@garrent.gt"]); err != nil {
		t.Fatal(err)
	}
	if _, err := testDB.Pool().Exec(bg, `
		INSERT INTO shared.jobs (type, payload, status, finished_at, created_at, updated_at)
		VALUES ('x_old', '{}', 'done', now() - interval '40 days', now() - interval '40 days', now() - interval '40 days')`); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Retention.Run(bg)
	if err != nil || res.NoticesDeleted != 1 || res.JobsDeleted != 1 {
		t.Fatalf("retención: %+v %v", res, err)
	}
	var left int
	if err := testDB.Pool().QueryRow(bg, `SELECT count(*) FROM tenant_garrent.notifications WHERE title = 'Reciente'`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("el aviso reciente se queda: %d %v", left, err)
	}
	if st, err := e.svc.Retention.WorkerStats(bg); err != nil || st.Pending < 1 {
		t.Fatalf("pulso del worker: %+v %v", st, err)
	}
}
