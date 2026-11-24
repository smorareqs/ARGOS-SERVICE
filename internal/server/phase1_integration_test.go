//go:build integration

package server_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/security"
	"github.com/smorareqs/argos-service/internal/core/storage"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Pruebas de la fase 1 completa: aislamiento entre empresas y roles
// (fase01.md §3.10) y los flujos de cuenta, equipo, roles, configuracion,
// archivos y plataforma.

// mailLink devuelve el token del ultimo correo encolado para ese destino con
// esa plantilla (el worker no corre en las pruebas: el trabajo queda en la cola).
func (e *env) mailLink(to, template string) string {
	e.t.Helper()
	var link string
	err := testDB.Pool().QueryRow(context.Background(), `
		SELECT payload->'data'->>'Link' FROM shared.jobs
		WHERE type = 'send_email' AND payload->>'to' = $1 AND payload->>'template' = $2
		ORDER BY created_at DESC LIMIT 1`, to, template).Scan(&link)
	if err != nil {
		e.t.Fatalf("no hay correo %s para %s: %v", template, to, err)
	}
	u, err := url.Parse(link)
	if err != nil {
		e.t.Fatal(err)
	}
	return u.Query().Get("token")
}

func (e *env) tenantPath(id uuid.UUID, rest string) string {
	return "/api/v1/tenants/" + id.String() + rest
}

func field(res response) string {
	errs, _ := res.body["errors"].([]any)
	if len(errs) == 0 {
		return ""
	}
	first, _ := errs[0].(map[string]any)
	loc, _ := first["location"].(string)
	return loc
}

// fase01.md §3.10 caso 1: un token de una empresa no ve ni toca otra (404).
func TestTenantIsolationOnAdminEndpoints(t *testing.T) {
	e := setup(t)
	a, b := e.tenant("garrent", "Garrent"), e.tenant("costa", "Costa Limpia")
	e.member("ana@garrent.gt", map[uuid.UUID]authz.RoleKey{a: authz.RoleAdmin})
	e.member("beto@costa.gt", map[uuid.UUID]authz.RoleKey{b: authz.RoleAdmin})
	access, _ := e.session("ana@garrent.gt")

	own := e.do(http.MethodGet, e.tenantPath(a, "/members"), access, nil)
	expect(t, own, http.StatusOK)
	if items := own.body["items"].([]any); len(items) != 1 {
		t.Errorf("solo debe ver a su gente: %v", items)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/members"},
		{http.MethodGet, "/invitations"},
		{http.MethodGet, "/roles"},
		{http.MethodGet, "/settings"},
		{http.MethodPatch, "/settings"},
		{http.MethodPost, "/invitations"},
		{http.MethodPost, "/roles"},
	} {
		body := map[string]any{"email": "x@y.gt", "role_key": "cleaner", "name": "Otro", "permissions": []string{"tasks.read"}}
		if c.method == http.MethodGet {
			body = nil
		}
		expect(t, e.do(c.method, e.tenantPath(b, c.path), access, body), http.StatusNotFound)
	}
	// Una membresia de otra empresa tampoco se alcanza por id.
	var foreign uuid.UUID
	if err := testDB.Pool().QueryRow(context.Background(),
		`SELECT id FROM shared.memberships WHERE tenant_id = $1`, b).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	expect(t, e.do(http.MethodPost, e.tenantPath(a, "/members/"+foreign.String()+"/suspend"), access, map[string]any{}), http.StatusNotFound)
}

// fase01.md §3.10 caso 2: un cleaner recibe 403 en lo que no le corresponde.
func TestCleanerIsForbidden(t *testing.T) {
	e := setup(t)
	a := e.tenant("garrent", "Garrent")
	e.member("maria@garrent.gt", map[uuid.UUID]authz.RoleKey{a: authz.RoleCleaner})
	access, _ := e.session("maria@garrent.gt")
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, e.tenantPath(a, "/members"), nil},
		{http.MethodGet, e.tenantPath(a, "/roles"), nil},
		{http.MethodGet, e.tenantPath(a, "/settings"), nil},
		{http.MethodPost, e.tenantPath(a, "/invitations"), map[string]any{"email": "x@y.gt", "role_key": "cleaner"}},
		{http.MethodPost, e.tenantPath(a, "/roles"), map[string]any{"name": "Yo", "permissions": []string{"tasks.read"}}},
		{http.MethodGet, "/api/v1/platform/tenants", nil},
		{http.MethodGet, "/api/v1/platform/audit", nil},
		{http.MethodPost, "/api/v1/files/presign-upload", map[string]any{
			"module": "tenant", "kind": "logo", "content_type": "image/png", "size": 1000,
		}},
	} {
		expect(t, e.do(c.method, c.path, access, c.body), http.StatusForbidden)
	}
	// Su propia cuenta si la administra.
	expect(t, e.do(http.MethodGet, "/api/v1/auth/sessions", access, nil), http.StatusOK)
}

func TestInvitationFlow(t *testing.T) {
	e := setup(t)
	a, b := e.tenant("garrent", "Garrent"), e.tenant("costa", "Costa Limpia")
	e.member("ana@garrent.gt", map[uuid.UUID]authz.RoleKey{a: authz.RoleAdmin})
	e.member("beto@costa.gt", map[uuid.UUID]authz.RoleKey{b: authz.RoleAdmin})
	admin, _ := e.session("ana@garrent.gt")

	created := e.do(http.MethodPost, e.tenantPath(a, "/invitations"), admin, map[string]any{
		"email": "andrea@garrent.gt", "role_key": "cleaner", "message": "Bienvenida",
	})
	expect(t, created, http.StatusCreated)
	if created.body["status"] != "pending" || created.body["role_name"] != "Colaborador de limpieza" ||
		created.body["invited_by"] != "Persona ana@garrent.gt" || created.body["message"] != "Bienvenida" {
		t.Errorf("invitacion inesperada: %v", created.body)
	}
	dup := e.do(http.MethodPost, e.tenantPath(a, "/invitations"), admin, map[string]any{"email": "andrea@garrent.gt", "role_key": "cleaner"})
	expect(t, dup, http.StatusUnprocessableEntity)
	if field(dup) != "body.email" {
		t.Errorf("el 422 debe senalar el correo: %v", dup.body)
	}
	member := e.do(http.MethodPost, e.tenantPath(a, "/invitations"), admin, map[string]any{"email": "ana@garrent.gt", "role_key": "cleaner"})
	expect(t, member, http.StatusUnprocessableEntity)
	expect(t, e.do(http.MethodPost, e.tenantPath(a, "/invitations"), admin, map[string]any{"email": "z@garrent.gt", "role_key": "guest"}), http.StatusUnprocessableEntity)

	pending := e.do(http.MethodGet, e.tenantPath(a, "/invitations?status=pending"), admin, nil)
	expect(t, pending, http.StatusOK)
	if n := len(pending.body["items"].([]any)); n != 1 {
		t.Errorf("pendientes = %d", n)
	}

	token := e.mailLink("andrea@garrent.gt", "invitation")
	preview := e.do(http.MethodGet, "/api/v1/auth/invitations/"+token, "", nil)
	expect(t, preview, http.StatusOK)
	if preview.body["tenant_name"] != "Garrent" || preview.body["user_exists"] != false {
		t.Errorf("vista previa: %v", preview.body)
	}
	expect(t, e.do(http.MethodPost, "/api/v1/auth/invitations/accept", "", map[string]any{
		"token": token, "password": "corta", "full_name": "Andrea Soto", "device": device,
	}), http.StatusUnprocessableEntity)
	accepted := e.do(http.MethodPost, "/api/v1/auth/invitations/accept", "", map[string]any{
		"token": token, "password": "clave-nueva-123", "full_name": "Andrea Soto", "device": device,
	})
	expect(t, accepted, http.StatusOK)
	if accepted.body["status"] != "authenticated" {
		t.Fatalf("debe abrir sesion: %v", accepted.body)
	}
	me := e.do(http.MethodGet, "/api/v1/auth/me", accepted.body["access_token"].(string), nil)
	expect(t, me, http.StatusOK)
	if me.body["role"].(map[string]any)["key"] != "cleaner" {
		t.Errorf("rol: %v", me.body["role"])
	}
	// El enlace sirve una sola vez.
	expect(t, e.do(http.MethodGet, "/api/v1/auth/invitations/"+token, "", nil), http.StatusNotFound)
	expect(t, e.do(http.MethodPost, "/api/v1/auth/invitations/accept", "", map[string]any{
		"token": token, "password": "clave-nueva-123", "full_name": "Andrea", "device": device,
	}), http.StatusNotFound)

	// Alguien con cuenta en otra empresa entra con su contrasena actual.
	expect(t, e.do(http.MethodPost, e.tenantPath(a, "/invitations"), admin, map[string]any{"email": "beto@costa.gt", "role_key": "supervisor"}), http.StatusCreated)
	second := e.mailLink("beto@costa.gt", "invitation")
	if e.do(http.MethodGet, "/api/v1/auth/invitations/"+second, "", nil).body["user_exists"] != true {
		t.Error("beto ya tiene cuenta")
	}
	wrong := e.do(http.MethodPost, "/api/v1/auth/invitations/accept", "", map[string]any{"token": second, "password": "no-es", "device": device})
	expect(t, wrong, http.StatusUnprocessableEntity)
	ok := e.do(http.MethodPost, "/api/v1/auth/invitations/accept", "", map[string]any{"token": second, "password": password, "device": device})
	expect(t, ok, http.StatusOK)
	if n := len(ok.body["memberships"].([]any)); n != 2 {
		t.Errorf("beto debe tener dos empresas: %d", n)
	}
}

func TestInvitationResendAndRevoke(t *testing.T) {
	e := setup(t)
	a := e.tenant("garrent", "Garrent")
	e.member("ana@garrent.gt", map[uuid.UUID]authz.RoleKey{a: authz.RoleAdmin})
	admin, _ := e.session("ana@garrent.gt")
	inv := e.do(http.MethodPost, e.tenantPath(a, "/invitations"), admin, map[string]any{"email": "luis@garrent.gt", "role_key": "maintenance"})
	expect(t, inv, http.StatusCreated)
	id := inv.body["id"].(string)
	first := e.mailLink("luis@garrent.gt", "invitation")

	expect(t, e.do(http.MethodPost, e.tenantPath(a, "/invitations/"+id+"/resend"), admin, map[string]any{}), http.StatusOK)
	second := e.mailLink("luis@garrent.gt", "invitation")
	if first == second {
		t.Fatal("el reenvio debe generar un enlace nuevo")
	}
	expect(t, e.do(http.MethodGet, "/api/v1/auth/invitations/"+first, "", nil), http.StatusNotFound)
	expect(t, e.do(http.MethodGet, "/api/v1/auth/invitations/"+second, "", nil), http.StatusOK)

	expect(t, e.do(http.MethodDelete, e.tenantPath(a, "/invitations/"+id), admin, nil), http.StatusNoContent)
	expect(t, e.do(http.MethodGet, "/api/v1/auth/invitations/"+second, "", nil), http.StatusNotFound)
}

func TestPasswordResetAndChange(t *testing.T) {
	e := setup(t)
	a := e.tenant("garrent", "Garrent")
	e.member("ana@garrent.gt", map[uuid.UUID]authz.RoleKey{a: authz.RoleAdmin})
	_, oldRefresh := e.session("ana@garrent.gt")

	expect(t, e.do(http.MethodPost, "/api/v1/auth/password/forgot", "", map[string]any{"email": "ana@garrent.gt"}), http.StatusAccepted)
	token := e.mailLink("ana@garrent.gt", "password_reset")
	weak := e.do(http.MethodPost, "/api/v1/auth/password/reset", "", map[string]any{"token": token, "new_password": "corta"})
	expect(t, weak, http.StatusUnprocessableEntity)
	if field(weak) != "body.new_password" {
		t.Errorf("campo: %v", weak.body)
	}
	expect(t, e.do(http.MethodPost, "/api/v1/auth/password/reset", "", map[string]any{"token": token, "new_password": "nueva-clave-1"}), http.StatusNoContent)
	expect(t, e.do(http.MethodPost, "/api/v1/auth/password/reset", "", map[string]any{"token": token, "new_password": "otra-clave-22"}), http.StatusNotFound)
	// Restablecer cierra las sesiones abiertas.
	expect(t, e.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{"refresh_token": oldRefresh, "device_id": "test-device"}), http.StatusUnauthorized)
	expect(t, e.login("ana@garrent.gt", password), http.StatusUnauthorized)
	current := e.login("ana@garrent.gt", "nueva-clave-1")
	expect(t, current, http.StatusOK)
	access := current.body["access_token"].(string)

	other := e.login("ana@garrent.gt", "nueva-clave-1")
	bad := e.do(http.MethodPost, "/api/v1/auth/password/change", access, map[string]any{"current_password": "mal", "new_password": "tercera-clave"})
	expect(t, bad, http.StatusUnprocessableEntity)
	if field(bad) != "body.current_password" {
		t.Errorf("campo: %v", bad.body)
	}
	expect(t, e.do(http.MethodPost, "/api/v1/auth/password/change", access,
		map[string]any{"current_password": "nueva-clave-1", "new_password": "tercera-clave"}), http.StatusNoContent)
	// La sesion actual sigue; la otra se cerro.
	expect(t, e.do(http.MethodGet, "/api/v1/auth/me", access, nil), http.StatusOK)
	expect(t, e.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{
		"refresh_token": other.body["refresh_token"], "device_id": "test-device",
	}), http.StatusUnauthorized)
	e.mailLink("ana@garrent.gt", "password_changed")
}

func TestTOTPEnrollmentAndLogin(t *testing.T) {
	e := setup(t)
	a := e.tenant("garrent", "Garrent")
	e.member("ana@garrent.gt", map[uuid.UUID]authz.RoleKey{a: authz.RoleAdmin})
	access, _ := e.session("ana@garrent.gt")

	expect(t, e.do(http.MethodPost, "/api/v1/auth/totp/confirm", access, map[string]any{"code": "123456"}), http.StatusUnprocessableEntity)
	setupRes := e.do(http.MethodPost, "/api/v1/auth/totp/setup", access, nil)
	expect(t, setupRes, http.StatusOK)
	secret := setupRes.body["secret"].(string)
	if !strings.HasPrefix(setupRes.body["otpauth_url"].(string), "otpauth://totp/ARGOS:") {
		t.Errorf("otpauth: %v", setupRes.body)
	}
	var stored string
	if err := testDB.Pool().QueryRow(context.Background(), `SELECT totp_secret FROM shared.users WHERE email = 'ana@garrent.gt'`).Scan(&stored); err != nil || stored == secret || !strings.HasPrefix(stored, "v1:") {
		t.Errorf("el secreto debe guardarse cifrado: %q %v", stored, err)
	}
	wrong := e.do(http.MethodPost, "/api/v1/auth/totp/confirm", access, map[string]any{"code": "000000"})
	expect(t, wrong, http.StatusUnprocessableEntity)
	code, _ := security.TOTPCode(secret, time.Now())
	expect(t, e.do(http.MethodPost, "/api/v1/auth/totp/confirm", access, map[string]any{"code": code}), http.StatusNoContent)

	expect(t, e.login("ana@garrent.gt", password), http.StatusPreconditionRequired)
	withCode := e.do(http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"email": "ana@garrent.gt", "password": password, "totp_code": "000000", "device": device,
	})
	expect(t, withCode, http.StatusUnauthorized)
	code, _ = security.TOTPCode(secret, time.Now())
	good := e.do(http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"email": "ana@garrent.gt", "password": password, "totp_code": code, "device": device,
	})
	expect(t, good, http.StatusOK)

	fresh := good.body["access_token"].(string)
	expect(t, e.do(http.MethodPost, "/api/v1/auth/totp/disable", fresh, map[string]any{"password": "mal", "code": code}), http.StatusUnprocessableEntity)
	expect(t, e.do(http.MethodPost, "/api/v1/auth/totp/disable", fresh, map[string]any{"password": password, "code": code}), http.StatusNoContent)
	expect(t, e.login("ana@garrent.gt", password), http.StatusOK)
}

func TestSessionsListAndRevoke(t *testing.T) {
	e := setup(t)
	a := e.tenant("garrent", "Garrent")
	e.member("ana@garrent.gt", map[uuid.UUID]authz.RoleKey{a: authz.RoleAdmin})
	access, _ := e.session("ana@garrent.gt")
	e.session("ana@garrent.gt")

	list := e.do(http.MethodGet, "/api/v1/auth/sessions", access, nil)
	expect(t, list, http.StatusOK)
	if len(list.list) != 2 {
		t.Fatalf("sesiones = %d", len(list.list))
	}
	var other string
	currents := 0
	for _, raw := range list.list {
		s := raw.(map[string]any)
		if s["current"] == true {
			currents++
		} else {
			other = s["id"].(string)
		}
		if s["tenant_name"] != "Garrent" {
			t.Errorf("empresa de la sesion: %v", s)
		}
	}
	if currents != 1 {
		t.Errorf("debe marcar una sola sesion actual: %v", list.list)
	}
	expect(t, e.do(http.MethodDelete, "/api/v1/auth/sessions/"+uuid.NewString(), access, nil), http.StatusNotFound)
	expect(t, e.do(http.MethodDelete, "/api/v1/auth/sessions/"+other, access, nil), http.StatusNoContent)
	if after := e.do(http.MethodGet, "/api/v1/auth/sessions", access, nil); len(after.list) != 1 {
		t.Errorf("tras cerrar quedan %d", len(after.list))
	}
	e.session("ana@garrent.gt")
	expect(t, e.do(http.MethodDelete, "/api/v1/auth/sessions", access, nil), http.StatusNoContent)
	if after := e.do(http.MethodGet, "/api/v1/auth/sessions", access, nil); len(after.list) != 1 {
		t.Errorf("solo debe quedar la actual: %d", len(after.list))
	}

	profile := e.do(http.MethodPatch, "/api/v1/auth/me", access, map[string]any{"full_name": "Ana López", "phone": "+502 5555 0101"})
	expect(t, profile, http.StatusOK)
	if profile.body["full_name"] != "Ana López" || profile.body["phone"] != "+502 5555 0101" {
		t.Errorf("perfil: %v", profile.body)
	}
}

func TestMembersAdministration(t *testing.T) {
	e := setup(t)
	a := e.tenant("garrent", "Garrent")
	e.member("ana@garrent.gt", map[uuid.UUID]authz.RoleKey{a: authz.RoleAdmin})
	e.member("maria@garrent.gt", map[uuid.UUID]authz.RoleKey{a: authz.RoleCleaner})
	admin, _ := e.session("ana@garrent.gt")
	maria, _ := e.session("maria@garrent.gt")

	members := e.do(http.MethodGet, e.tenantPath(a, "/members"), admin, nil).body["items"].([]any)
	ids := map[string]string{}
	for _, m := range members {
		row := m.(map[string]any)
		ids[row["email"].(string)] = row["id"].(string)
	}
	// Debe quedar al menos un administrador activo y nadie se suspende a si mismo.
	expect(t, e.do(http.MethodPatch, e.tenantPath(a, "/members/"+ids["ana@garrent.gt"]+"/role"), admin, map[string]any{"role_key": "supervisor"}), http.StatusUnprocessableEntity)
	expect(t, e.do(http.MethodPost, e.tenantPath(a, "/members/"+ids["ana@garrent.gt"]+"/suspend"), admin, map[string]any{}), http.StatusUnprocessableEntity)

	changed := e.do(http.MethodPatch, e.tenantPath(a, "/members/"+ids["maria@garrent.gt"]+"/role"), admin, map[string]any{"role_key": "supervisor"})
	expect(t, changed, http.StatusOK)
	if changed.body["role_key"] != "supervisor" {
		t.Errorf("rol: %v", changed.body)
	}
	suspended := e.do(http.MethodPost, e.tenantPath(a, "/members/"+ids["maria@garrent.gt"]+"/suspend"), admin, map[string]any{})
	expect(t, suspended, http.StatusOK)
	if suspended.body["status"] != "suspended" {
		t.Errorf("estado: %v", suspended.body)
	}
	expect(t, e.do(http.MethodGet, "/api/v1/auth/me", maria, nil), http.StatusUnauthorized)
	expect(t, e.login("maria@garrent.gt", password), http.StatusForbidden)
	expect(t, e.do(http.MethodPost, e.tenantPath(a, "/members/"+ids["maria@garrent.gt"]+"/reactivate"), admin, map[string]any{}), http.StatusOK)
	expect(t, e.login("maria@garrent.gt", password), http.StatusOK)
	expect(t, e.do(http.MethodDelete, e.tenantPath(a, "/members/"+ids["maria@garrent.gt"]+"/sessions"), admin, nil), http.StatusNoContent)

	var logged int
	if err := testDB.Pool().QueryRow(context.Background(), e.auditCount(), "member.suspended").Scan(&logged); err != nil || logged != 1 {
		t.Errorf("la suspension debe quedar en la bitacora: %d %v", logged, err)
	}
}

func (e *env) auditCount() string {
	return `SELECT count(*) FROM tenant_garrent.audit_log WHERE action = $1`
}

// fase01.md §3.10 caso 3 con roles personalizados: editar los permisos de un
// rol invalida los tokens de quienes lo tienen.
func TestCustomRoles(t *testing.T) {
	e := setup(t)
	a := e.tenant("garrent", "Garrent")
	e.member("ana@garrent.gt", map[uuid.UUID]authz.RoleKey{a: authz.RoleAdmin})
	e.member("jorge@garrent.gt", map[uuid.UUID]authz.RoleKey{a: authz.RoleSupervisor})
	admin, _ := e.session("ana@garrent.gt")

	roles := e.do(http.MethodGet, e.tenantPath(a, "/roles"), admin, nil)
	expect(t, roles, http.StatusOK)
	items := roles.body["items"].([]any)
	if len(items) != 6 {
		t.Errorf("seis roles de sistema (sin huesped): %d", len(items))
	}
	systemID := items[0].(map[string]any)["id"].(string)
	expect(t, e.do(http.MethodPatch, e.tenantPath(a, "/roles/"+systemID), admin, map[string]any{"name": "Jefe"}), http.StatusUnprocessableEntity)
	expect(t, e.do(http.MethodDelete, e.tenantPath(a, "/roles/"+systemID), admin, nil), http.StatusUnprocessableEntity)

	bad := e.do(http.MethodPost, e.tenantPath(a, "/roles"), admin, map[string]any{"name": "Raro", "permissions": []string{"no.existe"}})
	expect(t, bad, http.StatusUnprocessableEntity)
	created := e.do(http.MethodPost, e.tenantPath(a, "/roles"), admin, map[string]any{
		"name": "Supervisor de noche", "description": "Turno 22 a 6", "permissions": []string{"tasks.read", "users.read", "users.manage"},
	})
	expect(t, created, http.StatusCreated)
	if created.body["key"] != "supervisor_de_noche" || created.body["is_system"] != false {
		t.Errorf("rol: %v", created.body)
	}
	roleID := created.body["id"].(string)
	expect(t, e.do(http.MethodPost, e.tenantPath(a, "/roles"), admin, map[string]any{"name": "supervisor de noche", "permissions": []string{"tasks.read"}}), http.StatusUnprocessableEntity)

	members := e.do(http.MethodGet, e.tenantPath(a, "/members"), admin, nil).body["items"].([]any)
	var jorgeID string
	for _, m := range members {
		if row := m.(map[string]any); row["email"] == "jorge@garrent.gt" {
			jorgeID = row["id"].(string)
		}
	}
	expect(t, e.do(http.MethodPatch, e.tenantPath(a, "/members/"+jorgeID+"/role"), admin, map[string]any{"role_key": "supervisor_de_noche"}), http.StatusOK)
	jorge, _ := e.session("jorge@garrent.gt")
	expect(t, e.do(http.MethodGet, e.tenantPath(a, "/members"), jorge, nil), http.StatusOK)

	// Quien no tiene un permiso no puede darlo.
	escalate := e.do(http.MethodPost, e.tenantPath(a, "/invitations"), jorge, map[string]any{"email": "nuevo@garrent.gt", "role_key": "admin"})
	expect(t, escalate, http.StatusUnprocessableEntity)
	if field(escalate) != "body.role_key" {
		t.Errorf("campo: %v", escalate.body)
	}

	expect(t, e.do(http.MethodDelete, e.tenantPath(a, "/roles/"+roleID), admin, nil), http.StatusUnprocessableEntity)
	updated := e.do(http.MethodPatch, e.tenantPath(a, "/roles/"+roleID), admin, map[string]any{"permissions": []string{"tasks.read"}})
	expect(t, updated, http.StatusOK)
	expect(t, e.do(http.MethodGet, "/api/v1/auth/me", jorge, nil), http.StatusUnauthorized)

	expect(t, e.do(http.MethodPatch, e.tenantPath(a, "/members/"+jorgeID+"/role"), admin, map[string]any{"role_key": "supervisor"}), http.StatusOK)
	expect(t, e.do(http.MethodDelete, e.tenantPath(a, "/roles/"+roleID), admin, nil), http.StatusNoContent)
}

// fase01.md §3.10 caso 5 y flujo del logo.
func TestFilesAndLogo(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a, b := e.tenant("garrent", "Garrent"), e.tenant("costa", "Costa Limpia")
	e.member("ana@garrent.gt", map[uuid.UUID]authz.RoleKey{a: authz.RoleAdmin})
	admin, _ := e.session("ana@garrent.gt")

	expect(t, e.do(http.MethodPost, "/api/v1/files/presign-upload", admin, map[string]any{
		"module": "tenant", "kind": "logo", "content_type": "image/svg+xml", "size": 100,
	}), http.StatusUnprocessableEntity)
	expect(t, e.do(http.MethodPost, "/api/v1/files/presign-upload", admin, map[string]any{
		"module": "tenant", "kind": "logo", "content_type": "image/png", "size": 5 << 20,
	}), http.StatusUnprocessableEntity)
	presigned := e.do(http.MethodPost, "/api/v1/files/presign-upload", admin, map[string]any{
		"module": "tenant", "kind": "logo", "content_type": "image/png", "size": 1200, "filename": "logo.png",
	})
	expect(t, presigned, http.StatusOK)
	fileID := presigned.body["file_id"].(string)
	expect(t, e.do(http.MethodPost, "/api/v1/files/"+fileID+"/confirm", admin, nil), http.StatusUnprocessableEntity)

	var key string
	if err := testDB.Pool().QueryRow(ctx, `SELECT s3_key FROM tenant_garrent.files WHERE id = $1`, fileID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "tenants/"+a.String()+"/tenant/") {
		t.Errorf("la clave debe ir bajo el prefijo de la empresa: %s", key)
	}
	e.svc.Storage.Backend().(*storage.Memory).Put(key, storage.ObjectInfo{Size: 1200, ContentType: "image/png"})
	confirmed := e.do(http.MethodPost, "/api/v1/files/"+fileID+"/confirm", admin, nil)
	expect(t, confirmed, http.StatusOK)
	expect(t, e.do(http.MethodGet, "/api/v1/files/"+fileID+"/url", admin, nil), http.StatusOK)

	saved := e.do(http.MethodPatch, e.tenantPath(a, "/settings"), admin, map[string]any{"logo_file_id": fileID})
	expect(t, saved, http.StatusOK)
	if saved.body["logo_url"] == nil {
		t.Error("la configuracion debe devolver la URL del logo")
	}
	me := e.do(http.MethodGet, "/api/v1/auth/me", admin, nil)
	if me.body["tenant"].(map[string]any)["logo_url"] == nil {
		t.Error("/auth/me debe traer el logo")
	}

	// Una fila con la clave de otra empresa no se firma aunque exista.
	var foreign string
	if err := testDB.Pool().QueryRow(ctx, `
		INSERT INTO tenant_garrent.files (s3_key, module, kind, content_type, size_bytes, status)
		VALUES ($1, 'tenant', 'logo', 'image/png', 10, 'ready') RETURNING id`,
		"tenants/"+b.String()+"/tenant/2026/11/logo-x.png").Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	expect(t, e.do(http.MethodGet, "/api/v1/files/"+foreign+"/url", admin, nil), http.StatusNotFound)
	expect(t, e.do(http.MethodPatch, e.tenantPath(a, "/settings"), admin, map[string]any{"logo_file_id": foreign}), http.StatusUnprocessableEntity)
}

func TestTenantSettings(t *testing.T) {
	e := setup(t)
	a := e.tenant("garrent", "Garrent")
	e.member("ana@garrent.gt", map[uuid.UUID]authz.RoleKey{a: authz.RoleAdmin})
	admin, _ := e.session("ana@garrent.gt")

	current := e.do(http.MethodGet, e.tenantPath(a, "/settings"), admin, nil)
	expect(t, current, http.StatusOK)
	if current.body["checkin_time"] != "15:00" || current.body["checkout_time"] != "11:00" || current.body["currency"] != "GTQ" {
		t.Errorf("valores por defecto: %v", current.body)
	}
	bad := e.do(http.MethodPatch, e.tenantPath(a, "/settings"), admin, map[string]any{"checkin_time": "25:00"})
	expect(t, bad, http.StatusUnprocessableEntity)
	if field(bad) != "body.checkin_time" {
		t.Errorf("campo: %v", bad.body)
	}
	expect(t, e.do(http.MethodPatch, e.tenantPath(a, "/settings"), admin, map[string]any{"timezone": "Marte/Base"}), http.StatusUnprocessableEntity)
	saved := e.do(http.MethodPatch, e.tenantPath(a, "/settings"), admin, map[string]any{
		"name": "Garrent Rentals", "legal_name": "Garrent, S.A.", "nit": "1234567-8", "checkin_time": "14:00",
		"checkout_alarm_minutes": 45, "guest_instructions": "Llaves en recepción",
	})
	expect(t, saved, http.StatusOK)
	if saved.body["name"] != "Garrent Rentals" || saved.body["legal_name"] != "Garrent, S.A." ||
		saved.body["checkin_time"] != "14:00" || saved.body["checkout_alarm_minutes"] != float64(45) {
		t.Errorf("guardado: %v", saved.body)
	}
	if again := e.do(http.MethodGet, e.tenantPath(a, "/settings"), admin, nil); again.body["nit"] != "1234567-8" {
		t.Errorf("no persistio: %v", again.body)
	}
}

func TestPlatformAdministration(t *testing.T) {
	e := setup(t)
	a := e.tenant("garrent", "Garrent")
	e.member("ana@garrent.gt", map[uuid.UUID]authz.RoleKey{a: authz.RoleAdmin})
	platform, _ := e.session(platformEmail)
	ana, _ := e.session("ana@garrent.gt")

	// Con correo de alguien que ya tiene cuenta, queda como administradora.
	existing := e.do(http.MethodPost, "/api/v1/platform/tenants", platform, map[string]any{
		"name": "Lago Azul", "slug": "lago_azul", "admin_email": "ana@garrent.gt", "features": map[string]bool{"parking": true},
	})
	expect(t, existing, http.StatusCreated)
	if login := e.login("ana@garrent.gt", password); login.body["status"] != "select_tenant" {
		t.Errorf("ana debe elegir entre dos empresas: %v", login.body)
	}
	// Con un correo nuevo, se invita.
	invited := e.do(http.MethodPost, "/api/v1/platform/tenants", platform, map[string]any{
		"name": "Volcán", "slug": "volcan", "admin_email": "duena@volcan.gt",
	})
	expect(t, invited, http.StatusCreated)
	e.mailLink("duena@volcan.gt", "invitation")
	expect(t, e.do(http.MethodPost, "/api/v1/platform/tenants", platform, map[string]any{
		"name": "Malo", "slug": "malo", "admin_email": "no-es-correo",
	}), http.StatusUnprocessableEntity)

	list := e.do(http.MethodGet, "/api/v1/platform/tenants", platform, nil)
	expect(t, list, http.StatusOK)
	if len(list.list) != 3 {
		t.Errorf("empresas = %d", len(list.list))
	}

	suspended := e.do(http.MethodPatch, "/api/v1/platform/tenants/"+a.String(), platform, map[string]any{"is_active": false})
	expect(t, suspended, http.StatusOK)
	if suspended.body["is_active"] != false {
		t.Errorf("estado: %v", suspended.body)
	}
	expect(t, e.do(http.MethodGet, "/api/v1/auth/me", ana, nil), http.StatusUnauthorized)
	expect(t, e.do(http.MethodPatch, "/api/v1/platform/tenants/"+a.String(), platform, map[string]any{"is_active": true, "features": map[string]bool{"payroll": true}}), http.StatusOK)

	logs := e.do(http.MethodGet, "/api/v1/platform/audit", platform, nil)
	expect(t, logs, http.StatusOK)
	actions := map[string]int{}
	for _, raw := range logs.body["items"].([]any) {
		actions[raw.(map[string]any)["action"].(string)]++
	}
	if actions["tenant.created"] != 3 || actions["tenant.suspended"] != 1 || actions["tenant.reactivated"] != 1 {
		t.Errorf("bitacora de plataforma: %v", actions)
	}
	filtered := e.do(http.MethodGet, "/api/v1/platform/audit?tenant_id="+a.String(), platform, nil)
	if n := len(filtered.body["items"].([]any)); n != 3 {
		t.Errorf("filtro por empresa: %d", n)
	}
}

// Una persona administradora de ARGOS con empresas pasa al modo plataforma
// sin cerrar sesion.
func TestSwitchToPlatformMode(t *testing.T) {
	e := setup(t)
	a := e.tenant("garrent", "Garrent")
	user := e.member("sebas@argos.gt", map[uuid.UUID]authz.RoleKey{a: authz.RoleAdmin})
	if err := e.svc.Auth.SetPlatformAdmin(context.Background(), user, true); err != nil {
		t.Fatal(err)
	}
	login := e.login("sebas@argos.gt", password)
	expect(t, login, http.StatusOK)
	if login.body["platform_admin"] != true {
		t.Errorf("la respuesta debe avisar que puede entrar a la plataforma: %v", login.body)
	}
	switched := e.do(http.MethodPost, "/api/v1/auth/select-tenant", "", map[string]any{
		"platform": true, "refresh_token": login.body["refresh_token"],
	})
	expect(t, switched, http.StatusOK)
	me := e.do(http.MethodGet, "/api/v1/auth/me", switched.body["access_token"].(string), nil)
	if me.body["tenant"] != nil {
		t.Errorf("modo plataforma sin empresa: %v", me.body)
	}
	back := e.do(http.MethodPost, "/api/v1/auth/select-tenant", "", map[string]any{
		"tenant_id": a.String(), "refresh_token": switched.body["refresh_token"],
	})
	expect(t, back, http.StatusOK)

	// Quien no es de ARGOS no entra a la plataforma.
	e.member("ana@garrent.gt", map[uuid.UUID]authz.RoleKey{a: authz.RoleAdmin})
	_, refresh := e.session("ana@garrent.gt")
	expect(t, e.do(http.MethodPost, "/api/v1/auth/select-tenant", "", map[string]any{"platform": true, "refresh_token": refresh}), http.StatusNotFound)
}
