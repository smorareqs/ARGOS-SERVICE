//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/app"
	"github.com/smorareqs/argos-service/internal/core/config"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/server"
	"github.com/smorareqs/argos-service/internal/testutil"
)

const (
	adminEmail    = "admin@argos.io"
	adminPassword = "admin-password"
)

var testDB *database.DB

func TestMain(m *testing.M) {
	db, cleanup, err := testutil.StartDB(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testDB = db
	code := m.Run()
	cleanup()
	os.Exit(code)
}

type env struct {
	t   *testing.T
	srv *httptest.Server
	svc app.Services
}

type response struct {
	status int
	header http.Header
	body   map[string]any
	list   []any
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	if err := testutil.Reset(ctx, testDB); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Environment:              config.Testing,
		SecretKey:                "test-secret",
		JWTAlgorithm:             "HS256",
		AccessTokenExpireMinutes: 30,
		RefreshTokenExpireDays:   7,
		CORSOrigins:              []string{"*"},
		DBMaxConns:               10,
	}
	svc, err := app.NewServices(cfg, testDB, testutil.Logger)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Auth.Register(ctx, auth.UserCreate{
		Email: adminEmail, Password: adminPassword, FullName: "Admin ARGOS", Role: auth.RolePlatformAdmin,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(cfg, testutil.Logger, svc))
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, svc: svc}
}

func (e *env) do(method, path, token, contentType string, body io.Reader) response {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	out := response{status: res.StatusCode, header: res.Header}
	if len(raw) > 0 && strings.Contains(res.Header.Get("Content-Type"), "json") {
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			e.t.Fatalf("respuesta no JSON (%d): %s", res.StatusCode, raw)
		}
		out.body, _ = decoded.(map[string]any)
		out.list, _ = decoded.([]any)
	}
	return out
}

func (e *env) json(method, path, token string, payload any) response {
	e.t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			e.t.Fatal(err)
		}
		body = strings.NewReader(string(raw))
	}
	return e.do(method, path, token, "application/json", body)
}

func (e *env) login(email, password string) response {
	e.t.Helper()
	form := url.Values{"username": {email}, "password": {password}, "grant_type": {"password"}}
	return e.do(http.MethodPost, "/api/v1/auth/login", "", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
}

func (e *env) token(email, password string) string {
	e.t.Helper()
	res := e.login(email, password)
	if res.status != http.StatusOK {
		e.t.Fatalf("login %s: %d %v", email, res.status, res.body)
	}
	return res.body["access_token"].(string)
}

func expect(t *testing.T, res response, status int) {
	t.Helper()
	if res.status != status {
		t.Fatalf("status %d, se esperaba %d: %v", res.status, status, res.body)
	}
	if _, ok := res.body["$schema"]; ok {
		t.Errorf("la respuesta no debe incluir $schema: %v", res.body)
	}
}

func TestHealth(t *testing.T) {
	e := setup(t)
	res := e.do(http.MethodGet, "/api/v1/health", "", "", nil)
	expect(t, res, http.StatusOK)
	want := map[string]any{"status": "ok", "environment": "testing", "database": "up"}
	for k, v := range want {
		if res.body[k] != v {
			t.Errorf("%s = %v, se esperaba %v", k, res.body[k], v)
		}
	}
}

func TestLoginAndMe(t *testing.T) {
	e := setup(t)
	res := e.login(adminEmail, adminPassword)
	expect(t, res, http.StatusOK)
	if res.body["token_type"] != "bearer" || res.body["access_token"] == "" || res.body["refresh_token"] == "" {
		t.Fatalf("respuesta de login inesperada: %v", res.body)
	}

	me := e.do(http.MethodGet, "/api/v1/auth/me", res.body["access_token"].(string), "", nil)
	expect(t, me, http.StatusOK)
	if me.body["email"] != adminEmail || me.body["role"] != "platform_admin" || me.body["tenant_id"] != nil {
		t.Errorf("perfil inesperado: %v", me.body)
	}
	if _, ok := me.body["tenant_id"]; !ok {
		t.Error("tenant_id debe estar presente aunque sea null")
	}
	if _, ok := me.body["hashed_password"]; ok {
		t.Error("nunca se expone el hash")
	}
}

func TestLoginErrors(t *testing.T) {
	e := setup(t)

	res := e.login(adminEmail, "incorrecta")
	expect(t, res, http.StatusUnauthorized)
	if res.body["detail"] != "Correo o contrasena incorrectos." {
		t.Errorf("detail = %v", res.body["detail"])
	}
	expect(t, e.login("nadie@argos.io", adminPassword), http.StatusUnauthorized)

	missing := url.Values{"username": {adminEmail}}
	res = e.do(http.MethodPost, "/api/v1/auth/login", "", "application/x-www-form-urlencoded", strings.NewReader(missing.Encode()))
	expect(t, res, http.StatusUnprocessableEntity)
}

func TestMeRequiresValidAccessToken(t *testing.T) {
	e := setup(t)

	res := e.do(http.MethodGet, "/api/v1/auth/me", "", "", nil)
	expect(t, res, http.StatusUnauthorized)
	if res.header.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("falta WWW-Authenticate: %v", res.header)
	}
	expect(t, e.do(http.MethodGet, "/api/v1/auth/me", "basura", "", nil), http.StatusUnauthorized)

	// Un refresh token no sirve como token de acceso.
	refresh := e.login(adminEmail, adminPassword).body["refresh_token"].(string)
	expect(t, e.do(http.MethodGet, "/api/v1/auth/me", refresh, "", nil), http.StatusUnauthorized)
}

func TestRegister(t *testing.T) {
	e := setup(t)
	admin := e.token(adminEmail, adminPassword)
	tenant := e.json(http.MethodPost, "/api/v1/tenants", admin, map[string]any{"name": "Acme", "slug": "acme"})
	expect(t, tenant, http.StatusCreated)
	tenantID := tenant.body["id"].(string)

	employee := map[string]any{
		"email": "ana@acme.gt", "password": "12345678", "full_name": "Ana", "tenant_id": tenantID,
	}

	// Registrar exige platform_admin.
	expect(t, e.json(http.MethodPost, "/api/v1/auth/register", "", employee), http.StatusUnauthorized)

	res := e.json(http.MethodPost, "/api/v1/auth/register", admin, employee)
	expect(t, res, http.StatusCreated)
	if res.body["role"] != "employee" || res.body["tenant_id"] != tenantID || res.body["is_owner"] != false || res.body["is_active"] != true {
		t.Errorf("usuario inesperado: %v", res.body)
	}

	// El empleado inicia sesion; su peticion se resuelve en el schema del tenant.
	employeeToken := e.token("ana@acme.gt", "12345678")
	me := e.do(http.MethodGet, "/api/v1/auth/me", employeeToken, "", nil)
	expect(t, me, http.StatusOK)
	if me.body["tenant_id"] != tenantID {
		t.Errorf("tenant del empleado = %v", me.body["tenant_id"])
	}
	expect(t, e.json(http.MethodPost, "/api/v1/auth/register", employeeToken, employee), http.StatusForbidden)

	expect(t, e.json(http.MethodPost, "/api/v1/auth/register", admin, employee), http.StatusConflict)

	unknownTenant := map[string]any{"email": "luis@acme.gt", "password": "12345678", "full_name": "Luis", "tenant_id": uuid.NewString()}
	expect(t, e.json(http.MethodPost, "/api/v1/auth/register", admin, unknownTenant), http.StatusUnprocessableEntity)

	for name, body := range map[string]map[string]any{
		"contrasena corta": {"email": "x@acme.gt", "password": "123", "full_name": "X"},
		"correo invalido":  {"email": "no-es-correo", "password": "12345678", "full_name": "X"},
		"rol invalido":     {"email": "x@acme.gt", "password": "12345678", "full_name": "X", "role": "root"},
		"campo extra":      {"email": "x@acme.gt", "password": "12345678", "full_name": "X", "tenantId": tenantID},
	} {
		t.Run(name, func(t *testing.T) {
			expect(t, e.json(http.MethodPost, "/api/v1/auth/register", admin, body), http.StatusUnprocessableEntity)
		})
	}
}

func TestTenantsEndpoints(t *testing.T) {
	e := setup(t)
	admin := e.token(adminEmail, adminPassword)

	expect(t, e.json(http.MethodGet, "/api/v1/tenants", "", nil), http.StatusUnauthorized)

	empty := e.json(http.MethodGet, "/api/v1/tenants", admin, nil)
	expect(t, empty, http.StatusOK)
	if empty.list == nil || len(empty.list) != 0 {
		t.Errorf("la lista vacia debe ser []: %v", empty.list)
	}

	created := e.json(http.MethodPost, "/api/v1/tenants", admin, map[string]any{"name": "Garrent", "slug": " Garrent "})
	expect(t, created, http.StatusCreated)
	if created.body["slug"] != "garrent" || created.body["schema_name"] != "tenant_garrent" || created.body["is_active"] != true {
		t.Errorf("tenant inesperado: %v", created.body)
	}
	id := created.body["id"].(string)

	list := e.json(http.MethodGet, "/api/v1/tenants?limit=10&offset=0", admin, nil)
	expect(t, list, http.StatusOK)
	if len(list.list) != 1 {
		t.Errorf("se esperaba 1 tenant: %v", list.list)
	}

	got := e.json(http.MethodGet, "/api/v1/tenants/"+id, admin, nil)
	expect(t, got, http.StatusOK)
	if got.body["id"] != id {
		t.Errorf("tenant obtenido = %v", got.body)
	}

	expect(t, e.json(http.MethodGet, "/api/v1/tenants/"+uuid.NewString(), admin, nil), http.StatusNotFound)
	expect(t, e.json(http.MethodGet, "/api/v1/tenants/no-es-uuid", admin, nil), http.StatusUnprocessableEntity)
	expect(t, e.json(http.MethodGet, "/api/v1/tenants?limit=-1", admin, nil), http.StatusUnprocessableEntity)
	expect(t, e.json(http.MethodPost, "/api/v1/tenants", admin, map[string]any{"name": "Otro", "slug": "garrent"}), http.StatusConflict)
	expect(t, e.json(http.MethodPost, "/api/v1/tenants", admin, map[string]any{"name": "Malo", "slug": "mal-slug"}), http.StatusUnprocessableEntity)

	// Un usuario de tenant no administra tenants.
	expect(t, e.json(http.MethodPost, "/api/v1/auth/register", admin, map[string]any{
		"email": "jefe@garrent.gt", "password": "12345678", "full_name": "Jefe", "role": "admin", "tenant_id": id,
	}), http.StatusCreated)
	tenantAdmin := e.token("jefe@garrent.gt", "12345678")
	expect(t, e.json(http.MethodGet, "/api/v1/tenants", tenantAdmin, nil), http.StatusForbidden)
	expect(t, e.json(http.MethodGet, "/api/v1/tenants/"+id, tenantAdmin, nil), http.StatusForbidden)
}

func TestOpenAPIAndDocs(t *testing.T) {
	e := setup(t)
	spec := e.do(http.MethodGet, "/api/v1/openapi.json", "", "", nil)
	if spec.status != http.StatusOK {
		t.Fatalf("openapi.json: %d", spec.status)
	}
	paths, _ := spec.body["paths"].(map[string]any)
	for _, p := range []string{"/api/v1/health", "/api/v1/auth/login", "/api/v1/auth/register", "/api/v1/auth/me", "/api/v1/tenants", "/api/v1/tenants/{tenant_id}"} {
		if _, ok := paths[p]; !ok {
			t.Errorf("falta %s en el OpenAPI", p)
		}
	}
	login, _ := paths["/api/v1/auth/login"].(map[string]any)["post"].(map[string]any)
	content, _ := login["requestBody"].(map[string]any)["content"].(map[string]any)
	if _, ok := content["application/x-www-form-urlencoded"]; !ok {
		t.Errorf("el login debe documentarse como formulario: %v", content)
	}

	if docs := e.do(http.MethodGet, "/docs", "", "", nil); docs.status != http.StatusOK {
		t.Errorf("/docs: %d", docs.status)
	}
}

func TestCORSPreflight(t *testing.T) {
	e := setup(t)
	req, err := http.NewRequest(http.MethodOptions, e.srv.URL+"/api/v1/tenants", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://localhost:4200")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("Access-Control-Allow-Origin") != "http://localhost:4200" {
		t.Errorf("CORS: %d %v", res.StatusCode, res.Header)
	}
}
