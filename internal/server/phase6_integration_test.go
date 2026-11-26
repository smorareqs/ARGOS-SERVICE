//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/app"
	"github.com/smorareqs/argos-service/internal/core/config"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/billing"
	"github.com/smorareqs/argos-service/internal/server"
	"github.com/smorareqs/argos-service/internal/testutil"
)

// Fase 6 (Persona A): facturacion, solo lectura, claves API, reservas desde el
// sitio web, limites, /health/ready y la suite de aislamiento generada.

// raw hace una peticion con cabeceras propias y cuerpo crudo.
func (e *env) raw(method, path string, headers map[string]string, body []byte) response {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	out := response{status: res.StatusCode, header: res.Header}
	if len(data) > 0 && strings.Contains(res.Header.Get("Content-Type"), "json") {
		_ = json.Unmarshal(data, &out.body)
	}
	return out
}

// setupWith es setup con una configuracion propia (limites de peticiones).
func setupWith(t *testing.T, mod func(*config.Config)) *env {
	t.Helper()
	ctx := context.Background()
	if err := testutil.Reset(ctx, testDB); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Environment: config.Testing, SecretKey: "test-secret", JWTAlgorithm: "HS256",
		AccessTokenExpireMinutes: 15, RefreshTokenExpireDays: 60, CORSOrigins: []string{"*"}, DBMaxConns: 10,
	}
	mod(&cfg)
	svc, err := app.NewServices(cfg, testDB, testutil.Logger)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(cfg, testutil.Logger, svc))
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, svc: svc}
}

func sub(res response) map[string]any {
	s, _ := res.body["subscription"].(map[string]any)
	return s
}

func (e *env) cycleAt(t time.Time) billing.CycleStats {
	e.t.Helper()
	e.svc.Billing.SetClock(func() time.Time { return t })
	st, err := e.svc.Billing.Cycle(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

// fase06.md §3.2 y §6: prueba, factura, vencida, suspendida (solo lectura) y
// vuelve a operar al pagar.
func TestBillingLifecycle(t *testing.T) {
	e := setup(t)
	o := e.ops()
	now := time.Now()

	res := o.do(http.MethodGet, "/api/v1/billing/subscription", o.admin, nil)
	expect(t, res, http.StatusOK)
	if s := sub(res); s["status"] != "trialing" || s["plan_key"] != "completo" || s["read_only"] != false {
		t.Fatalf("empresa nueva: %v", s)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/billing/subscription", o.cleaner, nil), http.StatusForbidden)
	expect(t, o.do(http.MethodPost, "/api/v1/billing/checkout", o.admin, map[string]any{}), http.StatusUnprocessableEntity)

	// Termina la prueba: primera factura (IVA incluido) y queda activa.
	if st := e.cycleAt(now.AddDate(0, 0, 15)); st.Invoiced != 1 {
		t.Fatalf("ciclo al terminar la prueba: %+v", st)
	}
	inv := items(o.do(http.MethodGet, "/api/v1/billing/invoices", o.admin, nil))
	if len(inv) != 1 || inv[0]["status"] != "open" || num(inv[0]["total"]) != 2900 || num(inv[0]["tax"]) != 310.71 {
		t.Fatalf("factura: %v", inv)
	}
	if !strings.HasPrefix(inv[0]["number"].(string), "ARG-") {
		t.Fatalf("número: %v", inv[0]["number"])
	}
	// Vence sin pago: past_due; pasa la gracia (7 dias): suspendida.
	if st := e.cycleAt(now.AddDate(0, 0, 21)); st.PastDue != 1 {
		t.Fatalf("vencida: %+v", st)
	}
	if st := e.cycleAt(now.AddDate(0, 0, 28)); st.Suspended != 1 {
		t.Fatalf("suspendida: %+v", st)
	}
	if queuedEmails(t, "billing_notice") < 3 {
		t.Fatal("factura, vencimiento y suspensión avisan por correo")
	}

	// Solo lectura: leer si, escribir no (402), facturacion si.
	me := o.do(http.MethodGet, "/api/v1/auth/me", o.admin, nil)
	tenant := me.body["tenant"].(map[string]any)
	if tenant["read_only"] != true || tenant["billing_status"] != "suspended" {
		t.Fatalf("me: %v", tenant)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/properties", o.admin, nil), http.StatusOK)
	blocked := o.do(http.MethodPost, "/api/v1/properties", o.admin, map[string]any{"code": "X-1", "name": "Nueva", "type": "apartment"})
	expect(t, blocked, http.StatusPaymentRequired)
	if !strings.Contains(message(blocked), "solo lectura") {
		t.Fatalf("mensaje del 402: %v", blocked.body)
	}

	// Paga: vuelve a operar al momento.
	paid := o.do(http.MethodPost, "/api/v1/billing/checkout", o.admin, map[string]any{})
	expect(t, paid, http.StatusOK)
	ov := paid.body["overview"].(map[string]any)["subscription"].(map[string]any)
	if paid.body["status"] != "succeeded" || ov["status"] != "active" || ov["payment_method"] == nil {
		t.Fatalf("pago: %v", paid.body)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/properties", o.admin, map[string]any{"code": "X-1", "name": "Nueva", "type": "apartment"}), http.StatusCreated)

	// PDF de la factura: propio si, ajeno 404.
	status, ctype, body := o.fetch("/api/v1/billing/invoices/"+inv[0]["id"].(string)+"/pdf", o.admin)
	if status != http.StatusOK || ctype != "application/pdf" || !bytes.HasPrefix(body, []byte("%PDF")) {
		t.Fatalf("pdf: %d %s", status, ctype)
	}
	b := e.tenant("costa_limpia", "Costa Limpia")
	e.member("rosa@costa.gt", map[uuid.UUID]authz.RoleKey{b: authz.RoleAdmin})
	other, _ := e.session("rosa@costa.gt")
	if status, _, _ := o.fetch("/api/v1/billing/invoices/"+inv[0]["id"].(string)+"/pdf", other); status != http.StatusNotFound {
		t.Fatalf("factura ajena: %d", status)
	}

	// Cancelar: sigue hasta fin de periodo y luego solo lectura.
	cancel := o.do(http.MethodPost, "/api/v1/billing/cancel", o.admin, nil)
	expect(t, cancel, http.StatusOK)
	if sub(cancel)["cancel_at_period_end"] != true {
		t.Fatalf("cancelar: %v", sub(cancel))
	}
	if st := e.cycleAt(now.AddDate(0, 2, 0)); st.Cancelled != 1 {
		t.Fatalf("fin del periodo cancelado: %+v", st)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/buildings", o.admin, map[string]any{"name": "Torre"}), http.StatusPaymentRequired)
	// Volver con un plan: paga y opera.
	back := o.do(http.MethodPost, "/api/v1/billing/checkout", o.admin, map[string]any{"plan_key": "completo", "cycle": "yearly"})
	expect(t, back, http.StatusOK)
	if s := back.body["overview"].(map[string]any)["subscription"].(map[string]any); s["status"] != "active" || s["cycle"] != "yearly" {
		t.Fatalf("volver: %v", s)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/buildings", o.admin, map[string]any{"name": "Torre"}), http.StatusCreated)
}

// Limites del plan, cambio de plan y administracion de la plataforma.
func TestPlansAndLimits(t *testing.T) {
	e := setup(t)
	o := e.ops()
	o.payrollOn()
	platform, _ := e.session(platformEmail)

	created := o.do(http.MethodPost, "/api/v1/platform/plans", platform, map[string]any{
		"key": "mini", "name": "Mini", "price_monthly": 100, "price_yearly": 1000,
		"limits": map[string]any{"max_properties": 2, "max_users": 8, "features": []string{"reservations"}},
	})
	expect(t, created, http.StatusCreated)
	expect(t, o.do(http.MethodPost, "/api/v1/platform/plans", o.admin, map[string]any{}), http.StatusForbidden)
	expect(t, o.do(http.MethodPost, "/api/v1/platform/plans", platform, map[string]any{
		"key": "mini", "name": "Otro", "price_monthly": 1, "price_yearly": 1, "limits": map[string]any{"features": []string{}},
	}), http.StatusConflict)

	o.property("P-1", nil)
	o.property("P-2", nil)
	o.property("P-3", nil)
	// No cabe: 3 propiedades en un plan de 2.
	tooBig := o.do(http.MethodPost, "/api/v1/billing/checkout", o.admin, map[string]any{"plan_key": "mini"})
	expect(t, tooBig, http.StatusUnprocessableEntity)
	if !strings.Contains(message(tooBig), "permite 2 propiedades") {
		t.Fatalf("mensaje: %v", tooBig.body)
	}
	props := items(o.do(http.MethodGet, "/api/v1/properties", o.admin, nil))
	expect(t, o.do(http.MethodDelete, "/api/v1/properties/"+props[0]["id"].(string), o.admin, nil), http.StatusNoContent)

	changed := o.do(http.MethodPost, "/api/v1/billing/checkout", o.admin, map[string]any{"plan_key": "mini"})
	expect(t, changed, http.StatusOK)
	s := changed.body["overview"].(map[string]any)["subscription"].(map[string]any)
	if s["plan_key"] != "mini" || s["status"] != "active" || changed.body["status"] != "succeeded" {
		t.Fatalf("cambio de plan: %v", changed.body)
	}
	// La nomina no esta en Mini: se apago.
	if f := o.do(http.MethodGet, "/api/v1/auth/me", o.admin, nil).body["tenant"].(map[string]any)["features"].(map[string]any); f["payroll"] != false {
		t.Fatalf("banderas: %v", f)
	}
	// Limite de propiedades (2) y de personas (8: hay 6 miembros).
	full := o.do(http.MethodPost, "/api/v1/properties", o.admin, map[string]any{"code": "P-9", "name": "Otra", "type": "apartment"})
	expect(t, full, http.StatusUnprocessableEntity)
	inv := func(email string) response {
		return o.do(http.MethodPost, e.tenantPath(o.tenant, "/invitations"), o.admin, map[string]any{"email": email, "role_key": "cleaner"})
	}
	expect(t, inv("a1@garrent.gt"), http.StatusCreated)
	expect(t, inv("a2@garrent.gt"), http.StatusCreated)
	if res := inv("a3@garrent.gt"); res.status != http.StatusUnprocessableEntity || !strings.Contains(message(res), "permite 8 personas") {
		t.Fatalf("límite de personas: %d %v", res.status, res.body)
	}
	// La plataforma no puede activar una bandera fuera del plan.
	flag := o.do(http.MethodPatch, "/api/v1/platform/tenants/"+o.tenant.String(), platform, map[string]any{"features": map[string]bool{"reservations": true, "payroll": true}})
	if flag.status != http.StatusUnprocessableEntity || !strings.Contains(message(flag), "no incluye Nómina") {
		t.Fatalf("bandera fuera del plan: %d %v", flag.status, flag.body)
	}

	// Plataforma: suscripciones, prueba y reactivacion.
	list := items(o.do(http.MethodGet, "/api/v1/platform/subscriptions", platform, nil))
	if len(list) != 1 || list[0]["plan_key"] != "mini" || list[0]["tenant_name"] != "Garrent" {
		t.Fatalf("suscripciones: %v", list)
	}
	trial := o.do(http.MethodPost, "/api/v1/platform/tenants/"+o.tenant.String()+"/trial", platform, map[string]any{"days": 10, "plan_key": "completo"})
	expect(t, trial, http.StatusOK)
	if trial.body["status"] != "trialing" || trial.body["plan_key"] != "completo" {
		t.Fatalf("prueba: %v", trial.body)
	}
	if _, err := testDB.Pool().Exec(context.Background(), `UPDATE shared.subscriptions SET status = 'suspended'`); err != nil {
		t.Fatal(err)
	}
	re := o.do(http.MethodPost, "/api/v1/platform/subscriptions/"+trial.body["id"].(string)+"/reactivate", platform, map[string]any{"days": 3})
	expect(t, re, http.StatusOK)
	if re.body["status"] != "active" {
		t.Fatalf("reactivar: %v", re.body)
	}
	var actions []string
	rows, _ := testDB.Pool().Query(context.Background(), `SELECT action FROM shared.platform_audit_log WHERE action LIKE 'billing.%' OR action LIKE 'plan.%' ORDER BY created_at`)
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	for _, want := range []string{"plan.created", "billing.plan_changed", "billing.payment_succeeded", "billing.trial_started", "billing.reactivated"} {
		if !strings.Contains(strings.Join(actions, ","), want) {
			t.Errorf("falta %s en la bitácora de plataforma: %v", want, actions)
		}
	}
}

// Webhooks firmados e idempotentes.
func TestBillingWebhook(t *testing.T) {
	e := setup(t)
	o := e.ops()
	e.cycleAt(time.Now().AddDate(0, 0, 15))
	inv := items(o.do(http.MethodGet, "/api/v1/billing/invoices", o.admin, nil))[0]
	fake := e.svc.Billing.Provider().(*billing.FakeProvider)
	hook := func(body string, sig string) response {
		return e.raw(http.MethodPost, "/api/v1/public/billing/webhook/fake", map[string]string{"X-Argos-Signature": sig}, []byte(body))
	}
	failed := fmt.Sprintf(`{"id":"evt_1","type":"payment.failed","invoice_number":%q,"reason":"Fondos insuficientes."}`, inv["number"])
	expect(t, hook(failed, "sha256=00"), http.StatusUnauthorized)
	expect(t, hook(failed, fake.Sign([]byte(failed))), http.StatusNoContent)
	invoices := items(o.do(http.MethodGet, "/api/v1/billing/invoices", o.admin, nil))
	if invoices[0]["status"] != "open" || invoices[0]["last_error"] != "Fondos insuficientes." {
		t.Fatalf("cobro fallido: %v", invoices[0])
	}
	ok := fmt.Sprintf(`{"id":"evt_2","type":"payment.succeeded","invoice_number":%q,"payment_id":"pay_9","method":"Visa terminada en 1111"}`, inv["number"])
	expect(t, hook(ok, fake.Sign([]byte(ok))), http.StatusNoContent)
	expect(t, hook(ok, fake.Sign([]byte(ok))), http.StatusNoContent) // repetido: no hace nada
	var payments int
	if err := testDB.Pool().QueryRow(context.Background(), `SELECT count(*) FROM shared.payments WHERE status = 'succeeded'`).Scan(&payments); err != nil || payments != 1 {
		t.Fatalf("pagos = %d (%v)", payments, err)
	}
	s := sub(o.do(http.MethodGet, "/api/v1/billing/subscription", o.admin, nil))
	if s["payment_method"] != "Visa terminada en 1111" || s["open_invoice"] != nil || s["failed_payments"].(float64) != 1 {
		t.Fatalf("después del webhook: %v", s)
	}
	expect(t, hook(ok, fake.Sign([]byte(ok))+"x"), http.StatusUnauthorized)
	expect(t, e.raw(http.MethodPost, "/api/v1/public/billing/webhook/otra", nil, []byte(ok)), http.StatusNotFound)
}

// fase06.md §4.1: reservas desde el sitio web con clave API.
func TestPublicReservations(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.RateLimitAPIKey = 3 })
	o := e.ops()
	o.property("GAR-101", nil)
	today := todayGT(t)

	created := o.do(http.MethodPost, "/api/v1/api-keys", o.admin, map[string]any{"name": "Sitio web"})
	expect(t, created, http.StatusCreated)
	key := created.body["key"].(string)
	if !strings.HasPrefix(key, "argos_") || created.body["prefix"] != key[:14] {
		t.Fatalf("clave: %v", created.body)
	}
	if l := items(o.do(http.MethodGet, "/api/v1/api-keys", o.admin, nil)); len(l) != 1 || l[0]["key"] != nil {
		t.Fatalf("la lista no muestra el secreto: %v", l)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/api-keys", o.cleaner, nil), http.StatusForbidden)

	post := func(k string, body map[string]any) response {
		raw, _ := json.Marshal(body)
		return e.raw(http.MethodPost, "/api/v1/public/reservations", map[string]string{"X-Api-Key": k}, raw)
	}
	stay := map[string]any{
		"property_code": "gar-101", "check_in_date": addDays(today, 10), "check_out_date": addDays(today, 13), "adults": 2,
		"total_amount": 2100, "external_ref": "WEB-77", "guest": map[string]any{"full_name": "Elena Paz", "email": "elena@correo.com"},
	}
	res := post(key, stay)
	expect(t, res, http.StatusCreated)
	if res.body["status"] != "pending" || res.body["nights"].(float64) != 3 || res.body["property_code"] != "GAR-101" {
		t.Fatalf("reserva pública: %v", res.body)
	}
	detail := o.do(http.MethodGet, "/api/v1/reservations/"+res.body["id"].(string), o.admin, nil)
	if detail.body["source"] != "api" || detail.body["channel_kind"] != "direct" || detail.body["external_ref"] != "WEB-77" {
		t.Fatalf("detalle: %v %v %v", detail.body["source"], detail.body["channel_kind"], detail.body["external_ref"])
	}
	if queuedEmails(t, "reservation_requested") < 1 {
		t.Fatal("avisa por correo a quien administra reservas")
	}
	expect(t, post(key, stay), http.StatusUnprocessableEntity) // se cruza
	// Limite por clave (3 por minuto en esta prueba).
	limited := post(key, map[string]any{"property_code": "NO-EXISTE", "check_in_date": today, "check_out_date": addDays(today, 1), "guest": map[string]any{"full_name": "X Y"}})
	expect(t, limited, http.StatusNotFound)
	if res := post(key, stay); res.status != http.StatusTooManyRequests || res.header.Get("Retry-After") == "" {
		t.Fatalf("límite por clave: %d %v", res.status, res.header)
	}

	expect(t, post("argos_"+strings.Repeat("0", 48), stay), http.StatusUnauthorized)
	expect(t, post("cualquier-cosa", stay), http.StatusUnauthorized)
	second := o.do(http.MethodPost, "/api/v1/api-keys", o.admin, map[string]any{"name": "Agencia"})
	k2 := second.body["key"].(string)
	// Empresa en solo lectura: 402.
	if _, err := testDB.Pool().Exec(context.Background(), `UPDATE shared.subscriptions SET status = 'suspended'`); err != nil {
		t.Fatal(err)
	}
	expect(t, post(k2, stay), http.StatusPaymentRequired)
	if _, err := testDB.Pool().Exec(context.Background(), `UPDATE shared.subscriptions SET status = 'active'`); err != nil {
		t.Fatal(err)
	}
	// Sin el modulo de reservas: 403.
	if err := o.svc.Tenants.SetFeatures(context.Background(), o.tenant, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	expect(t, post(k2, stay), http.StatusForbidden)
	// Revocada: 401.
	revoked := o.do(http.MethodPost, "/api/v1/api-keys/"+second.body["id"].(string)+"/revoke", o.admin, nil)
	expect(t, revoked, http.StatusOK)
	expect(t, post(k2, stay), http.StatusUnauthorized)
	expect(t, o.do(http.MethodPost, "/api/v1/api-keys/"+second.body["id"].(string)+"/revoke", o.admin, nil), http.StatusUnprocessableEntity)
}

// Limite por IP en el acceso y /health/ready.
func TestRateLimitAndReadiness(t *testing.T) {
	e := setupWith(t, func(c *config.Config) { c.RateLimitAuth = 3 })
	for i := range 3 {
		expect(t, e.login(fmt.Sprintf("nadie%d@argos.gt", i), "x"), http.StatusUnauthorized)
	}
	res := e.login("otra@argos.gt", "x")
	if res.status != http.StatusTooManyRequests || res.header.Get("Retry-After") == "" {
		t.Fatalf("límite por IP: %d %v", res.status, res.body)
	}
	ready := e.do(http.MethodGet, "/api/v1/health/ready", "", nil)
	expect(t, ready, http.StatusOK)
	if ready.body["status"] != "ready" || ready.body["migrations"] != "up_to_date" || ready.body["storage"] != "memory" {
		t.Fatalf("ready: %v", ready.body)
	}
}

// --- Suite de aislamiento generada (fase06.md §5) ------------------------------

var pathParam = regexp.MustCompile(`\{([a-z_]+)\}`)

// fleet arma Garrent con un recurso de cada tipo y devuelve sus ids por
// nombre de parametro de ruta.
func fleet(o *opsEnv) map[string]string {
	o.t.Helper()
	o.payrollOn()
	ids := map[string]string{"tenant_id": o.tenant.String(), "user_id": o.ids["maria@garrent.gt"].String()}
	prop := o.property("GAR-1", nil)
	ids["property_id"] = prop
	b := o.do(http.MethodPost, "/api/v1/buildings", o.admin, map[string]any{"name": "Torre Norte"})
	expect(o.t, b, http.StatusCreated)
	ids["building_id"] = b.body["id"].(string)
	tpl := o.do(http.MethodPost, "/api/v1/checklist-templates", o.admin, map[string]any{
		"name": "Básico", "items": []map[string]any{{"section": "Sala", "text": "Barrer"}},
	})
	expect(o.t, tpl, http.StatusCreated)
	ids["template_id"] = tpl.body["id"].(string)
	ids["employee_id"] = o.employee("maria@garrent.gt", map[string]any{"pay_type": "per_day", "pay_rate": 250, "on_payroll": true})
	v := o.do(http.MethodPost, "/api/v1/vendors", o.admin, map[string]any{"name": "Frío Total"})
	expect(o.t, v, http.StatusCreated)
	ids["vendor_id"] = v.body["id"].(string)
	ids["asset_id"] = o.asset(map[string]any{"property_id": prop, "category": "hvac", "name": "A/C"})
	plan := o.do(http.MethodPost, "/api/v1/assets/"+ids["asset_id"]+"/plans", o.admin, map[string]any{"name": "Limpieza de filtros", "every_days": 30})
	if plan.status == http.StatusCreated {
		ids["plan_id"] = plan.body["id"].(string)
	}
	ids["ticket_id"] = o.ticket(o.admin, prop, "Fuga en la cocina", nil)
	today := todayGT(o.t)
	r := o.reserve(map[string]any{
		"property_id": prop, "channel_id": o.channel("direct"), "check_in_date": addDays(today, 5), "check_out_date": addDays(today, 7),
		"guest": map[string]any{"full_name": "Lucía Pérez", "email": "lucia@correo.com"},
	})
	ids["reservation_id"] = r["id"].(string)
	ids["guest_id"] = r["primary_guest_id"].(string)
	ids["channel_id"] = o.channel("direct")
	p := o.do(http.MethodPost, "/api/v1/payroll/periods", o.admin, map[string]any{"kind": "biweekly", "period_start": "2026-11-01", "period_end": "2026-11-15"})
	expect(o.t, p, http.StatusCreated)
	ids["period_id"] = p.body["id"].(string)
	expect(o.t, o.do(http.MethodPost, "/api/v1/payroll/periods/"+ids["period_id"]+"/compute", o.admin, nil), http.StatusOK)
	d := o.do(http.MethodGet, "/api/v1/payroll/periods/"+ids["period_id"], o.admin, nil)
	if entries, _ := d.body["entries"].([]any); len(entries) > 0 {
		ids["entry_id"] = entries[0].(map[string]any)["id"].(string)
	}
	a := o.do(http.MethodPost, "/api/v1/disciplinary", o.admin, map[string]any{
		"employee_id": ids["employee_id"], "type": "verbal", "occurred_on": "2026-11-20", "description": "Llegó tarde a la limpieza.",
	})
	expect(o.t, a, http.StatusCreated)
	ids["action_id"] = a.body["id"].(string)
	k := o.do(http.MethodPost, "/api/v1/api-keys", o.admin, map[string]any{"name": "Sitio"})
	ids["key_id"] = k.body["id"].(string)
	o.cycleAt(time.Now().AddDate(0, 0, 15))
	if inv := items(o.do(http.MethodGet, "/api/v1/billing/invoices", o.admin, nil)); len(inv) > 0 {
		ids["invoice_id"] = inv[0]["id"].(string)
	}
	s := sub(o.do(http.MethodGet, "/api/v1/billing/subscription", o.admin, nil))
	ids["subscription_id"] = s["id"].(string)
	ids["file_id"] = o.upload(o.admin, "ticket_photo", "image/jpeg", "foto.jpg")
	var member, role, session string
	if err := testDB.Pool().QueryRow(context.Background(), `
		SELECT m.id::text, m.role_id::text, (SELECT r.id::text FROM shared.refresh_tokens r WHERE r.user_id = m.user_id LIMIT 1)
		FROM shared.memberships m WHERE m.user_id = $1`, o.ids["maria@garrent.gt"]).Scan(&member, &role, &session); err != nil {
		o.t.Fatal(err)
	}
	ids["member_id"], ids["role_id"], ids["session_id"] = member, role, session
	return ids
}

type operation struct {
	method, path, id string
	secured          bool
}

func (e *env) operations() []operation {
	e.t.Helper()
	res, err := http.Get(e.srv.URL + "/api/v1/openapi.json")
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string                `json:"operationId"`
			Security    []map[string][]string `json:"security"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		e.t.Fatal(err)
	}
	var out []operation
	for path, methods := range doc.Paths {
		for method, op := range methods {
			secured := false
			for _, s := range op.Security {
				if _, ok := s["bearerAuth"]; ok {
					secured = true
				}
			}
			out = append(out, operation{method: strings.ToUpper(method), path: path, id: op.OperationID, secured: secured})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path+out[i].method < out[j].path+out[j].method })
	return out
}

// TestIsolationGenerated recorre TODOS los endpoints del contrato OpenAPI:
//   - sin token, todo endpoint protegido responde 401;
//   - con la sesion de la administradora de otra empresa (todos los permisos
//     en la suya), todo endpoint con id en la ruta, apuntando a recursos de
//     Garrent, responde 403/404 (o 400/422 por validacion), nunca 2xx ni 5xx.
//
// Un endpoint nuevo entra solo a la suite: el generador lee el contrato.
func TestIsolationGenerated(t *testing.T) {
	e := setup(t)
	o := e.ops()
	ids := fleet(o)
	other := e.tenant("costa_limpia", "Costa Limpia")
	if err := o.svc.Tenants.SetFeatures(context.Background(), other, map[string]bool{"reservations": true, "payroll": true}); err != nil {
		t.Fatal(err)
	}
	e.member("rosa@costa.gt", map[uuid.UUID]authz.RoleKey{other: authz.RoleAdmin})
	intruder, _ := e.session("rosa@costa.gt")

	ops := e.operations()
	if len(ops) < 150 {
		t.Fatalf("el contrato tiene %d operaciones: ¿se cargó el OpenAPI completo?", len(ops))
	}
	checked, unknown := 0, map[string]bool{}
	for _, op := range ops {
		if op.secured {
			res := e.do(op.method, strings.NewReplacer("{", "", "}", "").Replace(pathParam.ReplaceAllString(op.path, uuid.NewString())), "", map[string]any{})
			if res.status != http.StatusUnauthorized {
				t.Errorf("%s %s (%s) sin token: %d, se esperaba 401", op.method, op.path, op.id, res.status)
			}
		}
		if !op.secured || !pathParam.MatchString(op.path) {
			continue
		}
		path := pathParam.ReplaceAllStringFunc(op.path, func(m string) string {
			name := m[1 : len(m)-1]
			if v, ok := ids[name]; ok {
				return v
			}
			unknown[name] = true
			return uuid.NewString()
		})
		res := e.do(op.method, path, intruder, map[string]any{})
		checked++
		// /guest/* solo acepta el JWT del huesped: la sesion de un usuario es 401.
		if strings.HasPrefix(op.path, "/api/v1/guest/") && res.status == http.StatusUnauthorized {
			continue
		}
		switch res.status {
		case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
		default:
			t.Errorf("FUGA %s %s (%s) con recursos de Garrent: %d %v", op.method, path, op.id, res.status, res.body)
		}
	}
	t.Logf("aislamiento: %d operaciones, %d con recurso ajeno; parámetros sin recurso real: %v", len(ops), checked, keys(unknown))
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
