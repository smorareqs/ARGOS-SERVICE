//go:build integration

package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/smorareqs/argos-service/internal/core/database"
)

// Pruebas de la fase 5 de Persona B (fase05.md §7): marcaje con GPS y un solo
// abierto, correcciones con motivo, cierre solo a las 16 h; metricas por
// alcance, ranking por rol, metas e incentivos.

func gt() *time.Location {
	loc, _ := time.LoadLocation("America/Guatemala")
	return loc
}

func TestAttendanceFlow(t *testing.T) {
	e := setup(t)
	o := e.ops()
	gps := map[string]any{"lat": 14.6, "lng": -90.5}

	// Sin ficha de colaborador no se marca.
	expect(t, o.do(http.MethodPost, "/api/v1/attendance/clock-in", o.cleaner, gps), http.StatusUnprocessableEntity)
	maria := o.employee("maria@garrent.gt", map[string]any{"pay_type": "per_day", "pay_rate": 200})
	luis := o.employee("luis@garrent.gt", map[string]any{"pay_type": "per_hour", "pay_rate": 40})

	// GPS obligatorio.
	expect(t, o.do(http.MethodPost, "/api/v1/attendance/clock-in", o.cleaner, map[string]any{"lat": 14.6}), http.StatusUnprocessableEntity)
	expect(t, o.do(http.MethodPost, "/api/v1/attendance/clock-in", o.cleaner, map[string]any{"lat": 0, "lng": 0}), http.StatusUnprocessableEntity)
	// Foto que no es suya o no existe: 422.
	bad := map[string]any{"lat": 14.6, "lng": -90.5, "photo_file_id": "00000000-0000-0000-0000-000000000001"}
	expect(t, o.do(http.MethodPost, "/api/v1/attendance/clock-in", o.cleaner, bad), http.StatusUnprocessableEntity)

	res := o.do(http.MethodPost, "/api/v1/attendance/clock-in", o.cleaner, gps)
	expect(t, res, http.StatusCreated)
	if res.body["employee_id"] != maria || res.body["clock_out_at"] != nil || res.body["source"] != "app" {
		t.Fatalf("entrada: %v", res.body)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/attendance/clock-in", o.cleaner, gps), http.StatusConflict)

	res = o.do(http.MethodGet, "/api/v1/attendance/me", o.cleaner, nil)
	expect(t, res, http.StatusOK)
	if res.body["open"] == nil || len(items(res)) != 1 {
		t.Fatalf("mi asistencia con entrada abierta: %v", res.body)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/attendance/me?month=2026-13", o.cleaner, nil), http.StatusUnprocessableEntity)

	res = o.do(http.MethodPost, "/api/v1/attendance/clock-out", o.cleaner, gps)
	expect(t, res, http.StatusOK)
	if res.body["clock_out_at"] == nil || res.body["worked_minutes"] != float64(0) {
		t.Fatalf("salida: %v", res.body)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/attendance/clock-out", o.cleaner, gps), http.StatusConflict)

	// Solo RRHH ve el equipo y corrige.
	day := time.Now().In(gt()).AddDate(0, 0, -1)
	at := func(h int) string {
		return time.Date(day.Year(), day.Month(), day.Day(), h, 0, 0, 0, gt()).Format(time.RFC3339)
	}
	from, to := day.Format(time.DateOnly), time.Now().In(gt()).Format(time.DateOnly)
	expect(t, o.do(http.MethodGet, "/api/v1/attendance?from="+from+"&to="+to, o.cleaner, nil), http.StatusForbidden)
	manual := map[string]any{"employee_id": luis, "clock_in_at": at(8), "clock_out_at": at(16), "edit_reason": "x"}
	expect(t, o.do(http.MethodPost, "/api/v1/attendance", o.admin, manual), http.StatusUnprocessableEntity)
	manual["edit_reason"] = "Olvidó marcar"
	res = o.do(http.MethodPost, "/api/v1/attendance", o.admin, manual)
	expect(t, res, http.StatusCreated)
	if res.body["source"] != "manual" || res.body["worked_minutes"] != float64(480) || res.body["edit_reason"] != "Olvidó marcar" {
		t.Fatalf("marcaje a mano: %v", res.body)
	}
	record := res.body["id"].(string)
	expect(t, o.do(http.MethodPost, "/api/v1/attendance", o.admin, map[string]any{
		"employee_id": luis, "clock_in_at": at(10), "clock_out_at": at(12), "edit_reason": "Duplicado",
	}), http.StatusConflict)
	expect(t, o.do(http.MethodPost, "/api/v1/attendance", o.admin, map[string]any{
		"employee_id": luis, "clock_in_at": at(16), "clock_out_at": at(8), "edit_reason": "Al revés",
	}), http.StatusUnprocessableEntity)
	res = o.do(http.MethodPatch, "/api/v1/attendance/"+record, o.admin, map[string]any{"clock_out_at": at(17), "edit_reason": "Salió a las 5"})
	expect(t, res, http.StatusOK)
	if res.body["worked_minutes"] != float64(540) {
		t.Fatalf("corrección: %v", res.body)
	}
	res = o.do(http.MethodGet, "/api/v1/attendance?from="+from+"&to="+to, o.admin, nil)
	expect(t, res, http.StatusOK)
	if len(items(res)) != 2 {
		t.Fatalf("marcajes del equipo: %v", res.body)
	}
	res = o.do(http.MethodGet, "/api/v1/attendance?from="+from+"&to="+to+"&employee="+luis, o.admin, nil)
	if len(items(res)) != 1 {
		t.Fatalf("filtro por colaborador: %v", res.body)
	}

	// Entrada abierta de hace 17 h: el job la cierra a las 16 h y avisa a RRHH.
	ctx := context.Background()
	if _, err := testDB.Pool().Exec(ctx, `INSERT INTO tenant_garrent.attendance_records (employee_id, clock_in_at, in_lat, in_lng)
		VALUES ($1, now() - interval '17 hours', 14.6, -90.5)`, maria); err != nil {
		t.Fatal(err)
	}
	tctx := database.WithTenantSchema(ctx, "tenant_garrent")
	if err := testDB.InTx(tctx, func(q database.Querier) error {
		n, err := e.svc.Attendance.Autoclose(tctx, q, o.tenant)
		if err == nil && n != 1 {
			t.Errorf("cerrados = %d", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var minutes int
	var auto bool
	if err := testDB.Pool().QueryRow(ctx, `SELECT worked_minutes, autoclosed FROM tenant_garrent.attendance_records
		WHERE employee_id = $1 ORDER BY clock_in_at LIMIT 1`, maria).Scan(&minutes, &auto); err != nil {
		t.Fatal(err)
	}
	if minutes != 960 || !auto {
		t.Fatalf("cierre a las 16 h: %d %v", minutes, auto)
	}
	var notices int
	if err := testDB.Pool().QueryRow(ctx, `SELECT count(*) FROM tenant_garrent.notifications
		WHERE type = 'attendance_autoclosed' AND recipient_user_id = $1`, o.ids["ana@garrent.gt"]).Scan(&notices); err != nil {
		t.Fatal(err)
	}
	if notices != 1 {
		t.Fatalf("aviso a RRHH: %d", notices)
	}
	// Ya cerrado, puede volver a marcar.
	expect(t, o.do(http.MethodPost, "/api/v1/attendance/clock-in", o.cleaner, gps), http.StatusCreated)
}

func TestMetricsGoalsAndIncentives(t *testing.T) {
	e := setup(t)
	o := e.ops()
	o.employee("maria@garrent.gt", map[string]any{"pay_type": "per_cleaning", "pay_rate": 150})
	p1 := o.property("MT-101", nil)
	o.property("MT-202", nil)
	maria := o.ids["maria@garrent.gt"]
	ctx := context.Background()
	now := time.Now().In(gt())
	// Tres limpiezas validadas hoy de 60 min (una tarde) y una de otra persona sin ficha.
	for i, late := range []bool{false, false, true} {
		fin := now.Add(-time.Duration(i+1) * time.Minute)
		if _, err := testDB.Pool().Exec(ctx, `INSERT INTO tenant_garrent.tasks
			(property_id, type, kind, status, scheduled_date, assigned_to, started_at, finished_at, is_late)
			VALUES ($1, 'cleaning', 'checkout_cleaning', 'validated', $2, $3, $4, $5, $6)`,
			p1, now.Format(time.DateOnly), maria, fin.Add(-time.Hour), fin, late); err != nil {
			t.Fatal(err)
		}
	}

	expect(t, o.do(http.MethodGet, "/api/v1/metrics/board", o.cleaner, nil), http.StatusForbidden)
	res := o.do(http.MethodGet, "/api/v1/metrics/board?range=7d", o.admin, nil)
	expect(t, res, http.StatusOK)
	if days := res.body["per_day"].([]any); len(days) != 7 || days[6].(map[string]any)["count"] != float64(3) {
		t.Fatalf("por día: %v", res.body["per_day"])
	}
	var m map[string]any
	for _, raw := range res.body["people"].([]any) {
		if p := raw.(map[string]any); p["id"] == maria.String() {
			m = p
		}
	}
	if m == nil || m["cleanings"] != float64(3) || m["on_time"] != float64(67) || m["avg_minutes"] != float64(60) || m["late"] != float64(1) {
		t.Fatalf("María en el tablero: %v", m)
	}
	if props := res.body["properties"].([]any); len(props) != 1 || props[0].(map[string]any)["code"] != "MT-101" {
		t.Fatalf("por propiedad: %v", props)
	}
	// El propietario sin propiedades no ve limpiezas.
	res = o.do(http.MethodGet, "/api/v1/metrics/board", o.owner, nil)
	expect(t, res, http.StatusOK)
	if res.body["totals"].(map[string]any)["cleanings"] != float64(0) {
		t.Fatalf("alcance del propietario: %v", res.body["totals"])
	}
	expect(t, o.do(http.MethodGet, "/api/v1/metrics/board?range=1y", o.admin, nil), http.StatusUnprocessableEntity)

	// Supervisor: ranking del equipo (solo colaboradores con ficha).
	res = o.do(http.MethodGet, "/api/v1/metrics/team?range=month", o.super, nil)
	expect(t, res, http.StatusOK)
	team := res.body["people"].([]any)
	if len(team) != 1 || team[0].(map[string]any)["rank"] != float64(1) {
		t.Fatalf("equipo: %v", team)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/metrics/team", o.cleaner, nil), http.StatusForbidden)

	// Metas: solo quien maneja la nomina las crea.
	goal := map[string]any{"name": "Limpiezas del mes", "metric": "cleanings_done", "target": 2, "incentive_amount": 150}
	expect(t, o.do(http.MethodPost, "/api/v1/metrics/goals", o.super, goal), http.StatusForbidden)
	expect(t, o.do(http.MethodPost, "/api/v1/metrics/goals", o.admin, map[string]any{
		"name": "Para alguien", "metric": "late_count", "target": 0, "applies_to": "user",
	}), http.StatusUnprocessableEntity)
	res = o.do(http.MethodPost, "/api/v1/metrics/goals", o.admin, goal)
	expect(t, res, http.StatusCreated)
	if res.body["comparator"] != ">=" || res.body["unit"] != "limpiezas" || res.body["reward"] != "Q150" ||
		res.body["scope"] != "Todo el equipo" || res.body["current"] != float64(3) {
		t.Fatalf("meta: %v", res.body)
	}
	goalID := res.body["id"].(string)
	res = o.do(http.MethodPost, "/api/v1/metrics/goals", o.admin, map[string]any{"name": "Sin retrasos", "metric": "late_count", "target": 0})
	expect(t, res, http.StatusCreated)
	if res.body["comparator"] != "<=" {
		t.Fatalf("retrasos se miden hacia abajo: %v", res.body)
	}
	expect(t, o.do(http.MethodPatch, "/api/v1/metrics/goals/"+res.body["id"].(string), o.admin, map[string]any{"active": false}), http.StatusOK)
	res = o.do(http.MethodGet, "/api/v1/metrics/goals", o.admin, nil)
	expect(t, res, http.StatusOK)
	if len(res.list) != 1 {
		t.Fatalf("metas activas: %v", res.list)
	}

	// Mis metricas: puesto, tendencia y metas.
	res = o.do(http.MethodGet, "/api/v1/metrics/me?range=month", o.cleaner, nil)
	expect(t, res, http.StatusOK)
	me := res.body["me"].(map[string]any)
	goals := res.body["goals"].([]any)
	if me["cleanings"] != float64(3) || me["rank"] != float64(1) || len(goals) != 1 || goals[0].(map[string]any)["met"] != true {
		t.Fatalf("mis métricas: %v", res.body)
	}

	// Al cerrar el mes se crea el incentivo pendiente (una sola vez).
	first := time.Date(now.Year(), now.Month()+1, 1, 9, 0, 0, 0, gt())
	e.svc.Metrics.SetClock(func() time.Time { return first })
	defer e.svc.Metrics.SetClock(time.Now)
	tctx := database.WithTenantSchema(ctx, "tenant_garrent")
	for i, want := range []int{1, 0} {
		if err := testDB.InTx(tctx, func(q database.Querier) error {
			n, err := e.svc.Metrics.Evaluate(tctx, q, o.tenant)
			if err == nil && n != want {
				t.Errorf("evaluación %d: %d incentivos", i, n)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	e.svc.Metrics.SetClock(time.Now)
	expect(t, o.do(http.MethodGet, "/api/v1/metrics/incentives", o.super, nil), http.StatusForbidden)
	res = o.do(http.MethodGet, "/api/v1/metrics/incentives", o.admin, nil)
	expect(t, res, http.StatusOK)
	if len(res.list) != 1 {
		t.Fatalf("incentivos: %v", res.list)
	}
	inc := res.list[0].(map[string]any)
	if inc["status"] != "pending" || inc["amount"] != float64(150) || inc["goal_id"] != goalID || inc["achieved_value"] != float64(3) {
		t.Fatalf("incentivo: %v", inc)
	}
	res = o.do(http.MethodPost, "/api/v1/metrics/incentives/decide", o.admin, map[string]any{"ids": []string{inc["id"].(string)}, "status": "approved"})
	expect(t, res, http.StatusOK)
	if res.list[0].(map[string]any)["status"] != "approved" {
		t.Fatalf("aprobado: %v", res.list)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/metrics/incentives/decide", o.admin, map[string]any{"ids": []string{inc["id"].(string)}, "status": "rejected"}),
		http.StatusUnprocessableEntity)

	// Foto diaria.
	if err := testDB.InTx(tctx, func(q database.Querier) error {
		d, _ := time.Parse(time.DateOnly, now.Format(time.DateOnly))
		n, err := e.svc.Metrics.Snapshot(tctx, q, d)
		if err == nil && n != 1 {
			t.Errorf("fotos = %d", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var done int
	if err := testDB.Pool().QueryRow(ctx, `SELECT cleanings_done FROM tenant_garrent.performance_snapshots`).Scan(&done); err != nil || done != 3 {
		t.Fatalf("foto del día: %d %v", done, err)
	}
}
