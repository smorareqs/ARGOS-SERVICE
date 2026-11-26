//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/modules/notifications"
)

// Pruebas de la fase 3 de Persona B (fase03.md §4): bandeja, preferencias,
// push con tokens que caducan, recordatorios y tiempo real por WebSocket.

type fakePush struct {
	mu    sync.Mutex
	sent  []notifications.PushMessage
	stale map[string]bool
}

func (f *fakePush) Send(_ context.Context, m notifications.PushMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stale[m.Token] {
		return notifications.ErrInvalidToken
	}
	f.sent = append(f.sent, m)
	return nil
}

// runPushJobs corre los send_push encolados de la empresa.
func (o *opsEnv) runPushJobs() int {
	o.t.Helper()
	ctx := context.Background()
	rows, err := testDB.Pool().Query(ctx, `SELECT id, payload FROM shared.jobs WHERE type = 'send_push' AND status = 'pending' AND tenant_id = $1`, o.tenant)
	if err != nil {
		o.t.Fatal(err)
	}
	type pending struct {
		id      string
		payload []byte
	}
	var list []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.payload); err != nil {
			o.t.Fatal(err)
		}
		list = append(list, p)
	}
	rows.Close()
	tctx := database.WithTenantSchema(ctx, "tenant_garrent")
	for _, p := range list {
		job := jobsRunning(notifications.JobSendPush, &o.tenant)
		job.Payload = json.RawMessage(p.payload)
		if err := o.svc.Notifications.HandleJob(tctx, job); err != nil {
			o.t.Fatal(err)
		}
		if _, err := testDB.Pool().Exec(ctx, `UPDATE shared.jobs SET status = 'done' WHERE id = $1`, p.id); err != nil {
			o.t.Fatal(err)
		}
	}
	return len(list)
}

func TestNotificationsInboxPushAndRealtime(t *testing.T) {
	e := setup(t)
	o := e.ops()
	push := &fakePush{stale: map[string]bool{"tok-viejo": true}}
	e.svc.Notifications.SetPush(push)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.svc.Realtime.Run(ctx)

	p1 := o.property("TB-504", nil)
	o.assign(p1, "maria@garrent.gt")
	maria := o.ids["maria@garrent.gt"].String()

	// Telefono de Maria: dos tokens, uno ya caducado.
	for _, tok := range []string{"tok-bueno", "tok-viejo"} {
		expect(t, o.do(http.MethodPost, "/api/v1/devices", o.cleaner, map[string]any{"token": tok, "platform": "android"}), http.StatusNoContent)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/devices", o.cleaner, map[string]any{"token": " ", "platform": "android"}), http.StatusUnprocessableEntity)

	// WebSocket: sin token 401; con token saluda.
	wsURL := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/v1/ws"
	if _, res, err := websocket.Dial(ctx, wsURL, nil); err == nil || res == nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ws sin token: %v %v", res, err)
	}
	conn, _, err := websocket.Dial(ctx, wsURL+"?token="+o.cleaner, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	read := func() map[string]any {
		t.Helper()
		rctx, c := context.WithTimeout(ctx, 5*time.Second)
		defer c()
		_, b, err := conn.Read(rctx)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}
	if m := read(); m["type"] != "hello" {
		t.Fatalf("saludo: %v", m)
	}
	for i := 0; e.svc.Realtime.Connected(o.ids["maria@garrent.gt"]) == 0 && i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // el hub ya escucha el canal

	// Asignar una tarea a Maria: aviso en la bandeja, por socket y push.
	res := o.do(http.MethodPost, "/api/v1/tasks", o.admin, map[string]any{
		"type": "checkout_cleaning", "property_id": p1, "date": today(), "start": "11:00", "end": "15:00", "assignee_id": maria,
	})
	expect(t, res, http.StatusCreated)
	taskID := res.body["id"].(string)

	m := read()
	if m["type"] != "notification" {
		t.Fatalf("tiempo real: %v", m)
	}
	if d := m["data"].(map[string]any); d["type"] != "task_assigned" {
		t.Errorf("aviso por socket: %v", d)
	}

	inbox := o.do(http.MethodGet, "/api/v1/notifications", o.cleaner, nil)
	expect(t, inbox, http.StatusOK)
	list := inbox.body["items"].([]any)
	if len(list) != 1 || inbox.body["unread"].(float64) != 1 {
		t.Fatalf("bandeja: %v", inbox.body)
	}
	n := list[0].(map[string]any)
	data := n["data"].(map[string]any)
	if n["type"] != "task_assigned" || data["app_route"] != "/tasks/"+taskID || data["task_id"] != taskID || !strings.HasPrefix(data["route"].(string), "/itinerario") {
		t.Errorf("aviso: %v", n)
	}
	// Quien asigno no se avisa a si mismo.
	if adm := o.do(http.MethodGet, "/api/v1/notifications", o.admin, nil); len(adm.body["items"].([]any)) != 0 {
		t.Errorf("admin recibio: %v", adm.body)
	}
	// Correo encolado con la plantilla de avisos.
	var mails int
	if err := testDB.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM shared.jobs WHERE type = $1 AND tenant_id = $2 AND payload->>'template' = 'notification'`,
		jobs.TypeSendEmail, o.tenant).Scan(&mails); err != nil || mails != 1 {
		t.Errorf("correos: %d %v", mails, err)
	}

	// Push: el token bueno recibe; el caducado se borra.
	if ran := o.runPushJobs(); ran != 1 {
		t.Fatalf("send_push: %d", ran)
	}
	if len(push.sent) != 1 || push.sent[0].Token != "tok-bueno" || push.sent[0].Data["app_route"] != "/tasks/"+taskID {
		t.Errorf("push: %+v", push.sent)
	}
	var left int
	_ = testDB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM shared.device_tokens WHERE user_id = $1`, maria).Scan(&left)
	if left != 1 {
		t.Errorf("tokens tras push: %d", left)
	}

	// Leer: 404 si no es propio; luego unread 0 y aviso por socket.
	nid := n["id"].(string)
	expect(t, o.do(http.MethodPost, "/api/v1/notifications/"+nid+"/read", o.admin, nil), http.StatusNotFound)
	expect(t, o.do(http.MethodPost, "/api/v1/notifications/"+nid+"/read", o.cleaner, nil), http.StatusNoContent)
	if m := read(); m["type"] != "notification.read" {
		t.Errorf("leido por socket: %v", m)
	}
	if u := o.do(http.MethodGet, "/api/v1/notifications?unread=true", o.cleaner, nil); len(u.body["items"].([]any)) != 0 || u.body["unread"].(float64) != 0 {
		t.Errorf("sin leer: %v", u.body)
	}

	// Preferencias: apagar reprogramaciones; lo critico no se apaga.
	prefs := o.do(http.MethodPut, "/api/v1/notifications/preferences", o.cleaner, map[string]any{"items": []map[string]any{
		{"type": "task_rescheduled", "push": false, "email": false},
		{"type": "task_assigned", "push": false, "email": false},
	}})
	expect(t, prefs, http.StatusOK)
	for _, it := range prefs.list {
		p := it.(map[string]any)
		switch p["type"] {
		case "task_rescheduled":
			if p["push"] != false || p["email"] != false {
				t.Errorf("reprogramada: %v", p)
			}
		case "task_assigned":
			if p["push"] != true || p["critical"] != true {
				t.Errorf("critico: %v", p)
			}
		}
	}
	expect(t, o.do(http.MethodPut, "/api/v1/notifications/preferences", o.cleaner, map[string]any{"items": []map[string]any{
		{"type": "no_existe", "push": false, "email": false},
	}}), http.StatusUnprocessableEntity)

	// Reprogramar: queda en la bandeja pero sin push ni correo.
	expect(t, o.do(http.MethodPost, "/api/v1/tasks/"+taskID+"/reschedule", o.admin, map[string]any{
		"date": today(), "start": "12:00", "end": "16:00", "reason": "Llega tarde el huésped",
	}), http.StatusOK)
	if ran := o.runPushJobs(); ran != 0 {
		t.Errorf("push con preferencia apagada: %d", ran)
	}
	if res := o.do(http.MethodGet, "/api/v1/notifications", o.cleaner, nil); res.body["unread"].(float64) != 1 {
		t.Errorf("reprogramada en bandeja: %v", res.body)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/notifications/read-all", o.cleaner, nil), http.StatusNoContent)

	// Recordatorio 30 min antes: una sola vez por tarea.
	e.svc.Notifications.SetClock(func() time.Time {
		loc, _ := time.LoadLocation("America/Guatemala")
		d, _ := time.ParseInLocation("2006-01-02 15:04", today()+" 11:40", loc)
		return d
	})
	tctx := database.WithTenantSchema(context.Background(), "tenant_garrent")
	for i, want := range []int{1, 0} {
		var got int
		err := testDB.InTx(tctx, func(q database.Querier) error {
			var err error
			got, err = e.svc.Notifications.TaskReminders(tctx, q, o.tenant)
			return err
		})
		if err != nil || got != want {
			t.Errorf("recordatorios %d: %d %v", i, got, err)
		}
	}

	// Cerrar sesion: el token se quita.
	expect(t, o.do(http.MethodDelete, "/api/v1/devices/tok-bueno", o.cleaner, nil), http.StatusNoContent)
	_ = testDB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM shared.device_tokens WHERE user_id = $1`, maria).Scan(&left)
	if left != 0 {
		t.Errorf("tokens tras cerrar sesion: %d", left)
	}
}
