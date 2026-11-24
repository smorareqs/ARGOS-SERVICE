//go:build integration

package server_test

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Pruebas de la fase 2 de Persona A (fase02.md §7): alcance por rol, acceso
// cifrado, propiedades, edificios, plantillas, fichas laborales, bitacora y
// sync.

type opsEnv struct {
	*env
	tenant                                        uuid.UUID
	admin, cleaner, maint, owner, security, super string
	ids                                           map[string]uuid.UUID // personas por correo
}

// ops arma Garrent con una persona por rol y devuelve sus sesiones.
func (e *env) ops() *opsEnv {
	e.t.Helper()
	a := e.tenant("garrent", "Garrent")
	o := &opsEnv{env: e, tenant: a, ids: map[string]uuid.UUID{}}
	people := map[string]authz.RoleKey{
		"ana@garrent.gt": authz.RoleAdmin, "maria@garrent.gt": authz.RoleCleaner, "luis@garrent.gt": authz.RoleMaintenance,
		"ricardo@garrent.gt": authz.RoleOwner, "sofia@garrent.gt": authz.RoleSecurity, "jorge@garrent.gt": authz.RoleSupervisor,
	}
	for email, role := range people {
		o.ids[email] = e.member(email, map[uuid.UUID]authz.RoleKey{a: role})
	}
	o.admin, _ = e.session("ana@garrent.gt")
	o.cleaner, _ = e.session("maria@garrent.gt")
	o.maint, _ = e.session("luis@garrent.gt")
	o.owner, _ = e.session("ricardo@garrent.gt")
	o.security, _ = e.session("sofia@garrent.gt")
	o.super, _ = e.session("jorge@garrent.gt")
	return o
}

// property crea una propiedad como admin y devuelve su id.
func (o *opsEnv) property(code string, extra map[string]any) string {
	o.t.Helper()
	body := map[string]any{"code": code, "name": "Propiedad " + code, "type": "apartment"}
	for k, v := range extra {
		body[k] = v
	}
	res := o.do(http.MethodPost, "/api/v1/properties", o.admin, body)
	expect(o.t, res, http.StatusCreated)
	return res.body["id"].(string)
}

func items(res response) []map[string]any {
	raw, _ := res.body["items"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		out = append(out, it.(map[string]any))
	}
	return out
}

func codes(res response) []string {
	out := []string{}
	for _, it := range items(res) {
		out = append(out, it["code"].(string))
	}
	return out
}

// fase02.md §7: limpieza ve solo lo asignado (404 por id), el propietario solo
// lo suyo, y seguridad y supervision ven todo. Otra empresa no ve nada.
func TestPropertyScopeByRole(t *testing.T) {
	e := setup(t)
	o := e.ops()
	building := o.do(http.MethodPost, "/api/v1/buildings", o.admin, map[string]any{"name": "Torre Botánica", "zone": "Zona 10"})
	expect(t, building, http.StatusCreated)
	p1 := o.property("TB-504", map[string]any{"building_id": building.body["id"]})
	p2 := o.property("CY-12A", nil)
	o.property("AN-SEB", nil)
	expect(t, o.do(http.MethodPut, "/api/v1/properties/"+p1+"/assignments", o.admin,
		map[string]any{"user_ids": []string{o.ids["maria@garrent.gt"].String()}}), http.StatusOK)
	expect(t, o.do(http.MethodPut, "/api/v1/properties/"+p2+"/owners", o.admin,
		map[string]any{"user_ids": []string{o.ids["ricardo@garrent.gt"].String()}}), http.StatusOK)

	cases := []struct {
		who, token string
		want       []string
	}{
		{"limpieza", o.cleaner, []string{"TB-504"}},
		{"mantenimiento sin asignaciones", o.maint, []string{}},
		{"propietario", o.owner, []string{"CY-12A"}},
		{"seguridad", o.security, []string{"AN-SEB", "CY-12A", "TB-504"}},
		{"supervision", o.super, []string{"AN-SEB", "CY-12A", "TB-504"}},
	}
	for _, c := range cases {
		res := o.do(http.MethodGet, "/api/v1/properties", c.token, nil)
		expect(t, res, http.StatusOK)
		if got := codes(res); !slices.Equal(got, c.want) {
			t.Errorf("%s ve %v, se esperaba %v", c.who, got, c.want)
		}
	}
	expect(t, o.do(http.MethodGet, "/api/v1/properties/"+p2, o.cleaner, nil), http.StatusNotFound)
	expect(t, o.do(http.MethodGet, "/api/v1/properties/"+p1, o.owner, nil), http.StatusNotFound)
	expect(t, o.do(http.MethodGet, "/api/v1/properties/"+p1, o.cleaner, nil), http.StatusOK)
	if b := items(o.do(http.MethodGet, "/api/v1/buildings", o.owner, nil)); len(b) != 0 {
		t.Errorf("el propietario no tiene propiedades en el edificio: %v", b)
	}
	if b := items(o.do(http.MethodGet, "/api/v1/buildings", o.cleaner, nil)); len(b) != 1 || b[0]["properties_count"] != float64(1) {
		t.Errorf("limpieza ve el edificio de su propiedad: %v", b)
	}

	// Otra empresa: lista vacia y 404 por id.
	b := e.tenant("costa", "Costa Limpia")
	e.member("beto@costa.gt", map[uuid.UUID]authz.RoleKey{b: authz.RoleAdmin})
	other, _ := e.session("beto@costa.gt")
	if got := codes(o.do(http.MethodGet, "/api/v1/properties", other, nil)); len(got) != 0 {
		t.Errorf("otra empresa ve %v", got)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/properties/"+p1, other, nil), http.StatusNotFound)
	expect(t, o.do(http.MethodPatch, "/api/v1/properties/"+p1, other, map[string]any{"name": "Robada"}), http.StatusNotFound)
	// Sin properties.manage no se crea ni se asigna.
	expect(t, o.do(http.MethodPost, "/api/v1/properties", o.super, map[string]any{"code": "X-1", "name": "X uno", "type": "room"}), http.StatusForbidden)
	expect(t, o.do(http.MethodPut, "/api/v1/properties/"+p1+"/owners", o.cleaner, map[string]any{"user_ids": []string{}}), http.StatusForbidden)
}

func TestAccessInfoIsEncryptedAndAudited(t *testing.T) {
	e := setup(t)
	o := e.ops()
	secret := "Caja de llaves · código 4821"
	p1 := o.property("TB-504", map[string]any{"access_info": secret})
	expect(t, o.do(http.MethodPut, "/api/v1/properties/"+p1+"/assignments", o.admin,
		map[string]any{"user_ids": []string{o.ids["maria@garrent.gt"].String()}}), http.StatusOK)

	var stored string
	if err := testDB.Pool().QueryRow(context.Background(), `SELECT access_info FROM tenant_garrent.properties WHERE id = $1`, p1).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "4821") || stored == secret {
		t.Fatalf("el acceso debe guardarse cifrado: %q", stored)
	}
	detail := o.do(http.MethodGet, "/api/v1/properties/"+p1, o.admin, nil)
	if _, leaked := detail.body["access_info"]; leaked || detail.body["has_access_info"] != true {
		t.Errorf("la ficha no trae el texto de acceso: %v", detail.body)
	}

	got := o.do(http.MethodGet, "/api/v1/properties/"+p1+"/access-info", o.admin, nil)
	expect(t, got, http.StatusOK)
	if got.body["text"] != secret {
		t.Errorf("texto: %v", got.body)
	}
	var viewed int
	if err := testDB.Pool().QueryRow(context.Background(), e.auditCount(), "property.access_viewed").Scan(&viewed); err != nil || viewed != 1 {
		t.Errorf("la consulta debe quedar en la bitacora: %d %v", viewed, err)
	}

	// Limpieza: sin tarea del dia (sin gate) no lo ve; con el gate de tareas, si.
	expect(t, o.do(http.MethodGet, "/api/v1/properties/"+p1+"/access-info", o.cleaner, nil), http.StatusForbidden)
	e.svc.Properties.SetAccessGate(func(context.Context, database.Querier, authz.Actor, uuid.UUID) (bool, error) {
		return true, nil
	})
	expect(t, o.do(http.MethodGet, "/api/v1/properties/"+p1+"/access-info", o.cleaner, nil), http.StatusOK)
	// Seguridad siempre; el propietario no tiene el permiso.
	expect(t, o.do(http.MethodGet, "/api/v1/properties/"+p1+"/access-info", o.security, nil), http.StatusOK)
	expect(t, o.do(http.MethodGet, "/api/v1/properties/"+p1+"/access-info", o.owner, nil), http.StatusForbidden)

	cleared := o.do(http.MethodPatch, "/api/v1/properties/"+p1, o.admin, map[string]any{"access_info": nil})
	expect(t, cleared, http.StatusOK)
	if cleared.body["has_access_info"] != false {
		t.Errorf("null borra el acceso: %v", cleared.body)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/properties/"+p1+"/access-info", o.admin, nil), http.StatusNotFound)
}

func TestPropertyLifecycle(t *testing.T) {
	e := setup(t)
	o := e.ops()
	tpl := o.do(http.MethodPost, "/api/v1/checklist-templates", o.admin, map[string]any{
		"name": "Estándar", "is_default": true, "items": []map[string]any{{"section": "Cocina", "text": "Lavar trastos"}},
	})
	expect(t, tpl, http.StatusCreated)
	b := o.do(http.MethodPost, "/api/v1/buildings", o.admin, map[string]any{"name": "Cayalá"})
	expect(t, b, http.StatusCreated)

	created := o.do(http.MethodPost, "/api/v1/properties", o.admin, map[string]any{
		"code": " tb-504 ", "name": "  Torre   Botánica 504 ", "type": "apartment", "building_id": b.body["id"],
		"zone": "Zona 10", "lat": 14.5986, "lng": -90.5094, "bedrooms": 2,
	})
	expect(t, created, http.StatusCreated)
	id := created.body["id"].(string)
	if created.body["code"] != "TB-504" || created.body["name"] != "Torre Botánica 504" ||
		created.body["checklist_template_id"] != tpl.body["id"] || created.body["building_name"] != "Cayalá" {
		t.Errorf("alta normalizada con plantilla por defecto: %v", created.body)
	}

	checks := []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"code": "TB-504", "name": "Otra", "type": "house"}, "body.code"},
		{map[string]any{"code": "A", "name": "Corta", "type": "house"}, "body.code"},
		{map[string]any{"code": "X-1", "name": "Sin edificio", "type": "house", "building_id": uuid.NewString()}, "body.building_id"},
		{map[string]any{"code": "X-2", "name": "Solo latitud", "type": "house", "lat": 14.5}, "body.lat"},
	}
	for _, c := range checks {
		res := o.do(http.MethodPost, "/api/v1/properties", o.admin, c.body)
		expect(t, res, http.StatusUnprocessableEntity)
		if field(res) != c.field {
			t.Errorf("%v: campo %q, se esperaba %q", c.body, field(res), c.field)
		}
	}

	patched := o.do(http.MethodPatch, "/api/v1/properties/"+id, o.admin, map[string]any{"building_id": nil, "max_guests": 4})
	expect(t, patched, http.StatusOK)
	if patched.body["building_id"] != nil || patched.body["max_guests"] != float64(4) || patched.body["bedrooms"] != float64(2) ||
		patched.body["zone"] != "Zona 10" {
		t.Errorf("PATCH solo cambia lo que viene: %v", patched.body)
	}
	expect(t, o.do(http.MethodDelete, "/api/v1/buildings/"+b.body["id"].(string), o.admin, nil), http.StatusNoContent)

	// Paginacion por cursor y busqueda.
	o.property("AN-SEB", nil)
	o.property("CY-12A", nil)
	first := o.do(http.MethodGet, "/api/v1/properties?limit=2", o.admin, nil)
	expect(t, first, http.StatusOK)
	next, _ := first.body["next_cursor"].(string)
	if got := codes(first); !slices.Equal(got, []string{"AN-SEB", "CY-12A"}) || next == "" {
		t.Fatalf("primera pagina: %v %v", got, first.body["next_cursor"])
	}
	second := o.do(http.MethodGet, "/api/v1/properties?limit=2&cursor="+url.QueryEscape(next), o.admin, nil)
	if got := codes(second); !slices.Equal(got, []string{"TB-504"}) || second.body["next_cursor"] != nil {
		t.Errorf("segunda pagina: %v %v", got, second.body["next_cursor"])
	}
	expect(t, o.do(http.MethodGet, "/api/v1/properties?cursor=basura", o.admin, nil), http.StatusUnprocessableEntity)
	if got := codes(o.do(http.MethodGet, "/api/v1/properties?q=botan", o.admin, nil)); !slices.Equal(got, []string{"TB-504"}) {
		t.Errorf("busqueda: %v", got)
	}

	// Borrado logico: desaparece, libera el codigo y queda en la bitacora.
	expect(t, o.do(http.MethodDelete, "/api/v1/properties/"+id, o.admin, nil), http.StatusNoContent)
	expect(t, o.do(http.MethodGet, "/api/v1/properties/"+id, o.admin, nil), http.StatusNotFound)
	o.property("TB-504", nil)
	for _, action := range []string{"property.created", "property.updated", "property.deleted"} {
		var n int
		if err := testDB.Pool().QueryRow(context.Background(), e.auditCount(), action).Scan(&n); err != nil || n == 0 {
			t.Errorf("%s debe quedar en la bitacora: %d %v", action, n, err)
		}
	}
}

func TestAssignmentsAndInvitationProperties(t *testing.T) {
	e := setup(t)
	o := e.ops()
	p1, p2 := o.property("TB-504", nil), o.property("CY-12A", nil)
	maria := o.ids["maria@garrent.gt"].String()

	set := o.do(http.MethodPut, "/api/v1/property-assignments/"+maria, o.admin, map[string]any{"property_ids": []string{p1, p2, p1}})
	expect(t, set, http.StatusOK)
	if n := len(set.body["property_ids"].([]any)); n != 2 {
		t.Errorf("asignaciones sin repetidos: %v", set.body)
	}
	if got := codes(o.do(http.MethodGet, "/api/v1/properties", o.cleaner, nil)); len(got) != 2 {
		t.Errorf("limpieza ve sus dos propiedades: %v", got)
	}
	if got := codes(o.do(http.MethodGet, "/api/v1/properties?assignee="+maria, o.admin, nil)); len(got) != 2 {
		t.Errorf("filtro por asignado: %v", got)
	}
	members := o.do(http.MethodGet, e.tenantPath(o.tenant, "/members"), o.admin, nil)
	for _, m := range items(members) {
		if m["user_id"] == maria && m["properties_count"] != float64(2) {
			t.Errorf("properties_count: %v", m)
		}
	}
	bad := o.do(http.MethodPut, "/api/v1/property-assignments/"+maria, o.admin, map[string]any{"property_ids": []string{uuid.NewString()}})
	expect(t, bad, http.StatusUnprocessableEntity)
	if field(bad) != "body.property_ids" {
		t.Errorf("campo: %v", bad.body)
	}
	stranger := e.member("nadie@otra.gt", nil)
	notMember := o.do(http.MethodPut, "/api/v1/properties/"+p1+"/assignments", o.admin, map[string]any{"user_ids": []string{stranger.String()}})
	expect(t, notMember, http.StatusUnprocessableEntity)
	if field(notMember) != "body.user_ids" {
		t.Errorf("campo: %v", notMember.body)
	}
	expect(t, o.do(http.MethodPut, "/api/v1/property-assignments/"+stranger.String(), o.admin, map[string]any{"property_ids": []string{}}), http.StatusNotFound)

	// La invitacion guarda las propiedades y se aplican al aceptar.
	unknown := o.do(http.MethodPost, e.tenantPath(o.tenant, "/invitations"), o.admin, map[string]any{
		"email": "andrea@garrent.gt", "role_key": "cleaner", "property_ids": []string{uuid.NewString()},
	})
	expect(t, unknown, http.StatusUnprocessableEntity)
	if field(unknown) != "body.property_ids" {
		t.Errorf("campo: %v", unknown.body)
	}
	expect(t, o.do(http.MethodPost, e.tenantPath(o.tenant, "/invitations"), o.admin, map[string]any{
		"email": "andrea@garrent.gt", "role_key": "cleaner", "property_ids": []string{p2},
	}), http.StatusCreated)
	accepted := o.do(http.MethodPost, "/api/v1/auth/invitations/accept", "", map[string]any{
		"token": e.mailLink("andrea@garrent.gt", "invitation"), "password": "clave-nueva-123", "full_name": "Andrea Soto", "device": device,
	})
	expect(t, accepted, http.StatusOK)
	if got := codes(o.do(http.MethodGet, "/api/v1/properties", accepted.body["access_token"].(string), nil)); !slices.Equal(got, []string{"CY-12A"}) {
		t.Errorf("la invitada ve sus propiedades: %v", got)
	}
}

// Un rol personalizado con alcance restringido se comporta como limpieza, y
// quien lo tiene no crea roles que ven todo.
func TestScopedCustomRole(t *testing.T) {
	e := setup(t)
	o := e.ops()
	p1 := o.property("TB-504", nil)
	o.property("CY-12A", nil)
	role := o.do(http.MethodPost, e.tenantPath(o.tenant, "/roles"), o.admin, map[string]any{
		"name": "Coordinación de zona", "scoped": true,
		"permissions": []string{"properties.read", "roles.manage", "users.read"},
	})
	expect(t, role, http.StatusCreated)
	if role.body["scoped"] != true {
		t.Fatalf("rol con alcance: %v", role.body)
	}
	coord := e.member("coord@garrent.gt", nil)
	if err := testDB.InTx(context.Background(), func(q database.Querier) error {
		return authz.AddMember(context.Background(), q, coord, o.tenant, authz.RoleKey(role.body["key"].(string)))
	}); err != nil {
		t.Fatal(err)
	}
	expect(t, o.do(http.MethodPut, "/api/v1/property-assignments/"+coord.String(), o.admin, map[string]any{"property_ids": []string{p1}}), http.StatusOK)
	token, _ := e.session("coord@garrent.gt")
	if got := codes(o.do(http.MethodGet, "/api/v1/properties", token, nil)); !slices.Equal(got, []string{"TB-504"}) {
		t.Errorf("rol con alcance ve solo lo asignado: %v", got)
	}
	open := o.do(http.MethodPost, e.tenantPath(o.tenant, "/roles"), token, map[string]any{
		"name": "Ve todo", "permissions": []string{"properties.read"},
	})
	expect(t, open, http.StatusUnprocessableEntity)
	if field(open) != "body.scoped" {
		t.Errorf("campo: %v", open.body)
	}
}

func TestChecklistTemplates(t *testing.T) {
	e := setup(t)
	o := e.ops()
	a := o.do(http.MethodPost, "/api/v1/checklist-templates", o.admin, map[string]any{
		"name": "Estándar", "is_default": true, "items": []map[string]any{
			{"section": "Baño", "text": "Desinfectar", "requires_before_after": true},
			{"section": "Cocina", "text": "Lavar"},
		},
	})
	expect(t, a, http.StatusCreated)
	first := a.body["items"].([]any)[0].(map[string]any)
	if first["requires_photo"] != true || first["position"] != float64(1) {
		t.Errorf("antes y despues implica foto: %v", first)
	}
	b := o.do(http.MethodPost, "/api/v1/checklist-templates", o.admin, map[string]any{
		"name": "Salida", "is_default": true, "items": []map[string]any{{"section": "Inventario", "text": "Contar toallas"}},
	})
	expect(t, b, http.StatusCreated)
	list := items(o.do(http.MethodGet, "/api/v1/checklist-templates", o.cleaner, nil))
	if len(list) != 2 || list[0]["name"] != "Salida" || list[1]["is_default"] != false {
		t.Errorf("una sola por defecto, primero: %v", list)
	}
	dup := o.do(http.MethodPost, "/api/v1/checklist-templates", o.admin, map[string]any{"name": "estándar", "items": []map[string]any{{"section": "x", "text": "y"}}})
	expect(t, dup, http.StatusUnprocessableEntity)
	empty := o.do(http.MethodPost, "/api/v1/checklist-templates", o.admin, map[string]any{"name": "Vacía", "items": []map[string]any{}})
	if field(empty) != "body.items" {
		t.Errorf("sin puntos: %v", empty.body)
	}
	expect(t, o.do(http.MethodPost, "/api/v1/checklist-templates", o.cleaner, map[string]any{"name": "X", "items": []map[string]any{}}), http.StatusForbidden)

	patched := o.do(http.MethodPatch, "/api/v1/checklist-templates/"+a.body["id"].(string), o.admin, map[string]any{
		"items": []map[string]any{{"section": "Cocina", "text": "Uno"}, {"section": "Cocina", "text": "Dos"}, {"section": "Sala", "text": "Tres"}},
	})
	expect(t, patched, http.StatusOK)
	if its := patched.body["items"].([]any); len(its) != 3 || its[2].(map[string]any)["position"] != float64(3) {
		t.Errorf("items reemplaza todo: %v", patched.body["items"])
	}
	p := o.property("TB-504", map[string]any{"checklist_template_id": a.body["id"]})
	expect(t, o.do(http.MethodDelete, "/api/v1/checklist-templates/"+a.body["id"].(string), o.admin, nil), http.StatusNoContent)
	if got := o.do(http.MethodGet, "/api/v1/properties/"+p, o.admin, nil); got.body["checklist_template_id"] != nil {
		t.Errorf("la propiedad queda sin plantilla: %v", got.body)
	}
}

func TestEmployeesAndSchedule(t *testing.T) {
	e := setup(t)
	o := e.ops()
	maria := o.ids["maria@garrent.gt"].String()
	base := map[string]any{"user_id": maria, "position": "Limpieza", "hire_date": "2024-03-11", "pay_type": "per_cleaning", "pay_rate": 165.456}

	invalid := []struct {
		extra map[string]any
		field string
	}{
		{map[string]any{"document_type": "dpi", "document_number": "123"}, "body.document_number"},
		{map[string]any{"document_number": "1234567890123"}, "body.document_type"},
		{map[string]any{"nit": "abc"}, "body.nit"},
		{map[string]any{"termination_date": "2020-01-01"}, "body.termination_date"},
		{map[string]any{"emergency_name": "Rosa"}, "body.emergency_phone"},
		{map[string]any{"schedule": []map[string]any{{"weekday": 1, "starts_at": "07:00", "ends_at": "12:00"}, {"weekday": 1, "starts_at": "11:00", "ends_at": "15:00"}}}, "body.schedule"},
	}
	for _, c := range invalid {
		body := map[string]any{}
		for k, v := range base {
			body[k] = v
		}
		for k, v := range c.extra {
			body[k] = v
		}
		res := o.do(http.MethodPost, "/api/v1/employees", o.admin, body)
		expect(t, res, http.StatusUnprocessableEntity)
		if field(res) != c.field {
			t.Errorf("%v: campo %q, se esperaba %q", c.extra, field(res), c.field)
		}
	}

	body := map[string]any{
		"document_type": "dpi", "document_number": "2547 89654 0101", "nit": "1234567-8",
		"emergency_name": "Rosa Ramos", "emergency_phone": "+502 5555 1234",
		"schedule": []map[string]any{{"weekday": 1, "starts_at": "07:00", "ends_at": "15:00"}},
	}
	for k, v := range base {
		body[k] = v
	}
	created := o.do(http.MethodPost, "/api/v1/employees", o.admin, body)
	expect(t, created, http.StatusCreated)
	if created.body["employee_code"] != "EMP-0001" || created.body["document_number"] != "2547896540101" ||
		created.body["pay_rate"] != 165.46 || len(created.body["schedule"].([]any)) != 1 {
		t.Errorf("ficha creada: %v", created.body)
	}
	id := created.body["id"].(string)
	again := o.do(http.MethodPost, "/api/v1/employees", o.admin, base)
	if field(again) != "body.user_id" {
		t.Errorf("una ficha por persona: %v", again.body)
	}
	stranger := e.member("nadie@otra.gt", nil)
	notMember := o.do(http.MethodPost, "/api/v1/employees", o.admin, map[string]any{
		"user_id": stranger.String(), "position": "X y", "hire_date": "2024-01-01", "pay_type": "per_day", "pay_rate": 1,
	})
	if field(notMember) != "body.user_id" {
		t.Errorf("solo personas de la empresa: %v", notMember.body)
	}

	patched := o.do(http.MethodPatch, "/api/v1/employees/"+id, o.admin, map[string]any{"nit": nil, "position": "Limpieza senior"})
	expect(t, patched, http.StatusOK)
	if patched.body["nit"] != nil || patched.body["document_number"] != "2547896540101" || patched.body["position"] != "Limpieza senior" {
		t.Errorf("PATCH: %v", patched.body)
	}
	var after string
	if err := testDB.Pool().QueryRow(context.Background(),
		`SELECT after::text FROM tenant_garrent.audit_log WHERE action = 'employee.updated'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(after, "1234567") || !strings.Contains(after, "nit") {
		t.Errorf("la bitacora dice que cambio, nunca el valor: %s", after)
	}

	schedule := o.do(http.MethodPut, "/api/v1/employees/"+id+"/schedule", o.admin, map[string]any{"blocks": []map[string]any{
		{"weekday": 6, "starts_at": "08:00", "ends_at": "12:00"}, {"weekday": 1, "starts_at": "07:00", "ends_at": "15:00"},
	}})
	expect(t, schedule, http.StatusOK)
	if s := schedule.body["schedule"].([]any); len(s) != 2 || s[0].(map[string]any)["weekday"] != float64(1) {
		t.Errorf("horario ordenado por dia: %v", schedule.body["schedule"])
	}
	if got := items(o.do(http.MethodGet, "/api/v1/employees?user_id="+maria, o.admin, nil)); len(got) != 1 {
		t.Errorf("filtro por persona: %v", got)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/employees", o.super, nil), http.StatusForbidden)

	boot := o.do(http.MethodGet, "/api/v1/sync/bootstrap", o.cleaner, nil)
	expect(t, boot, http.StatusOK)
	if len(boot.body["schedule"].([]any)) != 2 {
		t.Errorf("el bootstrap trae el horario propio: %v", boot.body["schedule"])
	}
}

func TestAuditLogPages(t *testing.T) {
	e := setup(t)
	o := e.ops()
	p1 := o.property("TB-504", nil)
	o.property("CY-12A", nil)
	expect(t, o.do(http.MethodPatch, "/api/v1/properties/"+p1, o.admin, map[string]any{"name": "Torre 504"}), http.StatusOK)

	seen := map[string]bool{}
	cursor := ""
	for range 5 {
		path := "/api/v1/audit?limit=2"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		res := o.do(http.MethodGet, path, o.admin, nil)
		expect(t, res, http.StatusOK)
		for _, it := range items(res) {
			if seen[it["id"].(string)] {
				t.Fatalf("fila repetida entre paginas: %v", it)
			}
			seen[it["id"].(string)] = true
		}
		next, ok := res.body["next_cursor"].(string)
		if !ok {
			break
		}
		cursor = next
	}
	if len(seen) != 3 {
		t.Errorf("tres cambios en la bitacora, vistos %d", len(seen))
	}
	byProp := items(o.do(http.MethodGet, "/api/v1/audit?property_id="+p1, o.admin, nil))
	if len(byProp) != 2 || byProp[0]["action"] != "property.updated" || byProp[0]["entity_label"] != "TB-504" ||
		byProp[0]["actor_role"] != "Administrador" || byProp[0]["property_code"] != "TB-504" {
		t.Errorf("filtro por propiedad: %v", byProp)
	}
	expect(t, o.do(http.MethodGet, "/api/v1/audit", o.cleaner, nil), http.StatusForbidden)
}

func TestSyncBootstrapIsScoped(t *testing.T) {
	e := setup(t)
	o := e.ops()
	tpl := o.do(http.MethodPost, "/api/v1/checklist-templates", o.admin, map[string]any{
		"name": "Estándar", "items": []map[string]any{{"section": "Cocina", "text": "Lavar"}},
	})
	o.do(http.MethodPost, "/api/v1/checklist-templates", o.admin, map[string]any{
		"name": "Otra", "items": []map[string]any{{"section": "Sala", "text": "Barrer"}},
	})
	p1 := o.property("TB-504", map[string]any{"checklist_template_id": tpl.body["id"], "access_info": "secreto"})
	o.property("CY-12A", nil)
	expect(t, o.do(http.MethodPut, "/api/v1/properties/"+p1+"/assignments", o.admin,
		map[string]any{"user_ids": []string{o.ids["maria@garrent.gt"].String()}}), http.StatusOK)

	boot := o.do(http.MethodGet, "/api/v1/sync/bootstrap", o.cleaner, nil)
	expect(t, boot, http.StatusOK)
	props := boot.body["properties"].([]any)
	tpls := boot.body["checklist_templates"].([]any)
	if boot.body["scoped"] != true || len(props) != 1 || len(tpls) != 1 || tpls[0].(map[string]any)["name"] != "Estándar" {
		t.Errorf("bootstrap de limpieza: %v", boot.body)
	}
	if _, leaked := props[0].(map[string]any)["access_info"]; leaked {
		t.Error("el bootstrap nunca trae el texto de acceso")
	}
	all := o.do(http.MethodGet, "/api/v1/sync/bootstrap", o.admin, nil)
	if all.body["scoped"] != false || len(all.body["properties"].([]any)) != 2 {
		t.Errorf("bootstrap del admin: %v", all.body)
	}
}
