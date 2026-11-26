//go:build integration

package server_test

import (
	"bytes"
	"context"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/modules/payroll"
)

// Pruebas de la fase 5 de Persona A (fase05.md §7): esquemas de pago, IGSS solo
// en planilla, Bono 14 / aguinaldo proporcional, un periodo cerrado no se
// recalcula (409), cada colaborador solo ve sus boletas (404 en ajenas), Excel
// y PDF en files bajo el prefijo del tenant; disciplina con acuse.

// payrollOn activa la bandera payroll de la empresa.
func (o *opsEnv) payrollOn() {
	o.t.Helper()
	if err := o.svc.Tenants.SetFeatures(context.Background(), o.tenant, map[string]bool{"reservations": true, "payroll": true}); err != nil {
		o.t.Fatal(err)
	}
}

// employee crea la ficha laboral de una persona y devuelve su id.
func (o *opsEnv) employee(email string, body map[string]any) string {
	o.t.Helper()
	body["user_id"] = o.ids[email].String()
	if _, ok := body["position"]; !ok {
		body["position"] = "Colaborador"
	}
	if _, ok := body["hire_date"]; !ok {
		body["hire_date"] = "2023-01-02"
	}
	res := o.do(http.MethodPost, "/api/v1/employees", o.admin, body)
	expect(o.t, res, http.StatusCreated)
	return res.body["id"].(string)
}

// fetch hace una peticion y devuelve el cuerpo crudo (Excel o PDF).
func (o *opsEnv) fetch(path, token string) (int, string, []byte) {
	o.t.Helper()
	req, err := http.NewRequest(http.MethodGet, o.srv.URL+path, nil)
	if err != nil {
		o.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		o.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		o.t.Fatal(err)
	}
	return res.StatusCode, res.Header.Get("Content-Type"), raw
}

func entryOf(body map[string]any, employeeID string) map[string]any {
	list, _ := body["entries"].([]any)
	for _, raw := range list {
		e := raw.(map[string]any)
		if e["employee_id"] == employeeID {
			return e
		}
	}
	return nil
}

func num(v any) float64 {
	f, _ := v.(float64)
	return math.Round(f*100) / 100
}

func TestPayrollCycle(t *testing.T) {
	e := setup(t)
	o := e.ops()
	jorge := o.employee("jorge@garrent.gt", map[string]any{"pay_type": "fixed_monthly", "pay_rate": 7800, "on_payroll": true})
	maria := o.employee("maria@garrent.gt", map[string]any{"pay_type": "per_cleaning", "pay_rate": 165, "on_payroll": true})
	luis := o.employee("luis@garrent.gt", map[string]any{"pay_type": "per_day", "pay_rate": 260, "on_payroll": true})
	sofia := o.employee("sofia@garrent.gt", map[string]any{"pay_type": "per_hour", "pay_rate": 38, "schedule": []map[string]any{
		{"weekday": 1, "starts_at": "08:00", "ends_at": "12:00"}, {"weekday": 3, "starts_at": "08:00", "ends_at": "12:00"},
	}})

	// Sin la bandera payroll, la nomina no existe para la empresa.
	res := o.do(http.MethodGet, "/api/v1/payroll/periods", o.admin, nil)
	expect(t, res, http.StatusForbidden)
	o.payrollOn()

	res = o.do(http.MethodPost, "/api/v1/payroll/periods", o.admin, map[string]any{
		"kind": "biweekly", "period_start": "2026-11-01", "period_end": "2026-11-15",
	})
	expect(t, res, http.StatusCreated)
	period := res.body["id"].(string)
	if res.body["name"] != "Quincena 1–15 nov 2026" || res.body["status"] != "draft" {
		t.Fatalf("periodo = %v %v", res.body["name"], res.body["status"])
	}
	res = o.do(http.MethodPost, "/api/v1/payroll/periods", o.admin, map[string]any{
		"kind": "biweekly", "period_start": "2026-11-10", "period_end": "2026-11-20",
	})
	expect(t, res, http.StatusUnprocessableEntity)

	// Asistencia (Persona B): Luis 8 h de lunes a viernes (10 dias) y Sofia
	// 4 h los lunes y miercoles (4 dias, 16 h). Un dia de 3 h no cuenta como dia.
	punch := func(employee, date, in, out string) {
		t.Helper()
		expect(t, o.do(http.MethodPost, "/api/v1/attendance", o.admin, map[string]any{
			"employee_id": employee, "clock_in_at": date + "T" + in + ":00-06:00", "clock_out_at": date + "T" + out + ":00-06:00",
			"edit_reason": "Carga de la quincena",
		}), http.StatusCreated)
	}
	for _, d := range []string{"02", "03", "04", "05", "06", "09", "10", "11", "12", "13"} {
		punch(luis, "2026-11-"+d, "08:00", "16:00")
	}
	punch(luis, "2026-11-14", "08:00", "11:00")
	for _, d := range []string{"02", "04", "09", "11"} {
		punch(sofia, "2026-11-"+d, "08:00", "12:00")
	}

	base := "/api/v1/payroll/periods/" + period
	res = o.do(http.MethodPost, base+"/compute", o.admin, nil)
	expect(t, res, http.StatusOK)
	src := res.body["sources"].(map[string]any)
	if src["attendance"] != true || src["tasks"] != true {
		t.Fatalf("con asistencia y tareas (Persona B) el calculo las usa: %v", src)
	}
	// Salario fijo: 7800 / 2; IGSS 4.83 % de la base; bonificacion incentivo 125.
	j := entryOf(res.body, jorge)
	if num(j["base_amount"]) != 3900 || num(j["igss_employee"]) != 188.37 || num(j["incentive_bonus_amount"]) != 125 ||
		num(j["gross_amount"]) != 4025 || num(j["net_amount"]) != 3836.63 {
		t.Fatalf("salario fijo: %v", j)
	}
	// Por dia con marcajes: 10 dias de 8 h (el de 3 h no llega al minimo de 4 h).
	l := entryOf(res.body, luis)
	if num(l["days_worked"]) != 10 || num(l["base_amount"]) != 2600 || l["attendance_source"] != "attendance" {
		t.Fatalf("por día: %v", l)
	}
	// Por hora fuera de planilla: lunes y miercoles 4 h (4 dias marcados) sin IGSS ni bonificacion.
	s := entryOf(res.body, sofia)
	if num(s["hours_worked"]) != 16 || num(s["base_amount"]) != 608 || num(s["igss_employee"]) != 0 || num(s["incentive_bonus_amount"]) != 0 {
		t.Fatalf("por hora fuera de planilla: %v", s)
	}
	m := entryOf(res.body, maria)
	if num(m["cleanings_count"]) != 0 || num(m["base_amount"]) != 0 {
		t.Fatalf("por limpieza sin itinerario: %v", m)
	}
	mariaEntry := m["id"].(string)

	// En borrador no se corrige; en revision si, con historial.
	res = o.do(http.MethodPatch, base+"/entries/"+mariaEntry, o.admin, map[string]any{"cleanings_count": 20})
	expect(t, res, http.StatusConflict)
	expect(t, o.do(http.MethodPost, base+"/review", o.admin, nil), http.StatusOK)
	expect(t, o.do(http.MethodPost, base+"/compute", o.admin, nil), http.StatusConflict)
	res = o.do(http.MethodPatch, base+"/entries/"+mariaEntry, o.admin, map[string]any{
		"cleanings_count": 20, "other_deductions": 50, "reason": "Conteo del supervisor",
	})
	expect(t, res, http.StatusOK)
	if num(res.body["base_amount"]) != 3300 || num(res.body["igss_employee"]) != 159.39 || num(res.body["net_amount"]) != 3215.61 {
		t.Fatalf("corrección: %v", res.body)
	}
	history, _ := res.body["history"].([]any)
	if len(history) != 3 || !strings.Contains(strings.Join(toStrings(res.body["manual_fields"]), ","), "cleanings_count") {
		t.Fatalf("historial: %v · %v", history, res.body["manual_fields"])
	}
	expect(t, o.do(http.MethodPatch, base+"/entries/"+mariaEntry, o.admin, map[string]any{"cleanings_count": 20}), http.StatusUnprocessableEntity)

	// Documentos al vuelo antes del cierre.
	status, ct, raw := o.fetch(base+"/export.xlsx", o.admin)
	if status != http.StatusOK || ct != payroll.ContentTypeXLSX || !bytes.HasPrefix(raw, []byte("PK")) {
		t.Fatalf("Excel: %d %s", status, ct)
	}
	status, _, raw = o.fetch(base+"/payslips.pdf", o.admin)
	if status != http.StatusOK || !bytes.HasPrefix(raw, []byte("%PDF")) || bytes.Count(raw, []byte("/Type /Page ")) != 4 {
		t.Fatalf("boletas: %d, %d páginas", status, bytes.Count(raw, []byte("/Type /Page ")))
	}
	// La colaboradora no ve la nomina ni su boleta mientras no se cierra.
	expect(t, o.do(http.MethodGet, base, o.cleaner, nil), http.StatusForbidden)
	if status, _, _ := o.fetch("/api/v1/payroll/entries/"+mariaEntry+"/payslip.pdf", o.cleaner); status != http.StatusNotFound {
		t.Fatalf("boleta de un periodo abierto = %d", status)
	}

	var mu sync.Mutex
	var closed []payroll.ClosedEvent
	o.svc.Events.Subscribe(payroll.EventClosed, func(_ context.Context, _ database.Querier, ev events.Event) error {
		mu.Lock()
		defer mu.Unlock()
		closed = append(closed, ev.Data.(payroll.ClosedEvent))
		return nil
	})
	before := queuedEmails(t, "payslip_available")
	res = o.do(http.MethodPost, base+"/close", o.admin, nil)
	expect(t, res, http.StatusOK)
	if res.body["status"] != "closed" || res.body["has_export"] != true {
		t.Fatalf("cierre: %v", res.body)
	}
	if got := queuedEmails(t, "payslip_available") - before; got != 4 {
		t.Fatalf("correos de boleta = %d", got)
	}
	if len(closed) != 1 || len(closed[0].Entries) != 4 {
		t.Fatalf("evento payroll.closed = %+v", closed)
	}
	var files int
	if err := testDB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM tenant_garrent.files
		WHERE module = 'payroll' AND status = 'ready' AND s3_key LIKE 'tenants/' || $1 || '/payroll/%'`, o.tenant.String()).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if files != 5 {
		t.Fatalf("archivos guardados = %d (Excel + 4 boletas)", files)
	}

	// Cerrado es inmutable.
	expect(t, o.do(http.MethodPost, base+"/close", o.admin, nil), http.StatusConflict)
	expect(t, o.do(http.MethodPost, base+"/compute", o.admin, nil), http.StatusConflict)
	expect(t, o.do(http.MethodPatch, base+"/entries/"+mariaEntry, o.admin, map[string]any{"other_deductions": 0}), http.StatusConflict)
	expect(t, o.do(http.MethodDelete, base, o.admin, nil), http.StatusConflict)

	// Cada quien ve solo sus boletas.
	res = o.do(http.MethodGet, "/api/v1/payroll/me", o.cleaner, nil)
	expect(t, res, http.StatusOK)
	mine := items(res)
	if len(mine) != 1 || mine[0]["id"] != mariaEntry || mine[0]["payslip_url"] == nil {
		t.Fatalf("mis boletas: %v", mine)
	}
	if status, _, raw := o.fetch("/api/v1/payroll/entries/"+mariaEntry+"/payslip.pdf", o.cleaner); status != http.StatusOK || !bytes.HasPrefix(raw, []byte("%PDF")) {
		t.Fatalf("mi boleta = %d", status)
	}
	if status, _, _ := o.fetch("/api/v1/payroll/entries/"+j["id"].(string)+"/payslip.pdf", o.cleaner); status != http.StatusNotFound {
		t.Fatalf("boleta ajena = %d", status)
	}
	// La ficha del colaborador lista sus lineas.
	res = o.do(http.MethodGet, "/api/v1/payroll/entries?employee_id="+maria, o.admin, nil)
	expect(t, res, http.StatusOK)
	if list := items(res); len(list) != 1 || list[0]["period_status"] != "closed" {
		t.Fatalf("nómina de la colaboradora: %v", list)
	}
}

func TestPayrollBonusesAndSettings(t *testing.T) {
	e := setup(t)
	o := e.ops()
	o.payrollOn()
	// Ingresó el 1 de junio: 183 de 365 días de la ventana del aguinaldo (dic 2025 a nov 2026) → 6000 × 183 / 365.
	half := o.employee("jorge@garrent.gt", map[string]any{"pay_type": "fixed_monthly", "pay_rate": 6000, "on_payroll": true, "hire_date": "2026-06-01"})
	// Ingresó a media quincena: base proporcional (8 de 15 días); sus meses caen fuera de la ventana, sin aguinaldo.
	late := o.employee("maria@garrent.gt", map[string]any{"pay_type": "fixed_monthly", "pay_rate": 4000, "on_payroll": true, "hire_date": "2026-12-08"})
	out := o.employee("sofia@garrent.gt", map[string]any{"pay_type": "fixed_monthly", "pay_rate": 5000, "on_payroll": false})

	res := o.do(http.MethodPost, "/api/v1/payroll/periods", o.admin, map[string]any{
		"kind": "biweekly", "period_start": "2026-12-01", "period_end": "2026-12-15",
	})
	expect(t, res, http.StatusCreated)
	if res.body["aguinaldo"] != true || res.body["bonus14"] != false {
		t.Fatalf("la quincena del 1 al 15 de diciembre paga aguinaldo: %v", res.body)
	}
	base := "/api/v1/payroll/periods/" + res.body["id"].(string)
	res = o.do(http.MethodPost, base+"/compute", o.admin, nil)
	expect(t, res, http.StatusOK)
	h := entryOf(res.body, half)
	if num(h["aguinaldo_amount"]) != 3008.22 || num(h["igss_employee"]) != 144.9 {
		t.Fatalf("aguinaldo proporcional (sin IGSS): %v", h)
	}
	detail, _ := h["bonus_detail"].([]any)
	if len(detail) != 1 || detail[0].(map[string]any)["months_estimated"] != float64(6) {
		t.Fatalf("detalle del aguinaldo: %v", detail)
	}
	l := entryOf(res.body, late)
	if num(l["base_amount"]) != 1066.67 || num(l["aguinaldo_amount"]) != 0 {
		t.Fatalf("ingreso a media quincena: %v", l)
	}
	if x := entryOf(res.body, out); num(x["aguinaldo_amount"]) != 0 || num(x["igss_employee"]) != 0 {
		t.Fatalf("fuera de planilla: %v", x)
	}
	if res.body["totals"].(map[string]any)["igss_employer"] == float64(0) {
		t.Fatal("la cuota patronal se estima para el Excel")
	}

	// La configuracion aplica desde el siguiente calculo.
	res = o.do(http.MethodGet, "/api/v1/payroll/settings", o.admin, nil)
	expect(t, res, http.StatusOK)
	cfg := res.body
	cfg["igss_employee_rate"] = 0.05
	delete(cfg, "updated_at")
	delete(cfg, "$schema")
	expect(t, o.do(http.MethodPut, "/api/v1/payroll/settings", o.admin, cfg), http.StatusOK)
	res = o.do(http.MethodPost, base+"/compute", o.admin, nil)
	expect(t, res, http.StatusOK)
	if got := num(entryOf(res.body, half)["igss_employee"]); got != 150 {
		t.Fatalf("IGSS con la tasa nueva = %v", got)
	}
	// Un borrador se puede borrar.
	expect(t, o.do(http.MethodDelete, base, o.admin, nil), http.StatusNoContent)
	expect(t, o.do(http.MethodGet, base, o.admin, nil), http.StatusNotFound)
}

func TestDisciplinaryActions(t *testing.T) {
	e := setup(t)
	o := e.ops()
	maria := o.employee("maria@garrent.gt", map[string]any{"pay_type": "per_cleaning", "pay_rate": 165})
	o.employee("luis@garrent.gt", map[string]any{"pay_type": "per_day", "pay_rate": 260})

	// Reglas de suspension.
	res := o.do(http.MethodPost, "/api/v1/disciplinary", o.admin, map[string]any{
		"employee_id": maria, "type": "suspension", "occurred_on": "2026-11-20", "description": "Dejó la llave en otra caja.",
	})
	expect(t, res, http.StatusUnprocessableEntity)
	res = o.do(http.MethodPost, "/api/v1/disciplinary", o.admin, map[string]any{
		"employee_id": maria, "type": "verbal", "occurred_on": "2026-11-20", "description": "Dejó la llave en otra caja.",
		"suspension_from": "2026-11-21", "suspension_to": "2026-11-22",
	})
	expect(t, res, http.StatusUnprocessableEntity)

	// Con adjunto: solo los que subio quien registra.
	mine := o.uploadIn(o.admin, "disciplinary", "attachment", "application/pdf", "acta.pdf")
	before := queuedEmails(t, "disciplinary_notice")
	res = o.do(http.MethodPost, "/api/v1/disciplinary", o.admin, map[string]any{
		"employee_id": maria, "type": "written", "occurred_on": "2026-11-20",
		"description": "Dejó la llave de Casa San Sebastián en la caja equivocada.", "attachment_file_ids": []string{mine},
	})
	expect(t, res, http.StatusCreated)
	action := res.body["id"].(string)
	att, _ := res.body["attachments"].([]any)
	if len(att) != 1 || att[0].(map[string]any)["url"] == "" {
		t.Fatalf("adjuntos: %v", att)
	}
	if queuedEmails(t, "disciplinary_notice")-before != 1 {
		t.Fatal("el colaborador recibe el aviso por correo")
	}

	// Pendientes de acuse para quien administra; la colaboradora ve las suyas.
	res = o.do(http.MethodGet, "/api/v1/disciplinary?pending=true", o.admin, nil)
	expect(t, res, http.StatusOK)
	if len(items(res)) != 1 {
		t.Fatalf("pendientes = %d", len(items(res)))
	}
	res = o.do(http.MethodGet, "/api/v1/disciplinary/me", o.cleaner, nil)
	expect(t, res, http.StatusOK)
	if list := items(res); len(list) != 1 || len(list[0]["attachments"].([]any)) != 1 {
		t.Fatalf("mi historial: %v", list)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/disciplinary/"+action, o.cleaner, nil), http.StatusOK)
	expect(t, o.do(http.MethodGet, "/api/v1/disciplinary/"+action, o.maint, nil), http.StatusNotFound)
	expect(t, o.do(http.MethodPost, "/api/v1/disciplinary/"+action+"/acknowledge", o.maint, map[string]any{}), http.StatusNotFound)
	expect(t, o.do(http.MethodGet, "/api/v1/disciplinary/me", o.admin, nil), http.StatusForbidden)

	res = o.do(http.MethodPost, "/api/v1/disciplinary/"+action+"/acknowledge", o.cleaner, map[string]any{"note": "Ya la cambié de caja."})
	expect(t, res, http.StatusOK)
	if res.body["acknowledged_at"] == nil || res.body["acknowledgement_note"] != "Ya la cambié de caja." {
		t.Fatalf("acuse: %v", res.body)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/disciplinary/"+action+"/acknowledge", o.cleaner, map[string]any{}), http.StatusUnprocessableEntity)
	res = o.do(http.MethodGet, "/api/v1/disciplinary?pending=true", o.admin, nil)
	expect(t, res, http.StatusOK)
	if len(items(res)) != 0 {
		t.Fatal("con acuse ya no está pendiente")
	}
	// Un adjunto ajeno o ya usado no se liga.
	res = o.do(http.MethodPost, "/api/v1/disciplinary", o.admin, map[string]any{
		"employee_id": maria, "type": "other", "occurred_on": "2026-11-21", "description": "Uniforme incompleto en la entrega.",
		"attachment_file_ids": []string{mine},
	})
	expect(t, res, http.StatusUnprocessableEntity)
}
