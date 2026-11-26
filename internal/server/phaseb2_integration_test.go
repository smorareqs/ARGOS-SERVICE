//go:build integration

package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/storage"
)

// Pruebas de la fase 2 de Persona B (fase02.md §7): alcance de tareas,
// ejecucion con fotos y conciliacion de custodia, inventario sin negativos y
// el trabajo de retrasos.

// taskPhoto sube una foto de un punto del checklist y la confirma.
func (o *opsEnv) taskPhoto(token, kind, itemID string) string {
	o.t.Helper()
	res := o.do(http.MethodPost, "/api/v1/files/presign-upload", token, map[string]any{
		"module": "tasks", "kind": kind, "content_type": "image/jpeg", "size": 2048, "filename": kind + ".jpg",
		"entity_type": "task_checklist_item", "entity_id": itemID,
	})
	expect(o.t, res, http.StatusOK)
	id := res.body["file_id"].(string)
	var key string
	if err := testDB.Pool().QueryRow(context.Background(), `SELECT s3_key FROM tenant_garrent.files WHERE id = $1`, id).Scan(&key); err != nil {
		o.t.Fatal(err)
	}
	o.svc.Storage.Backend().(*storage.Memory).Put(key, storage.ObjectInfo{Size: 2048, ContentType: "image/jpeg"})
	expect(o.t, o.do(http.MethodPost, "/api/v1/files/"+id+"/confirm", token, nil), http.StatusOK)
	return id
}

func today() string {
	loc, _ := time.LoadLocation("America/Guatemala")
	return time.Now().In(loc).Format(time.DateOnly)
}

func TestTaskScopeAndLifecycle(t *testing.T) {
	e := setup(t)
	o := e.ops()
	expect(t, o.do(http.MethodPost, "/api/v1/checklist-templates", o.admin, map[string]any{
		"name": "Estándar", "is_default": true, "items": []map[string]any{
			{"section": "Baño", "text": "Desinfectar", "requires_before_after": true},
			{"section": "Cocina", "text": "Lavar trastos", "requires_photo": true},
			{"section": "Cocina", "text": "Sacar basura"},
		},
	}), http.StatusCreated)
	p1 := o.property("TB-504", map[string]any{"access_info": "Caja de llaves 4821"})
	p2 := o.property("CY-12A", map[string]any{"access_info": "Portero"})
	o.assign(p1, "maria@garrent.gt")
	maria := o.ids["maria@garrent.gt"].String()

	// Asignar a quien no tiene la propiedad: 422.
	bad := o.do(http.MethodPost, "/api/v1/tasks", o.admin, map[string]any{
		"type": "checkout_cleaning", "property_id": p2, "date": today(), "start": "11:00", "end": "15:00", "assignee_id": maria,
	})
	expect(t, bad, http.StatusUnprocessableEntity)

	res := o.do(http.MethodPost, "/api/v1/tasks", o.admin, map[string]any{
		"type": "checkout_cleaning", "property_id": p1, "date": today(), "start": "11:00", "end": "15:00",
		"assignee_id": maria, "priority": "high",
	})
	expect(t, res, http.StatusCreated)
	taskID := res.body["id"].(string)
	if res.body["type"] != "checkout_cleaning" || res.body["status"] != "pending" || res.body["assignee_name"] == nil {
		t.Fatalf("tarea creada: %v", res.body)
	}
	if cl := res.body["checklist"].([]any); len(cl) != 2 {
		t.Errorf("checklist por secciones: %v", cl)
	}
	other := o.do(http.MethodPost, "/api/v1/tasks", o.admin, map[string]any{
		"type": "deep_cleaning", "property_id": p2, "date": today(), "start": "09:00", "end": "12:00",
	})
	expect(t, other, http.StatusCreated)

	// Alcance: limpieza solo ve la suya; mantenimiento sin asignaciones nada.
	mine := o.do(http.MethodGet, "/api/v1/tasks?date="+today(), o.cleaner, nil)
	expect(t, mine, http.StatusOK)
	if got := ids(mine); len(got) != 1 || got[0] != taskID {
		t.Errorf("limpieza ve %v", got)
	}
	if got := ids(o.do(http.MethodGet, "/api/v1/tasks?from="+today()+"&to="+today(), o.maint, nil)); len(got) != 0 {
		t.Errorf("mantenimiento ve %v", got)
	}
	if got := ids(o.do(http.MethodGet, "/api/v1/tasks?from="+today()+"&to="+today(), o.super, nil)); len(got) != 2 {
		t.Errorf("supervision ve %v", got)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/tasks/"+other.body["id"].(string), o.cleaner, nil), http.StatusNotFound)

	// El acceso de la propiedad: hoy si (tiene tarea), la otra no.
	expect(t, o.do(http.MethodGet, "/api/v1/properties/"+p1+"/access-info", o.cleaner, nil), http.StatusOK)
	o.assign(p2, "maria@garrent.gt")
	if res := o.do(http.MethodGet, "/api/v1/properties/"+p2+"/access-info", o.cleaner, nil); res.status == http.StatusOK {
		t.Errorf("sin tarea hoy no ve el acceso: %v", res.body)
	}

	// Inventario: bodega central, toallas (se devuelven) y jabon (consumible).
	wh := o.do(http.MethodPost, "/api/v1/inventory/warehouses", o.admin, map[string]any{"name": "Bodega central"})
	expect(t, wh, http.StatusCreated)
	whID := wh.body["id"].(string)
	towels := o.do(http.MethodPost, "/api/v1/inventory/items", o.admin, map[string]any{
		"sku": "TOA-01", "name": "Toalla de baño", "category": "linens", "unit": "unidad", "min": 2,
	})
	expect(t, towels, http.StatusCreated)
	soap := o.do(http.MethodPost, "/api/v1/inventory/items", o.admin, map[string]any{
		"sku": "JAB-01", "name": "Jabón", "category": "amenities", "unit": "unidad", "min": 0,
	})
	expect(t, soap, http.StatusCreated)
	towelID, soapID := towels.body["id"].(string), soap.body["id"].(string)
	for _, it := range []string{towelID, soapID} {
		expect(t, o.do(http.MethodPost, "/api/v1/inventory/movements", o.admin, map[string]any{
			"kind": "entry", "item_id": it, "warehouse_id": whID, "qty": 10,
		}), http.StatusNoContent)
	}
	// Nunca negativo: trasladar mas de lo que hay falla.
	wh2 := o.do(http.MethodPost, "/api/v1/inventory/warehouses", o.admin, map[string]any{"name": "Bodega TB", "property_id": p1})
	expect(t, wh2, http.StatusCreated)
	expect(t, o.do(http.MethodPost, "/api/v1/inventory/movements", o.admin, map[string]any{
		"kind": "transfer", "item_id": towelID, "warehouse_id": whID, "to_warehouse_id": wh2.body["id"], "qty": 50,
	}), http.StatusUnprocessableEntity)
	// Limpieza no hace entradas, si saca a su custodia.
	expect(t, o.do(http.MethodPost, "/api/v1/inventory/movements", o.cleaner, map[string]any{
		"kind": "entry", "item_id": towelID, "warehouse_id": whID, "qty": 1,
	}), http.StatusForbidden)
	for _, it := range []string{towelID, soapID} {
		expect(t, o.do(http.MethodPost, "/api/v1/inventory/movements", o.cleaner, map[string]any{
			"kind": "checkout", "item_id": it, "warehouse_id": whID, "qty": 5,
		}), http.StatusNoContent)
	}

	// Iniciar sin ser el asignado: 403; el asignado si.
	expect(t, o.do(http.MethodPost, "/api/v1/tasks/"+taskID+"/start", o.maint, map[string]any{"lat": 14.6, "lng": -90.5}), http.StatusNotFound)
	started := o.do(http.MethodPost, "/api/v1/tasks/"+taskID+"/start", o.cleaner, map[string]any{"lat": 14.6, "lng": -90.5})
	expect(t, started, http.StatusOK)
	itemsList := started.body["items"].([]any)
	if len(itemsList) != 3 || len(started.body["custody"].([]any)) != 2 {
		t.Fatalf("detalle al iniciar: %v", started.body)
	}
	item := func(i int) string { return itemsList[i].(map[string]any)["id"].(string) }

	// Cerrar con puntos pendientes falla.
	closeBody := map[string]any{"lat": 14.6, "lng": -90.5, "custody": []map[string]any{
		{"item_id": towelID, "returned": 4, "consumed": 0, "kept": 0},
		{"item_id": soapID, "returned": 2, "consumed": 3, "kept": 0},
	}}
	expect(t, o.do(http.MethodPost, "/api/v1/tasks/"+taskID+"/complete", o.cleaner, closeBody), http.StatusUnprocessableEntity)
	for i := range itemsList {
		expect(t, o.do(http.MethodPatch, "/api/v1/tasks/"+taskID+"/checklist/"+item(i), o.cleaner, map[string]any{"is_done": true}), http.StatusOK)
	}
	// Marcados pero sin fotos obligatorias: falla.
	missing := o.do(http.MethodPost, "/api/v1/tasks/"+taskID+"/complete", o.cleaner, closeBody)
	expect(t, missing, http.StatusUnprocessableEntity)
	o.taskPhoto(o.cleaner, "before", item(0))
	o.taskPhoto(o.cleaner, "after", item(0))
	o.taskPhoto(o.cleaner, "evidence", item(1))
	// Sin declarar toda la custodia: falla.
	expect(t, o.do(http.MethodPost, "/api/v1/tasks/"+taskID+"/complete", o.cleaner, map[string]any{
		"lat": 14.6, "lng": -90.5, "custody": []map[string]any{{"item_id": soapID, "returned": 5}},
	}), http.StatusUnprocessableEntity)
	done := o.do(http.MethodPost, "/api/v1/tasks/"+taskID+"/complete", o.cleaner, closeBody)
	expect(t, done, http.StatusOK)
	if done.body["status"] != "done" || done.body["photos"].(float64) != 3 || done.body["discrepancy"] == nil {
		t.Fatalf("cerrada: %v", done.body)
	}
	if c := done.body["consumption"].([]any); len(c) != 1 || c[0].(map[string]any)["qty"].(float64) != 3 {
		t.Errorf("consumo: %v", c)
	}

	// Custodia 5 y devolucion 4: una diferencia abierta; existencias cuadran.
	ov := o.do(http.MethodGet, "/api/v1/inventory/overview", o.admin, nil)
	expect(t, ov, http.StatusOK)
	disc := ov.body["discrepancies"].([]any)
	if len(disc) != 1 || disc[0].(map[string]any)["expected"].(float64) != 5 || disc[0].(map[string]any)["returned"].(float64) != 4 {
		t.Errorf("diferencias: %v", disc)
	}
	if len(ov.body["custody"].([]any)) != 0 {
		t.Errorf("custodia vacia tras cerrar: %v", ov.body["custody"])
	}
	totals := map[string]float64{}
	for _, it := range ov.body["items"].([]any) {
		m := it.(map[string]any)
		totals[m["id"].(string)] = m["total"].(float64)
	}
	if totals[towelID] != 9 || totals[soapID] != 7 {
		t.Errorf("existencias: %v", totals)
	}
	expect(t, o.do(http.MethodPatch, "/api/v1/inventory/discrepancies/"+disc[0].(map[string]any)["id"].(string), o.admin,
		map[string]any{"status": "justified", "note": "Se manchó y se tiró"}), http.StatusNoContent)

	// Devolver exige motivo; validar la deja validada.
	expect(t, o.do(http.MethodPost, "/api/v1/tasks/"+taskID+"/return", o.super, map[string]any{"note": nil}), http.StatusUnprocessableEntity)
	val := o.do(http.MethodPost, "/api/v1/tasks/"+taskID+"/validate", o.super, map[string]any{"note": "Bien"})
	expect(t, val, http.StatusOK)
	if val.body["status"] != "validated" {
		t.Errorf("validada: %v", val.body)
	}
	types := []string{}
	for _, ev := range val.body["timeline"].([]any) {
		types = append(types, ev.(map[string]any)["type"].(string))
	}
	want := []string{"created", "assigned", "started", "finished", "discrepancy", "validated"}
	if len(types) != len(want) {
		t.Errorf("linea de tiempo %v, se esperaba %v", types, want)
	}

	// Reprogramar y reasignar (web: arrastrar).
	mv := o.do(http.MethodPost, "/api/v1/tasks/"+other.body["id"].(string)+"/reschedule", o.super, map[string]any{
		"date": time.Now().AddDate(0, 0, 1).Format(time.DateOnly), "reason": "Llega tarde el huésped",
	})
	expect(t, mv, http.StatusOK)
	assignables := o.do(http.MethodGet, "/api/v1/tasks/assignables", o.super, nil)
	expect(t, assignables, http.StatusOK)
	if len(assignables.list) < 2 {
		t.Errorf("asignables: %v", assignables.list)
	}
}

// fase02.md §7: el trabajo de retrasos marca en la hora local de la empresa.
func TestMarkLateTasks(t *testing.T) {
	e := setup(t)
	o := e.ops()
	p := o.property("TB-504", nil)
	res := o.do(http.MethodPost, "/api/v1/tasks", o.admin, map[string]any{
		"type": "touch_up", "property_id": p, "date": "2026-03-02", "start": "09:00", "end": "10:00",
	})
	expect(t, res, http.StatusCreated)
	ctx := garrentCtx()
	// 09:30 en Guatemala (UTC-6): paso el inicio sin iniciar.
	e.svc.Tasks.SetClock(func() time.Time { return time.Date(2026, 3, 2, 15, 30, 0, 0, time.UTC) })
	run := func() int {
		var n int
		if err := testDB.InTx(ctx, func(q database.Querier) error {
			var err error
			n, err = e.svc.Tasks.MarkLate(ctx, q, o.tenant)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := run(); n != 1 {
		t.Fatalf("marcadas %d", n)
	}
	if n := run(); n != 0 {
		t.Fatalf("segunda pasada %d", n)
	}
	got := o.do(http.MethodGet, "/api/v1/tasks/"+res.body["id"].(string), o.admin, nil)
	if got.body["late"] != true {
		t.Errorf("late: %v", got.body)
	}
}
