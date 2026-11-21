//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/app"
	"github.com/smorareqs/argos-service/internal/core/config"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
	"github.com/smorareqs/argos-service/internal/server"
	"github.com/smorareqs/argos-service/internal/testutil"
)

const (
	platformEmail = "admin@argos.io"
	password      = "clave-de-prueba"
)

var device = map[string]any{"id": "test-device", "name": "Go test", "platform": "web"}

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
		AccessTokenExpireMinutes: 15,
		RefreshTokenExpireDays:   60,
		CORSOrigins:              []string{"*"},
		DBMaxConns:               10,
	}
	svc, err := app.NewServices(cfg, testDB, testutil.Logger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Auth.CreateUser(ctx, auth.UserCreate{
		Email: platformEmail, Password: password, FullName: "Admin ARGOS", IsPlatformAdmin: true,
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(cfg, testutil.Logger, svc))
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, svc: svc}
}

// tenant crea una empresa (con sus siete roles) y devuelve su id.
func (e *env) tenant(slug, name string) uuid.UUID {
	e.t.Helper()
	t, err := e.svc.Tenants.CreateTenant(context.Background(), tenants.TenantCreate{
		Name: name, Slug: slug, Features: map[string]bool{"reservations": true},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return t.ID
}

// member crea una persona con membresia activa en cada empresa indicada.
func (e *env) member(email string, roles map[uuid.UUID]authz.RoleKey) uuid.UUID {
	e.t.Helper()
	ctx := context.Background()
	user, err := e.svc.Auth.CreateUser(ctx, auth.UserCreate{Email: email, Password: password, FullName: "Persona " + email})
	if err != nil {
		e.t.Fatal(err)
	}
	for tenantID, role := range roles {
		if err := testDB.InTx(ctx, func(q database.Querier) error {
			return authz.AddMember(ctx, q, user.ID, tenantID, role)
		}); err != nil {
			e.t.Fatal(err)
		}
	}
	return user.ID
}

func (e *env) do(method, path, token string, payload any) response {
	e.t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			e.t.Fatal(err)
		}
		body = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, e.srv.URL+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
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

func (e *env) login(email, pass string) response {
	e.t.Helper()
	return e.do(http.MethodPost, "/api/v1/auth/login", "", map[string]any{"email": email, "password": pass, "device": device})
}

// session inicia sesion de alguien con una sola empresa (o plataforma).
func (e *env) session(email string) (access, refresh string) {
	e.t.Helper()
	res := e.login(email, password)
	expect(e.t, res, http.StatusOK)
	if res.body["status"] != "authenticated" {
		e.t.Fatalf("se esperaba sesion abierta: %v", res.body)
	}
	return res.body["access_token"].(string), res.body["refresh_token"].(string)
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
	res := e.do(http.MethodGet, "/api/v1/health", "", nil)
	expect(t, res, http.StatusOK)
	want := map[string]any{"status": "ok", "environment": "testing", "database": "up"}
	for k, v := range want {
		if res.body[k] != v {
			t.Errorf("%s = %v, se esperaba %v", k, res.body[k], v)
		}
	}
}

func TestLoginSingleTenantAndMe(t *testing.T) {
	e := setup(t)
	garrent := e.tenant("garrent", "Garrent")
	e.member("ana@garrent.gt", map[uuid.UUID]authz.RoleKey{garrent: authz.RoleAdmin})

	res := e.login("ANA@garrent.gt", password)
	expect(t, res, http.StatusOK)
	if res.body["status"] != "authenticated" || res.body["expires_in"] != float64(900) {
		t.Fatalf("sesion inesperada: %v", res.body)
	}
	memberships := res.body["memberships"].([]any)
	if len(memberships) != 1 || memberships[0].(map[string]any)["role_key"] != "admin" {
		t.Errorf("membresias inesperadas: %v", memberships)
	}

	me := e.do(http.MethodGet, "/api/v1/auth/me", res.body["access_token"].(string), nil)
	expect(t, me, http.StatusOK)
	tenant := me.body["tenant"].(map[string]any)
	if tenant["name"] != "Garrent" || tenant["timezone"] != "America/Guatemala" || tenant["currency"] != "GTQ" {
		t.Errorf("tenant inesperado: %v", tenant)
	}
	features := tenant["features"].(map[string]any)
	if features["reservations"] != true || features["parking"] != false {
		t.Errorf("banderas inesperadas: %v", features)
	}
	if me.body["role"].(map[string]any)["key"] != "admin" {
		t.Errorf("rol inesperado: %v", me.body["role"])
	}
	perms := me.body["permissions"].([]any)
	if !slices.Contains(perms, any("users.manage")) || slices.Contains(perms, any("hr.disciplinary.own")) {
		t.Errorf("permisos del admin inesperados: %v", perms)
	}
}

func TestLoginErrors(t *testing.T) {
	e := setup(t)
	e.member("sin.empresa@garrent.gt", nil)

	expect(t, e.login(platformEmail, "incorrecta"), http.StatusUnauthorized)
	expect(t, e.login("nadie@garrent.gt", password), http.StatusUnauthorized)
	expect(t, e.login("sin.empresa@garrent.gt", password), http.StatusForbidden)
	bad := e.do(http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"email": platformEmail, "password": password, "device": map[string]any{"id": "x", "platform": "desktop"},
	})
	expect(t, bad, http.StatusUnprocessableEntity)
}

func TestLockoutAfterTenFailures(t *testing.T) {
	e := setup(t)
	for range 10 {
		expect(t, e.login(platformEmail, "incorrecta"), http.StatusUnauthorized)
	}
	res := e.login(platformEmail, password)
	expect(t, res, http.StatusTooManyRequests)
	if res.header.Get("Retry-After") == "" {
		t.Errorf("un 429 debe indicar Retry-After: %v", res.header)
	}
}

// fase01.md §3.10 caso 8.
func TestSelectTenantAndSwitch(t *testing.T) {
	e := setup(t)
	garrent := e.tenant("garrent", "Garrent")
	costa := e.tenant("costa_limpia", "Costa Limpia")
	otra := e.tenant("otra", "Otra")
	e.member("carlos@argos.gt", map[uuid.UUID]authz.RoleKey{garrent: authz.RoleSupervisor, costa: authz.RoleAdmin})

	res := e.login("carlos@argos.gt", password)
	expect(t, res, http.StatusOK)
	if res.body["status"] != "select_tenant" || res.body["access_token"] != nil || len(res.body["memberships"].([]any)) != 2 {
		t.Fatalf("se esperaba elegir empresa: %v", res.body)
	}
	selection := res.body["selection_token"].(string)

	expect(t, e.do(http.MethodPost, "/api/v1/auth/select-tenant", "", map[string]any{
		"tenant_id": otra, "selection_token": selection, "device": device,
	}), http.StatusNotFound)

	chosen := e.do(http.MethodPost, "/api/v1/auth/select-tenant", "", map[string]any{
		"tenant_id": garrent, "selection_token": selection, "device": device,
	})
	expect(t, chosen, http.StatusOK)
	me := e.do(http.MethodGet, "/api/v1/auth/me", chosen.body["access_token"].(string), nil)
	if me.body["tenant"].(map[string]any)["name"] != "Garrent" || me.body["role"].(map[string]any)["key"] != "supervisor" {
		t.Fatalf("primera empresa inesperada: %v", me.body)
	}

	switched := e.do(http.MethodPost, "/api/v1/auth/select-tenant", "", map[string]any{
		"tenant_id": costa, "refresh_token": chosen.body["refresh_token"],
	})
	expect(t, switched, http.StatusOK)
	me = e.do(http.MethodGet, "/api/v1/auth/me", switched.body["access_token"].(string), nil)
	if me.body["tenant"].(map[string]any)["name"] != "Costa Limpia" || me.body["role"].(map[string]any)["key"] != "admin" {
		t.Errorf("segunda empresa inesperada: %v", me.body)
	}
	// El refresh anterior quedo rotado.
	expect(t, e.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{
		"refresh_token": chosen.body["refresh_token"], "device_id": "test-device",
	}), http.StatusUnauthorized)
}

// fase01.md §3.10 caso 4.
func TestRefreshRotationAndReuse(t *testing.T) {
	e := setup(t)
	garrent := e.tenant("garrent", "Garrent")
	e.member("ana@garrent.gt", map[uuid.UUID]authz.RoleKey{garrent: authz.RoleAdmin})
	_, first := e.session("ana@garrent.gt")

	rotated := e.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{"refresh_token": first, "device_id": "test-device"})
	expect(t, rotated, http.StatusOK)
	second := rotated.body["refresh_token"].(string)
	if second == first {
		t.Fatal("el refresh debe rotar")
	}
	expect(t, e.do(http.MethodGet, "/api/v1/auth/me", rotated.body["access_token"].(string), nil), http.StatusOK)

	expect(t, e.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{"refresh_token": second, "device_id": "otro"}), http.StatusUnauthorized)
	// Reutilizar el primero revoca toda la sesion, tambien el segundo.
	expect(t, e.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{"refresh_token": first, "device_id": "test-device"}), http.StatusUnauthorized)
	expect(t, e.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{"refresh_token": second, "device_id": "test-device"}), http.StatusUnauthorized)
}

func TestLogout(t *testing.T) {
	e := setup(t)
	_, refresh := e.session(platformEmail)
	expect(t, e.do(http.MethodPost, "/api/v1/auth/logout", "", map[string]any{"refresh_token": refresh}), http.StatusNoContent)
	expect(t, e.do(http.MethodPost, "/api/v1/auth/logout", "", map[string]any{"refresh_token": refresh}), http.StatusNoContent)
	expect(t, e.do(http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{"refresh_token": refresh, "device_id": "test-device"}), http.StatusUnauthorized)
}

func TestForgotPasswordAlwaysAccepted(t *testing.T) {
	e := setup(t)
	expect(t, e.do(http.MethodPost, "/api/v1/auth/password/forgot", "", map[string]any{"email": "nadie@garrent.gt"}), http.StatusAccepted)
}

// fase01.md §3.10 casos 3 y 9.
func TestAccessTokenStopsWorking(t *testing.T) {
	ctx := context.Background()
	cases := map[string]string{
		"membresia suspendida": `UPDATE shared.memberships SET status = 'suspended'`,
		"tenant inactivo":      `UPDATE shared.tenants SET is_active = false`,
		"usuario inactivo":     `UPDATE shared.users SET is_active = false WHERE NOT is_platform_admin`,
		"permisos del rol":     `UPDATE shared.roles SET permissions_version = permissions_version + 1`,
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			garrent := e.tenant("garrent", "Garrent")
			e.member("ana@garrent.gt", map[uuid.UUID]authz.RoleKey{garrent: authz.RoleAdmin})
			access, _ := e.session("ana@garrent.gt")
			expect(t, e.do(http.MethodGet, "/api/v1/auth/me", access, nil), http.StatusOK)
			if _, err := testDB.Pool().Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
			expect(t, e.do(http.MethodGet, "/api/v1/auth/me", access, nil), http.StatusUnauthorized)
		})
	}
}

func TestMeRequiresValidAccessToken(t *testing.T) {
	e := setup(t)
	expect(t, e.do(http.MethodGet, "/api/v1/auth/me", "", nil), http.StatusUnauthorized)
	expect(t, e.do(http.MethodGet, "/api/v1/auth/me", "no-es-jwt", nil), http.StatusUnauthorized)
	_, refresh := e.session(platformEmail)
	// Un refresh token (opaco) no sirve como access token.
	res := e.do(http.MethodGet, "/api/v1/auth/me", refresh, nil)
	expect(t, res, http.StatusUnauthorized)
	if res.header.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("falta WWW-Authenticate: %v", res.header)
	}
}

func TestPlatformTenants(t *testing.T) {
	e := setup(t)
	admin, _ := e.session(platformEmail)

	me := e.do(http.MethodGet, "/api/v1/auth/me", admin, nil)
	expect(t, me, http.StatusOK)
	if me.body["tenant"] != nil || me.body["user"].(map[string]any)["is_platform_admin"] != true {
		t.Errorf("modo plataforma inesperado: %v", me.body)
	}

	expect(t, e.do(http.MethodGet, "/api/v1/platform/tenants", "", nil), http.StatusUnauthorized)
	empty := e.do(http.MethodGet, "/api/v1/platform/tenants", admin, nil)
	expect(t, empty, http.StatusOK)
	if empty.list == nil || len(empty.list) != 0 {
		t.Errorf("la lista vacia debe ser []: %v", empty.list)
	}

	created := e.do(http.MethodPost, "/api/v1/platform/tenants", admin, map[string]any{"name": "Garrent", "slug": " Garrent "})
	expect(t, created, http.StatusCreated)
	if created.body["slug"] != "garrent" || created.body["schema_name"] != "tenant_garrent" || created.body["is_active"] != true {
		t.Errorf("tenant inesperado: %v", created.body)
	}
	id := created.body["id"].(string)
	var roles int
	if err := testDB.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM shared.roles WHERE tenant_id = $1 AND is_system`, id).Scan(&roles); err != nil || roles != 7 {
		t.Errorf("el alta debe sembrar 7 roles: %d %v", roles, err)
	}

	expect(t, e.do(http.MethodGet, "/api/v1/platform/tenants/"+id, admin, nil), http.StatusOK)
	expect(t, e.do(http.MethodGet, "/api/v1/platform/tenants/"+uuid.NewString(), admin, nil), http.StatusNotFound)
	expect(t, e.do(http.MethodGet, "/api/v1/platform/tenants?limit=-1", admin, nil), http.StatusUnprocessableEntity)
	expect(t, e.do(http.MethodPost, "/api/v1/platform/tenants", admin, map[string]any{"name": "Otro", "slug": "garrent"}), http.StatusConflict)
	expect(t, e.do(http.MethodPost, "/api/v1/platform/tenants", admin, map[string]any{"name": "Malo", "slug": "mal-slug"}), http.StatusUnprocessableEntity)

	// El administrador de una empresa no administra la plataforma.
	e.member("jefe@garrent.gt", map[uuid.UUID]authz.RoleKey{uuid.MustParse(id): authz.RoleAdmin})
	tenantAdmin, _ := e.session("jefe@garrent.gt")
	expect(t, e.do(http.MethodGet, "/api/v1/platform/tenants", tenantAdmin, nil), http.StatusForbidden)
}

func TestOpenAPIAndDocs(t *testing.T) {
	e := setup(t)
	spec := e.do(http.MethodGet, "/api/v1/openapi.json", "", nil)
	if spec.status != http.StatusOK {
		t.Fatalf("openapi.json: %d", spec.status)
	}
	paths, _ := spec.body["paths"].(map[string]any)
	for _, p := range []string{
		"/api/v1/health", "/api/v1/auth/login", "/api/v1/auth/select-tenant", "/api/v1/auth/refresh",
		"/api/v1/auth/logout", "/api/v1/auth/me", "/api/v1/auth/password/forgot",
		"/api/v1/platform/tenants", "/api/v1/platform/tenants/{tenant_id}",
	} {
		if _, ok := paths[p]; !ok {
			t.Errorf("falta %s en el OpenAPI", p)
		}
	}
	if docs := e.do(http.MethodGet, "/docs", "", nil); docs.status != http.StatusOK {
		t.Errorf("/docs: %d", docs.status)
	}
}

func TestCORSPreflight(t *testing.T) {
	e := setup(t)
	req, err := http.NewRequest(http.MethodOptions, e.srv.URL+"/api/v1/auth/login", nil)
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
