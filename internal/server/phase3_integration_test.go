//go:build integration

package server_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/core/storage"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/maintenance"
)

// Pruebas de la fase 3 de Persona A (fase03.md §8): alcance de incidencias,
// ciclo de vida, alertas de activos, preventivos, stock bajo, documentos,
// indicadores e Inicio.

// garrentCtx opera en el schema de Garrent (jobs y consultas directas).
func garrentCtx() context.Context {
	return database.WithTenantSchema(context.Background(), "tenant_garrent")
}

// assign asigna la propiedad a la persona (alcance de limpieza y mantenimiento).
func (o *opsEnv) assign(propertyID, email string) {
	o.t.Helper()
	expect(o.t, o.do(http.MethodPut, "/api/v1/properties/"+propertyID+"/assignments", o.admin,
		map[string]any{"user_ids": []string{o.ids[email].String()}}), http.StatusOK)
}

// ticket reporta una incidencia y devuelve su id.
func (o *opsEnv) ticket(token, propertyID, title string, extra map[string]any) string {
	o.t.Helper()
	body := map[string]any{"property_id": propertyID, "title": title, "urgency": "medium"}
	for k, v := range extra {
		body[k] = v
	}
	res := o.do(http.MethodPost, "/api/v1/tickets", token, body)
	expect(o.t, res, http.StatusCreated)
	return res.body["id"].(string)
}

// asset da de alta un activo y devuelve su id.
func (o *opsEnv) asset(body map[string]any) string {
	o.t.Helper()
	res := o.do(http.MethodPost, "/api/v1/assets", o.admin, body)
	expect(o.t, res, http.StatusCreated)
	return res.body["id"].(string)
}

// upload sube un archivo al almacen en memoria y lo confirma.
func (o *opsEnv) upload(token, kind, contentType, filename string) string {
	o.t.Helper()
	res := o.do(http.MethodPost, "/api/v1/files/presign-upload", token, map[string]any{
		"module": "maintenance", "kind": kind, "content_type": contentType, "size": 2048, "filename": filename,
	})
	expect(o.t, res, http.StatusOK)
	id := res.body["file_id"].(string)
	var key string
	if err := testDB.Pool().QueryRow(context.Background(), `SELECT s3_key FROM tenant_garrent.files WHERE id = $1`, id).Scan(&key); err != nil {
		o.t.Fatal(err)
	}
	o.svc.Storage.Backend().(*storage.Memory).Put(key, storage.ObjectInfo{Size: 2048, ContentType: contentType})
	expect(o.t, o.do(http.MethodPost, "/api/v1/files/"+id+"/confirm", token, nil), http.StatusOK)
	return id
}

func ids(res response) []string {
	out := []string{}
	for _, it := range items(res) {
		out = append(out, it["id"].(string))
	}
	return out
}

func eventTypes(res response) []string {
	out := []string{}
	for _, e := range res.body["events"].([]any) {
		out = append(out, e.(map[string]any)["type"].(string))
	}
	return out
}

// fase03.md §8: limpieza y propietario reportan solo sobre su alcance (404
// fuera) y ven solo esas incidencias; otra empresa no ve nada.
func TestTicketScopeByRole(t *testing.T) {
	e := setup(t)
	o := e.ops()
	p1 := o.property("TB-504", nil)
	p2 := o.property("CY-12A", nil)
	o.assign(p1, "maria@garrent.gt")
	expect(t, o.do(http.MethodPut, "/api/v1/properties/"+p2+"/owners", o.admin,
		map[string]any{"user_ids": []string{o.ids["ricardo@garrent.gt"].String()}}), http.StatusOK)

	byCleaner := o.ticket(o.cleaner, p1, "Fuga en el lavamanos", nil)
	expect(t, o.do(http.MethodPost, "/api/v1/tickets", o.cleaner, map[string]any{
		"property_id": p2, "title": "Fuera de alcance", "urgency": "low",
	}), http.StatusNotFound)
	byOwner := o.ticket(o.owner, p2, "La puerta del balcón no cierra", nil)
	expect(t, o.do(http.MethodPost, "/api/v1/tickets", o.owner, map[string]any{
		"property_id": p1, "title": "No es mía", "urgency": "low",
	}), http.StatusNotFound)
	bySecurity := o.ticket(o.security, p1, "Luz del pasillo fundida", nil)

	cases := []struct {
		who, token string
		want       []string
	}{
		{"limpieza", o.cleaner, []string{bySecurity, byCleaner}},
		{"propietario", o.owner, []string{byOwner}},
		{"mantenimiento sin asignaciones", o.maint, []string{}},
		{"supervision", o.super, []string{bySecurity, byOwner, byCleaner}},
	}
	for _, c := range cases {
		res := o.do(http.MethodGet, "/api/v1/tickets", c.token, nil)
		expect(t, res, http.StatusOK)
		if got := ids(res); !slices.Equal(got, c.want) {
			t.Errorf("%s ve %v, se esperaba %v", c.who, got, c.want)
		}
	}
	expect(t, o.do(http.MethodGet, "/api/v1/tickets/"+byOwner, o.cleaner, nil), http.StatusNotFound)

	// Asignarle una incidencia a mantenimiento se la deja ver aunque la
	// propiedad no sea suya.
	expect(t, o.do(http.MethodPost, "/api/v1/tickets/"+byOwner+"/assign", o.admin,
		map[string]any{"assignee_id": o.ids["luis@garrent.gt"].String()}), http.StatusOK)
	if got := ids(o.do(http.MethodGet, "/api/v1/tickets?mine=true", o.maint, nil)); !slices.Equal(got, []string{byOwner}) {
		t.Errorf("mantenimiento ve sus asignadas: %v", got)
	}

	b := e.tenant("costa", "Costa Limpia")
	e.member("beto@costa.gt", map[uuid.UUID]authz.RoleKey{b: authz.RoleAdmin})
	other, _ := e.session("beto@costa.gt")
	if got := ids(o.do(http.MethodGet, "/api/v1/tickets", other, nil)); len(got) != 0 {
		t.Errorf("otra empresa ve %v", got)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/tickets/"+byCleaner, other, nil), http.StatusNotFound)
	expect(t, o.do(http.MethodPost, "/api/v1/tickets/"+byCleaner+"/assign", other,
		map[string]any{"assignee_id": o.ids["luis@garrent.gt"].String()}), http.StatusNotFound)
}

// Ciclo completo: reportar con foto, asignar (crea la tarea si hay linker),
// iniciar, resolver con costo y activo operativo, cerrar. Cada paso publica
// su evento y queda en la linea de tiempo.
func TestTicketLifecycle(t *testing.T) {
	e := setup(t)
	o := e.ops()
	p1 := o.property("TB-504", nil)
	o.assign(p1, "maria@garrent.gt")
	vendor := o.do(http.MethodPost, "/api/v1/vendors", o.admin, map[string]any{"name": "Fríos del Sur", "service_type": "Aire acondicionado"})
	expect(t, vendor, http.StatusCreated)
	expect(t, o.do(http.MethodPost, "/api/v1/vendors", o.admin, map[string]any{"name": "fríos del sur"}), http.StatusUnprocessableEntity)
	ac := o.asset(map[string]any{"property_id": p1, "category": "hvac", "name": "A/C de la sala", "status": "needs_attention"})

	published := []string{}
	e.svc.Events.Subscribe(maintenance.EventTicketCreated, func(_ context.Context, _ database.Querier, ev events.Event) error {
		published = append(published, ev.Type)
		return nil
	})
	e.svc.Events.Subscribe(maintenance.EventTicketAssigned, func(_ context.Context, _ database.Querier, ev events.Event) error {
		published = append(published, ev.Type)
		if ev.Data.(maintenance.TicketEvent).Ticket.AssigneeName == nil {
			t.Error("el evento trae al asignado")
		}
		return nil
	})
	e.svc.Events.Subscribe(maintenance.EventTicketResolved, func(_ context.Context, _ database.Querier, ev events.Event) error {
		published = append(published, ev.Type)
		return nil
	})
	// El linker real (tareas, Persona B) crea la tarea; se guarda su id.
	var task uuid.UUID
	e.svc.Maintenance.SetTaskLinker(func(ctx context.Context, q database.Querier, a authz.Actor, tk maintenance.TicketRead) (*uuid.UUID, error) {
		id, err := e.svc.Tasks.LinkTicket(ctx, q, a, tk)
		if id != nil {
			task = *id
		}
		return id, err
	})

	photo := o.upload(o.cleaner, "ticket_photo", "image/jpeg", "fuga.jpg")
	id := o.ticket(o.cleaner, p1, "El A/C gotea", map[string]any{"asset_id": ac, "urgency": "high", "photo_file_ids": []string{photo}})
	detail := o.do(http.MethodGet, "/api/v1/tickets/"+id, o.cleaner, nil)
	if detail.body["number"] != float64(1) || detail.body["photos_count"] != float64(1) || len(detail.body["photos"].([]any)) != 1 {
		t.Errorf("folio y foto: %v", detail.body)
	}
	// Una foto ya usada (o ajena) no se vuelve a adjuntar.
	expect(t, o.do(http.MethodPost, "/api/v1/tickets", o.security, map[string]any{
		"property_id": p1, "title": "Otra", "urgency": "low", "photo_file_ids": []string{photo},
	}), http.StatusUnprocessableEntity)

	expect(t, o.do(http.MethodPost, "/api/v1/tickets/"+id+"/assign", o.admin, map[string]any{}), http.StatusUnprocessableEntity)
	expect(t, o.do(http.MethodPost, "/api/v1/tickets/"+id+"/assign", o.admin,
		map[string]any{"assignee_id": o.ids["maria@garrent.gt"].String()}), http.StatusUnprocessableEntity)
	assigned := o.do(http.MethodPost, "/api/v1/tickets/"+id+"/assign", o.admin, map[string]any{
		"assignee_id": o.ids["luis@garrent.gt"].String(), "vendor_id": vendor.body["id"], "note": "Lleva escalera",
	})
	expect(t, assigned, http.StatusOK)
	if assigned.body["status"] != "assigned" || assigned.body["task_id"] != task.String() {
		t.Errorf("asignada con tarea: %v", assigned.body)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/tickets/"+id+"/start", o.cleaner, nil), http.StatusForbidden)
	expect(t, o.do(http.MethodPost, "/api/v1/tickets/"+id+"/start", o.maint, nil), http.StatusOK)
	expect(t, o.do(http.MethodPost, "/api/v1/tickets/"+id+"/resolve", o.maint, map[string]any{"resolution": "x"}), http.StatusUnprocessableEntity)
	resolved := o.do(http.MethodPost, "/api/v1/tickets/"+id+"/resolve", o.maint, map[string]any{
		"resolution": "Se limpió el drenaje y se cambió el empaque.", "cost": 350.5, "asset_operational": true,
	})
	expect(t, resolved, http.StatusOK)
	if resolved.body["status"] != "resolved" || resolved.body["cost"] != 350.5 {
		t.Errorf("resuelta: %v", resolved.body)
	}
	if a := o.do(http.MethodGet, "/api/v1/assets/"+ac, o.admin, nil); a.body["status"] != "operational" || a.body["cost_year"] != 350.5 {
		t.Errorf("el activo vuelve a operar y acumula el costo: %v %v", a.body["status"], a.body["cost_year"])
	}
	expect(t, o.do(http.MethodPost, "/api/v1/tickets/"+id+"/reject", o.admin, map[string]any{"reason": "Duplicada"}), http.StatusUnprocessableEntity)
	expect(t, o.do(http.MethodPost, "/api/v1/tickets/"+id+"/reopen", o.admin, map[string]any{}), http.StatusUnprocessableEntity)
	closed := o.do(http.MethodPost, "/api/v1/tickets/"+id+"/close", o.admin, map[string]any{"reason": "Revisado"})
	expect(t, closed, http.StatusOK)
	if got := eventTypes(closed); !slices.Equal(got, []string{"created", "assigned", "started", "resolved", "closed"}) {
		t.Errorf("linea de tiempo: %v", got)
	}
	if !slices.Equal(published, []string{maintenance.EventTicketCreated, maintenance.EventTicketAssigned, maintenance.EventTicketResolved}) {
		t.Errorf("eventos publicados: %v", published)
	}

	// Rechazar y reabrir con motivo.
	other := o.ticket(o.security, p1, "Ruido raro", nil)
	expect(t, o.do(http.MethodPost, "/api/v1/tickets/"+other+"/reject", o.admin, map[string]any{}), http.StatusUnprocessableEntity)
	expect(t, o.do(http.MethodPost, "/api/v1/tickets/"+other+"/reject", o.admin, map[string]any{"reason": "Es normal"}), http.StatusOK)
	reopened := o.do(http.MethodPost, "/api/v1/tickets/"+other+"/reopen", o.admin, map[string]any{"reason": "Volvió a sonar"})
	expect(t, reopened, http.StatusOK)
	if reopened.body["status"] != "open" {
		t.Errorf("reabierta: %v", reopened.body["status"])
	}
	// Corregir: urgencia y costo quedan en la linea de tiempo.
	patched := o.do(http.MethodPatch, "/api/v1/tickets/"+other, o.admin, map[string]any{"urgency": "critical", "cost": 120})
	expect(t, patched, http.StatusOK)
	if patched.body["urgency"] != "critical" || slices.Index(eventTypes(patched), "updated") < 0 {
		t.Errorf("corregida: %v", patched.body)
	}
	expect(t, o.do(http.MethodPatch, "/api/v1/tickets/"+other, o.maint, map[string]any{"urgency": "low"}), http.StatusForbidden)
	if found := ids(o.do(http.MethodGet, "/api/v1/tickets?q=%231", o.admin, nil)); !slices.Equal(found, []string{id}) {
		t.Errorf("buscar por folio: %v", found)
	}
	if found := ids(o.do(http.MethodGet, "/api/v1/tickets?status=active", o.admin, nil)); !slices.Equal(found, []string{other}) {
		t.Errorf("abiertas: %v", found)
	}
}

// fase03.md §3: un activo con falla levanta asset_failure (una sola viva) y
// volver a operational la resuelve. Alertas con alcance y permisos.
func TestAssetFailureAlert(t *testing.T) {
	e := setup(t)
	o := e.ops()
	p1 := o.property("TB-504", nil)
	ac := o.asset(map[string]any{"property_id": p1, "category": "hvac", "name": "A/C de la sala"})
	expect(t, o.do(http.MethodPost, "/api/v1/assets", o.admin, map[string]any{"category": "hvac", "name": "Sin lugar"}), http.StatusUnprocessableEntity)
	if a := items(o.do(http.MethodGet, "/api/v1/alerts", o.admin, nil)); len(a) != 0 {
		t.Fatalf("sin falla no hay alertas: %v", a)
	}
	expect(t, o.do(http.MethodPatch, "/api/v1/assets/"+ac, o.admin, map[string]any{"status": "needs_attention"}), http.StatusOK)
	expect(t, o.do(http.MethodPatch, "/api/v1/assets/"+ac, o.admin, map[string]any{"status": "out_of_service"}), http.StatusOK)
	list := items(o.do(http.MethodGet, "/api/v1/alerts", o.admin, nil))
	if len(list) != 1 || list[0]["type"] != "asset_failure" || list[0]["severity"] != "high" || list[0]["property_code"] != "TB-504" {
		t.Fatalf("una sola alerta viva, alta: %v", list)
	}
	alertID := list[0]["id"].(string)
	summary := o.do(http.MethodGet, "/api/v1/alerts/summary", o.admin, nil)
	if summary.body["open"] != float64(1) || summary.body["high"] != float64(1) || summary.body["fresh"] != float64(1) {
		t.Errorf("resumen: %v", summary.body)
	}

	// Mantenimiento solo ve las de sus propiedades; limpieza no tiene alerts.read.
	if a := items(o.do(http.MethodGet, "/api/v1/alerts", o.maint, nil)); len(a) != 0 {
		t.Errorf("mantenimiento sin asignaciones ve %v", a)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/alerts/"+alertID+"/ack", o.maint, nil), http.StatusForbidden)
	o.assign(p1, "luis@garrent.gt")
	if a := items(o.do(http.MethodGet, "/api/v1/alerts", o.maint, nil)); len(a) != 1 {
		t.Errorf("mantenimiento asignado ve la alerta: %v", a)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/alerts", o.cleaner, nil), http.StatusForbidden)

	acked := o.do(http.MethodPost, "/api/v1/alerts/"+alertID+"/ack", o.super, nil)
	expect(t, acked, http.StatusOK)
	if acked.body["status"] != "acknowledged" || acked.body["acknowledged_by_name"] == nil {
		t.Errorf("reconocida: %v", acked.body)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/alerts/"+alertID+"/ack", o.super, nil), http.StatusOK)

	expect(t, o.do(http.MethodPatch, "/api/v1/assets/"+ac, o.admin, map[string]any{"status": "operational"}), http.StatusOK)
	if a := items(o.do(http.MethodGet, "/api/v1/alerts", o.admin, nil)); len(a) != 0 {
		t.Errorf("volver a operar la resuelve: %v", a)
	}
	resolved := items(o.do(http.MethodGet, "/api/v1/alerts?status=resolved", o.admin, nil))
	if len(resolved) != 1 || resolved[0]["resolved_by_name"] != nil {
		t.Errorf("resuelta sola: %v", resolved)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/alerts/"+alertID+"/resolve", o.admin, nil), http.StatusUnprocessableEntity)

	// Una falla nueva es una alerta nueva; resolverla a mano deja quien fue.
	expect(t, o.do(http.MethodPatch, "/api/v1/assets/"+ac, o.admin, map[string]any{"status": "in_repair"}), http.StatusOK)
	fresh := items(o.do(http.MethodGet, "/api/v1/alerts", o.admin, nil))
	if len(fresh) != 1 || fresh[0]["id"] == alertID {
		t.Fatalf("alerta nueva: %v", fresh)
	}
	done := o.do(http.MethodPost, "/api/v1/alerts/"+fresh[0]["id"].(string)+"/resolve", o.admin, nil)
	if done.body["status"] != "resolved" || done.body["resolved_by_name"] == nil {
		t.Errorf("resuelta a mano: %v", done.body)
	}
}

// fase03.md §8: el job de preventivos genera el ticket (una sola vez) y la
// alerta de vencido; resolverlo recalcula next_due_at.
func TestPreventivePlanJob(t *testing.T) {
	e := setup(t)
	o := e.ops()
	building := o.do(http.MethodPost, "/api/v1/buildings", o.admin, map[string]any{"name": "Torre Botánica"})
	expect(t, building, http.StatusCreated)
	p1 := o.property("TB-504", map[string]any{"building_id": building.body["id"]})
	o.assign(p1, "luis@garrent.gt")
	elevator := o.asset(map[string]any{"building_id": building.body["id"], "category": "elevator", "name": "Elevador principal"})
	heater := o.asset(map[string]any{"property_id": p1, "category": "water", "name": "Calentador"})

	today := todayGT(t)
	soon := o.do(http.MethodPost, "/api/v1/assets/"+elevator+"/plans", o.admin, map[string]any{
		"name": "Revisión mensual", "frequency_days": 30, "next_due_at": addDays(today, 3), "lead_days": 7,
		"responsible_user_id": o.ids["luis@garrent.gt"].String(),
	})
	expect(t, soon, http.StatusCreated)
	if soon.body["due_state"] != "upcoming" {
		t.Errorf("dentro de su anticipacion: %v", soon.body["due_state"])
	}
	late := o.do(http.MethodPost, "/api/v1/assets/"+heater+"/plans", o.admin, map[string]any{
		"name": "Purga del tanque", "frequency_days": 90, "next_due_at": addDays(today, -10),
	})
	expect(t, late, http.StatusCreated)
	expect(t, o.do(http.MethodPost, "/api/v1/assets/"+heater+"/plans", o.admin, map[string]any{
		"name": "Lejano", "frequency_days": 365, "next_due_at": addDays(today, 200),
	}), http.StatusCreated)
	expect(t, o.do(http.MethodPost, "/api/v1/assets/"+heater+"/plans", o.admin, map[string]any{
		"name": "Con cleaner", "frequency_days": 30, "responsible_user_id": o.ids["maria@garrent.gt"].String(),
	}), http.StatusUnprocessableEntity)

	due := 0
	e.svc.Events.Subscribe(maintenance.EventMaintenanceDue, func(context.Context, database.Querier, events.Event) error {
		due++
		return nil
	})
	run := func() maintenance.DueResult {
		var res maintenance.DueResult
		ctx := garrentCtx()
		if err := testDB.InTx(ctx, func(q database.Querier) (err error) {
			res, err = e.svc.Maintenance.RunDue(ctx, q, o.tenant)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run(); res.Tickets != 2 || res.Overdue != 1 || due != 2 {
		t.Fatalf("primera pasada: %+v (%d eventos)", res, due)
	}
	if res := run(); res.Tickets != 0 || res.Overdue != 0 {
		t.Errorf("la segunda pasada no duplica: %+v", res)
	}

	tickets := items(o.do(http.MethodGet, "/api/v1/tickets?status=active", o.admin, nil))
	if len(tickets) != 2 {
		t.Fatalf("dos tickets de preventivo: %v", tickets)
	}
	var elevatorTicket, heaterTicket map[string]any
	for _, tk := range tickets {
		if tk["asset_id"] == elevator {
			elevatorTicket = tk
		} else {
			heaterTicket = tk
		}
	}
	if elevatorTicket["property_id"] != nil || elevatorTicket["building_name"] != "Torre Botánica" ||
		elevatorTicket["status"] != "assigned" || elevatorTicket["reporter_name"] != nil {
		t.Errorf("el del elevador es del edificio y ya va asignado: %v", elevatorTicket)
	}
	if heaterTicket["urgency"] != "high" || heaterTicket["plan_name"] != "Purga del tanque" {
		t.Errorf("el vencido va con urgencia alta: %v", heaterTicket)
	}
	// Mantenimiento (asignado a una propiedad del edificio) ve el del elevador.
	if mine := ids(o.do(http.MethodGet, "/api/v1/tickets?mine=true", o.maint, nil)); !slices.Equal(mine, []string{elevatorTicket["id"].(string)}) {
		t.Errorf("mantenimiento ve el preventivo asignado: %v", mine)
	}
	if a := items(o.do(http.MethodGet, "/api/v1/alerts?type=maintenance_overdue", o.admin, nil)); len(a) != 1 || a[0]["severity"] != "high" {
		t.Errorf("alerta de vencido: %v", a)
	}

	resolved := o.do(http.MethodPost, "/api/v1/tickets/"+heaterTicket["id"].(string)+"/resolve", o.super, map[string]any{"resolution": "Tanque purgado."})
	expect(t, resolved, http.StatusOK)
	plans := items(o.do(http.MethodGet, "/api/v1/assets/"+heater+"/plans", o.admin, nil))
	for _, p := range plans {
		if p["name"] != "Purga del tanque" {
			continue
		}
		if p["last_done_at"] != today || p["next_due_at"].(string) <= today || p["due_state"] != "ok" || p["open_ticket_id"] != nil {
			t.Errorf("el plan avanza una frecuencia: %v", p)
		}
	}
	if a := items(o.do(http.MethodGet, "/api/v1/alerts?type=maintenance_overdue", o.admin, nil)); len(a) != 0 {
		t.Errorf("resolver el preventivo resuelve su alerta: %v", a)
	}
	calendar := items(o.do(http.MethodGet, "/api/v1/maintenance/plans?from="+today+"&to="+addDays(today, 40), o.admin, nil))
	if len(calendar) != 1 || calendar[0]["name"] != "Revisión mensual" {
		t.Errorf("calendario del mes: %v", calendar)
	}

	// La app recibe sin conexion los activos de su alcance (los del edificio incluidos).
	boot := o.do(http.MethodGet, "/api/v1/sync/bootstrap", o.maint, nil)
	expect(t, boot, http.StatusOK)
	if assets := boot.body["assets"].([]any); len(assets) != 2 {
		t.Errorf("activos en el bootstrap: %v", assets)
	}
	if assets := o.do(http.MethodGet, "/api/v1/sync/bootstrap", o.owner, nil).body["assets"].([]any); len(assets) != 0 {
		t.Errorf("el propietario no tiene propiedades aqui: %v", assets)
	}

	// El job recurrente (sin empresa) encola uno por empresa activa.
	if err := e.svc.Maintenance.HandleJob(context.Background(), jobsRunning(maintenance.JobMaintenanceDue, nil)); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := testDB.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM shared.jobs WHERE type = 'maintenance_due' AND tenant_id = $1`, o.tenant).Scan(&queued); err != nil || queued != 1 {
		t.Errorf("fan-out por empresa: %d %v", queued, err)
	}
}

// low_stock usa las tablas de inventario de Persona B (fase02.md §2). Sin
// ellas no hace nada; con ellas, una alerta por articulo bajo minimo que se
// resuelve sola al reponerlo.
func TestLowStockJob(t *testing.T) {
	e := setup(t)
	o := e.ops()
	ctx := garrentCtx()
	run := func() int {
		var n int
		if err := testDB.InTx(ctx, func(q database.Querier) (err error) {
			n, err = e.svc.Maintenance.RunLowStock(ctx, q, o.tenant)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := run(); n != 0 {
		t.Fatalf("sin inventario no hay alertas: %d", n)
	}
	var towels uuid.UUID
	if err := testDB.InTx(ctx, func(q database.Querier) error {
		for _, ddl := range []string{
			`CREATE TABLE IF NOT EXISTS warehouses (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), name varchar(128) NOT NULL,
			   property_id uuid, is_active boolean NOT NULL DEFAULT true)`,
			`CREATE TABLE IF NOT EXISTS inventory_items (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), name varchar(128) NOT NULL,
			   category varchar(16) NOT NULL DEFAULT 'consumable', unit varchar(16), min_stock numeric(12,3) NOT NULL DEFAULT 0,
			   is_active boolean NOT NULL DEFAULT true)`,
			`CREATE TABLE IF NOT EXISTS inventory_stock (warehouse_id uuid NOT NULL, item_id uuid NOT NULL,
			   quantity numeric(12,3) NOT NULL DEFAULT 0, PRIMARY KEY (warehouse_id, item_id))`,
		} {
			if _, err := q.Exec(ctx, ddl); err != nil {
				return err
			}
		}
		return q.QueryRow(ctx, `
			WITH w AS (INSERT INTO warehouses (name) VALUES ('Bodega central') RETURNING id),
			     i AS (INSERT INTO inventory_items (name, category, unit, min_stock) VALUES ('Toallas', 'linens', 'pza', 10) RETURNING id)
			INSERT INTO inventory_stock (warehouse_id, item_id, quantity) SELECT w.id, i.id, 3 FROM w, i RETURNING item_id`).Scan(&towels)
	}); err != nil {
		t.Fatal(err)
	}
	if n := run(); n != 1 {
		t.Fatalf("una alerta nueva: %d", n)
	}
	if n := run(); n != 0 {
		t.Errorf("no se duplica: %d", n)
	}
	list := items(o.do(http.MethodGet, "/api/v1/alerts?type=low_stock", o.admin, nil))
	if len(list) != 1 || list[0]["message"] != "Toallas: 3 de 10 pza en Bodega central" || list[0]["entity_id"] != towels.String() {
		t.Fatalf("mensaje: %v", list)
	}
	if err := testDB.InTx(ctx, func(q database.Querier) error {
		_, err := q.Exec(ctx, `UPDATE inventory_stock SET quantity = 25`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	run()
	if list := items(o.do(http.MethodGet, "/api/v1/alerts?type=low_stock", o.admin, nil)); len(list) != 0 {
		t.Errorf("reponer la resuelve: %v", list)
	}
}

// Documentos del activo (nombre original, URL firmada) y ficha con planes e
// incidencias.
func TestAssetDocuments(t *testing.T) {
	e := setup(t)
	o := e.ops()
	p1 := o.property("TB-504", nil)
	ac := o.asset(map[string]any{
		"property_id": p1, "category": "hvac", "name": "A/C de la sala", "brand": "Mabe",
		"installed_at": "2024-03-01", "warranty_until": "2026-03-01",
	})
	expect(t, o.do(http.MethodPatch, "/api/v1/assets/"+ac, o.admin, map[string]any{"warranty_until": "2020-01-01"}), http.StatusUnprocessableEntity)
	doc := o.upload(o.admin, "asset_document", "application/pdf", `C:\manuales\Manual A-C.pdf`)
	attached := o.do(http.MethodPost, "/api/v1/assets/"+ac+"/documents", o.admin, map[string]any{"file_id": doc})
	expect(t, attached, http.StatusCreated)
	if attached.body["name"] != "Manual A-C.pdf" || attached.body["url"] == "" {
		t.Errorf("documento: %v", attached.body)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/assets/"+ac+"/documents", o.admin, map[string]any{"file_id": uuid.NewString()}), http.StatusUnprocessableEntity)
	o.ticket(o.admin, p1, "El A/C no enfría", map[string]any{"asset_id": ac})
	detail := o.do(http.MethodGet, "/api/v1/assets/"+ac, o.admin, nil)
	if len(detail.body["documents"].([]any)) != 1 || len(detail.body["tickets"].([]any)) != 1 || detail.body["open_tickets"] != float64(1) {
		t.Errorf("ficha: %v", detail.body)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/assets/"+ac, o.cleaner, nil), http.StatusNotFound)
	expect(t, o.do(http.MethodDelete, "/api/v1/assets/"+ac+"/documents/"+doc, o.admin, nil), http.StatusNoContent)
	if d := o.do(http.MethodGet, "/api/v1/assets/"+ac, o.admin, nil); len(d.body["documents"].([]any)) != 0 {
		t.Errorf("quitado: %v", d.body["documents"])
	}
	if found := items(o.do(http.MethodGet, "/api/v1/assets?q=mabe&property_id="+p1, o.admin, nil)); len(found) != 1 {
		t.Errorf("buscar activos: %v", found)
	}
}

// Indicadores (W-G5) e Inicio (W-B1) con alcance y permisos.
func TestMetricsAndDashboard(t *testing.T) {
	e := setup(t)
	o := e.ops()
	p1 := o.property("TB-504", map[string]any{"lat": 14.5986, "lng": -90.5094})
	p2 := o.property("CY-12A", nil)
	o.assign(p1, "maria@garrent.gt")
	ac := o.asset(map[string]any{"property_id": p1, "category": "hvac", "name": "A/C", "status": "out_of_service"})
	open := o.ticket(o.admin, p1, "A/C fuera de servicio", map[string]any{"asset_id": ac, "urgency": "critical"})
	done := o.ticket(o.admin, p2, "Grifo flojo", nil)
	expect(t, o.do(http.MethodPost, "/api/v1/tickets/"+done+"/resolve", o.super, map[string]any{"resolution": "Ajustado.", "cost": 80}), http.StatusOK)

	m := o.do(http.MethodGet, "/api/v1/maintenance/metrics", o.admin, nil)
	expect(t, m, http.StatusOK)
	if m.body["open"] != float64(1) || m.body["urgent"] != float64(1) || m.body["unassigned"] != float64(1) ||
		m.body["cost_year"] != float64(80) || len(m.body["resolution"].([]any)) != 6 {
		t.Errorf("indicadores: %v", m.body)
	}
	byPlace := m.body["open_by_place"].([]any)
	if len(byPlace) != 1 || byPlace[0].(map[string]any)["code"] != "TB-504" {
		t.Errorf("abiertas por propiedad: %v", byPlace)
	}

	d := o.do(http.MethodGet, "/api/v1/dashboard", o.admin, nil)
	expect(t, d, http.StatusOK)
	alertsBlock := d.body["alerts"].(map[string]any)
	if alertsBlock["high"] != float64(1) || len(d.body["top_alerts"].([]any)) != 1 {
		t.Errorf("alertas del inicio: %v", d.body["alerts"])
	}
	if inc := d.body["incidents"].(map[string]any); inc["open"] != float64(1) || inc["urgent"] != float64(1) {
		t.Errorf("incidencias del inicio: %v", inc)
	}
	pins := d.body["pins"].([]any)
	status := map[string]string{}
	for _, p := range pins {
		pin := p.(map[string]any)
		status[pin["code"].(string)] = pin["status"].(string)
	}
	if status["TB-504"] != "incident" || status["CY-12A"] != "ready" {
		t.Errorf("pines: %v", status)
	}
	if len(d.body["activity"].([]any)) == 0 || d.body["tasks"].(map[string]any)["available"] != true {
		t.Errorf("actividad y tareas: %v %v", d.body["activity"], d.body["tasks"])
	}

	// Limpieza: sin alertas ni bitacora; solo su propiedad en el mapa.
	c := o.do(http.MethodGet, "/api/v1/dashboard", o.cleaner, nil)
	expect(t, c, http.StatusOK)
	if len(c.body["top_alerts"].([]any)) != 0 || len(c.body["activity"].([]any)) != 0 || len(c.body["pins"].([]any)) != 1 {
		t.Errorf("inicio de limpieza: %v", c.body)
	}
	if inc := c.body["incidents"].(map[string]any); inc["open"] != float64(1) {
		t.Errorf("limpieza ve la incidencia de su propiedad: %v", inc)
	}
	_ = open
}

// todayGT es la fecha de hoy en la zona de las empresas de prueba.
func todayGT(t *testing.T) string {
	t.Helper()
	var d string
	if err := testDB.Pool().QueryRow(context.Background(),
		`SELECT to_char((now() AT TIME ZONE 'America/Guatemala')::date, 'YYYY-MM-DD')`).Scan(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

func addDays(d string, n int) string {
	day, _ := time.Parse(time.DateOnly, d)
	return day.AddDate(0, 0, n).Format(time.DateOnly)
}

func jobsRunning(jobType string, tenantID *uuid.UUID) jobs.Running {
	return jobs.Running{ID: uuid.New(), Type: jobType, TenantID: tenantID, Attempt: 1}
}
