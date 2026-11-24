package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/app"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/checklists"
	"github.com/smorareqs/argos-service/internal/modules/employees"
	"github.com/smorareqs/argos-service/internal/modules/properties"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
)

// Semilla de la fase 2 (Persona A): edificios, propiedades, plantillas de
// checklist, asignaciones y fichas laborales de las empresas de demostracion.
// Son las mismas propiedades que contaba el mock de la web
// (WEB/src/app/core/mocks/demo-properties.ts). Pasa por los servicios, asi la
// bitacora de cada empresa tiene entradas reales. Es idempotente: lo que ya
// existe (por codigo, nombre o persona) no se toca.

type demoBuilding struct {
	name, address, zone string
	lat, lng            float64
}

type demoProperty struct {
	code, name, kind, building, zone string
	lat, lng                         float64
	rooms                            [3]int
	assigned, owners                 []string // correos
	access                           string
	inactive                         bool
}

type demoEmployee struct {
	email, position, payType string
	payRate                  float64
	onPayroll                bool
	hireDate                 string
}

type demoOps struct {
	admin     string // correo de quien hace los cambios (queda en la bitacora)
	buildings []demoBuilding
	props     []demoProperty
	employees []demoEmployee
}

var demoTemplates = []checklists.TemplateCreate{
	{
		Name: "Limpieza estándar", Description: ptr("Entre huéspedes: deja la propiedad lista para el siguiente check-in."),
		IsDefault: true, Items: []checklists.ItemInput{
			{Section: "Entrada y sala", Text: "Barrer y trapear pisos"},
			{Section: "Entrada y sala", Text: "Sacudir muebles y repisas"},
			{Section: "Entrada y sala", Text: "Foto general de la sala ordenada", RequiresPhoto: true},
			{Section: "Cocina", Text: "Lavar y guardar la vajilla"},
			{Section: "Cocina", Text: "Limpiar estufa, horno y microondas", RequiresBeforeAfter: true},
			{Section: "Cocina", Text: "Vaciar y limpiar la refrigeradora"},
			{Section: "Habitaciones", Text: "Cambiar sábanas y fundas", RequiresPhoto: true},
			{Section: "Habitaciones", Text: "Revisar debajo de camas y closets"},
			{Section: "Baños", Text: "Desinfectar inodoro, lavamanos y ducha", RequiresBeforeAfter: true},
			{Section: "Baños", Text: "Reponer papel, jabón y toallas", RequiresPhoto: true},
			{Section: "Cierre", Text: "Sacar la basura y dejar bolsas nuevas"},
			{Section: "Cierre", Text: "Cerrar ventanas, apagar luces y aire"},
		},
	},
	{
		Name: "Revisión de salida", Description: ptr("Al check-out: inventario y daños antes de limpiar."),
		Items: []checklists.ItemInput{
			{Section: "Inventario", Text: "Contar toallas y ropa de cama", RequiresPhoto: true},
			{Section: "Inventario", Text: "Revisar control remoto y llaves"},
			{Section: "Daños", Text: "Fotografiar cualquier daño o mancha", RequiresPhoto: true},
			{Section: "Daños", Text: "Revisar electrodomésticos encendiendo cada uno"},
		},
	},
}

var demoOperations = map[string]demoOps{
	"garrent": {
		admin: "ana.lopez@garrent.gt",
		buildings: []demoBuilding{
			{"Torre Botánica", "4a avenida 15-70", "Zona 10, Ciudad de Guatemala", 14.5986, -90.5094},
			{"Residencial Cayalá", "Boulevard Austriaco 36-12", "Zona 16, Ciudad de Guatemala", 14.6092, -90.4867},
			{"Hostal Antigua", "6a calle poniente 21", "Antigua Guatemala", 14.5572, -90.7347},
		},
		props: []demoProperty{
			{
				code: "TB-504", name: "Torre Botánica 504", kind: "apartment", building: "Torre Botánica", rooms: [3]int{2, 2, 4},
				assigned: []string{"maria.ramos@garrent.gt"}, owners: []string{"ricardo.castillo@garrent.gt"},
				access: "Caja de llaves en recepción · código 4821\nWifi: Botanica504 · clave verde-palma-72\nParqueo: sótano 2, espacio 31",
			},
			{
				code: "TB-1102", name: "Torre Botánica 1102", kind: "apartment", building: "Torre Botánica", rooms: [3]int{3, 2, 6},
				assigned: []string{"luis.torres@garrent.gt"}, access: "Cerradura digital · código 0619#\nWifi: TB1102 · clave lago-azul-19",
			},
			{
				code: "TB-207", name: "Torre Botánica 207", kind: "apartment", building: "Torre Botánica", rooms: [3]int{1, 1, 2},
				assigned: []string{"maria.ramos@garrent.gt"},
			},
			{code: "TB-903", name: "Torre Botánica 903", kind: "apartment", building: "Torre Botánica", rooms: [3]int{2, 2, 4}, inactive: true},
			{
				code: "CY-12A", name: "Cayalá 12A", kind: "apartment", building: "Residencial Cayalá", rooms: [3]int{2, 2, 4},
				assigned: []string{"diego.castillo@garrent.gt"}, owners: []string{"isabel.monzon@garrent.gt"},
				access: "Llave con el agente de garita · torre B\nWifi: Cayala12A · clave paseo-sol-88",
			},
			{
				code: "CY-08C", name: "Cayalá 8C", kind: "apartment", building: "Residencial Cayalá", rooms: [3]int{1, 1, 3},
				assigned: []string{"luis.torres@garrent.gt"},
			},
			{
				code: "CY-PH1", name: "Penthouse Cayalá", kind: "apartment", building: "Residencial Cayalá", rooms: [3]int{3, 3, 6},
				assigned: []string{"maria.ramos@garrent.gt", "luis.torres@garrent.gt"}, owners: []string{"ricardo.castillo@garrent.gt"},
			},
			{
				code: "AN-SEB", name: "Casa San Sebastián", kind: "house", zone: "Antigua Guatemala", lat: 14.5611, lng: -90.7381,
				rooms: [3]int{4, 3, 8}, assigned: []string{"diego.castillo@garrent.gt"}, owners: []string{"ricardo.castillo@garrent.gt"},
				access: "Portón: control remoto en caja 12\nPuerta principal: llave de bronce en el llavero rojo",
			},
			{
				code: "AN-CAR", name: "Casa del Carmen", kind: "house", zone: "Antigua Guatemala", lat: 14.5547, lng: -90.7299,
				rooms: [3]int{3, 2, 6}, assigned: []string{"maria.ramos@garrent.gt"},
			},
			{
				code: "AN-H03", name: "Hostal Antigua · Hab. 3", kind: "room", building: "Hostal Antigua", rooms: [3]int{1, 1, 2},
				assigned: []string{"luis.torres@garrent.gt"},
			},
			{
				code: "AN-H05", name: "Hostal Antigua · Hab. 5", kind: "room", building: "Hostal Antigua", rooms: [3]int{1, 1, 2},
				assigned: []string{"luis.torres@garrent.gt"},
			},
			{
				code: "AT-VIS", name: "Villa Vista al Lago", kind: "house", zone: "Panajachel, Sololá", lat: 14.7409, lng: -91.1577,
				rooms: [3]int{4, 4, 10}, assigned: []string{"diego.castillo@garrent.gt"}, owners: []string{"isabel.monzon@garrent.gt"},
				access: "Muelle: candado 7731\nCasa: cerradura digital 2468*",
			},
			{
				code: "MT-PLY", name: "Casa Playa Monterrico", kind: "house", zone: "Monterrico, Santa Rosa", lat: 13.8946, lng: -90.4836,
				rooms: [3]int{3, 2, 8}, assigned: []string{"maria.ramos@garrent.gt", "luis.torres@garrent.gt"},
			},
		},
		employees: []demoEmployee{
			{"jorge.pena@garrent.gt", "Supervisor de operaciones", "fixed_monthly", 7800, true, "2023-02-01"},
			{"maria.ramos@garrent.gt", "Colaboradora de limpieza", "per_cleaning", 165, true, "2024-03-11"},
			{"luis.torres@garrent.gt", "Técnico de mantenimiento", "per_day", 260, true, "2023-09-04"},
			{"diego.castillo@garrent.gt", "Técnico de mantenimiento", "per_hour", 38, false, "2025-06-16"},
			{"sofia.mendez@garrent.gt", "Agente de seguridad", "fixed_monthly", 5200, true, "2024-01-08"},
		},
	},
	"costa_limpia": {
		admin: "carlos.ruiz@argos.gt",
		buildings: []demoBuilding{
			{"Condominio Marbella", "Km 107 carretera al Puerto", "Puerto de San José, Escuintla", 13.9254, -90.8243},
		},
		props: []demoProperty{
			{
				code: "MB-3B", name: "Marbella 3B", kind: "apartment", building: "Condominio Marbella", rooms: [3]int{2, 2, 5},
				assigned: []string{"pedro.aju@costalimpia.gt"}, access: "Llave en la garita principal · nombre de Costa Limpia",
			},
			{
				code: "MB-5A", name: "Marbella 5A", kind: "apartment", building: "Condominio Marbella", rooms: [3]int{2, 1, 4},
				assigned: []string{"rosa.xol@costalimpia.gt"},
			},
			{
				code: "MB-7C", name: "Marbella 7C", kind: "apartment", building: "Condominio Marbella", rooms: [3]int{3, 2, 6},
				assigned: []string{"pedro.aju@costalimpia.gt", "rosa.xol@costalimpia.gt"},
			},
			{
				code: "MT-SOL", name: "Casa Sol · Monterrico", kind: "house", zone: "Monterrico, Santa Rosa", lat: 13.8961, lng: -90.4772,
				rooms: [3]int{4, 3, 10}, assigned: []string{"rosa.xol@costalimpia.gt"},
			},
		},
		employees: []demoEmployee{
			{"pedro.aju@costalimpia.gt", "Colaborador de limpieza", "per_cleaning", 150, true, "2024-11-04"},
			{"rosa.xol@costalimpia.gt", "Colaboradora de limpieza", "per_day", 210, false, "2025-02-17"},
		},
	},
}

// demoSchedule es el horario de las fichas sembradas: lunes a viernes de 7 a
// 15 y sabado de 8 a 12.
var demoSchedule = []employees.ScheduleBlock{
	{Weekday: 1, StartsAt: "07:00", EndsAt: "15:00"},
	{Weekday: 2, StartsAt: "07:00", EndsAt: "15:00"},
	{Weekday: 3, StartsAt: "07:00", EndsAt: "15:00"},
	{Weekday: 4, StartsAt: "07:00", EndsAt: "15:00"},
	{Weekday: 5, StartsAt: "07:00", EndsAt: "15:00"},
	{Weekday: 6, StartsAt: "08:00", EndsAt: "12:00"},
}

// seedOperations siembra la fase 2 en cada empresa de demostracion.
func seedOperations(ctx context.Context, services app.Services, ids map[string]tenants.Tenant) error {
	for slug, ops := range demoOperations {
		t := ids[slug]
		admin, err := services.Auth.UserByEmail(ctx, ops.admin)
		if err != nil {
			return err
		}
		tctx := database.WithTenantSchema(ctx, t.SchemaName)
		actor := authz.Actor{UserID: admin.ID, TenantID: t.ID, Permissions: allPermissions()}
		s := opsSeeder{services: services, ctx: tctx, actor: actor}
		if err := s.run(ops); err != nil {
			return fmt.Errorf("operacion de %s: %w", slug, err)
		}
	}
	fmt.Println("Propiedades, edificios, plantillas y fichas laborales de demostracion listas.")
	return nil
}

type opsSeeder struct {
	services app.Services
	ctx      context.Context
	actor    authz.Actor
}

func (s opsSeeder) run(ops demoOps) error {
	if err := s.templates(); err != nil {
		return err
	}
	buildings, err := s.buildings(ops.buildings)
	if err != nil {
		return err
	}
	if err := s.properties(ops.props, buildings); err != nil {
		return err
	}
	return s.employees(ops.employees)
}

func (s opsSeeder) templates() error {
	existing, err := s.services.Checklists.List(s.ctx)
	if err != nil {
		return err
	}
	for _, t := range demoTemplates {
		if hasTemplate(existing, t.Name) {
			continue
		}
		if _, err := s.services.Checklists.Create(s.ctx, s.actor, t); err != nil {
			return err
		}
	}
	return nil
}

func (s opsSeeder) buildings(list []demoBuilding) (map[string]uuid.UUID, error) {
	existing, err := s.services.Properties.Buildings(s.ctx, s.actor)
	if err != nil {
		return nil, err
	}
	out := map[string]uuid.UUID{}
	for _, b := range existing {
		out[b.Name] = b.ID
	}
	for _, b := range list {
		if _, ok := out[b.name]; ok {
			continue
		}
		created, err := s.services.Properties.CreateBuilding(s.ctx, s.actor, properties.BuildingCreate{
			Name: b.name, Address: ptr(b.address), Zone: ptr(b.zone), Lat: ptr(b.lat), Lng: ptr(b.lng),
		})
		if err != nil {
			return nil, err
		}
		out[b.name] = created.ID
	}
	return out, nil
}

func (s opsSeeder) properties(list []demoProperty, buildings map[string]uuid.UUID) error {
	existing, _, err := s.services.Properties.List(s.ctx, s.actor, properties.ListFilter{Limit: 500})
	if err != nil {
		return err
	}
	for _, p := range list {
		if hasProperty(existing, p.code) {
			continue
		}
		created, err := s.services.Properties.Create(s.ctx, s.actor, newDemoProperty(p, buildings))
		if err != nil {
			return fmt.Errorf("propiedad %s: %w", p.code, err)
		}
		if err := s.people(created.ID, p); err != nil {
			return fmt.Errorf("personas de %s: %w", p.code, err)
		}
		if p.inactive {
			off := false
			if _, err := s.services.Properties.Update(s.ctx, s.actor, created.ID, properties.PropertyUpdate{IsActive: &off}); err != nil {
				return err
			}
		}
	}
	return nil
}

func newDemoProperty(p demoProperty, buildings map[string]uuid.UUID) properties.PropertyCreate {
	in := properties.PropertyCreate{
		Code: p.code, Name: p.name, Type: p.kind,
		Bedrooms: ptr(p.rooms[0]), Bathrooms: ptr(p.rooms[1]), MaxGuests: ptr(p.rooms[2]),
	}
	if id, ok := buildings[p.building]; ok {
		in.BuildingID = &id
	}
	if p.zone != "" {
		in.Zone, in.Lat, in.Lng = ptr(p.zone), ptr(p.lat), ptr(p.lng)
	}
	if p.access != "" {
		in.AccessInfo = ptr(p.access)
	}
	return in
}

func (s opsSeeder) people(propertyID uuid.UUID, p demoProperty) error {
	assigned, err := s.userIDs(p.assigned)
	if err != nil {
		return err
	}
	if len(assigned) > 0 {
		if _, err := s.services.Properties.SetAssignees(s.ctx, s.actor, propertyID, assigned); err != nil {
			return err
		}
	}
	owners, err := s.userIDs(p.owners)
	if err != nil {
		return err
	}
	if len(owners) == 0 {
		return nil
	}
	_, err = s.services.Properties.SetOwners(s.ctx, s.actor, propertyID, owners)
	return err
}

func (s opsSeeder) employees(list []demoEmployee) error {
	existing, err := s.services.Employees.List(s.ctx, nil)
	if err != nil {
		return err
	}
	for _, e := range list {
		user, err := s.services.Auth.UserByEmail(s.ctx, e.email)
		if err != nil {
			return err
		}
		if hasEmployee(existing, user.ID) {
			continue
		}
		if _, err := s.services.Employees.Create(s.ctx, s.actor, employees.EmployeeCreate{
			UserID: user.ID, Position: e.position, HireDate: e.hireDate, PayType: e.payType, PayRate: e.payRate,
			OnPayroll: e.onPayroll, Schedule: demoSchedule,
		}); err != nil {
			return fmt.Errorf("ficha de %s: %w", e.email, err)
		}
	}
	return nil
}

func (s opsSeeder) userIDs(emails []string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(emails))
	for _, email := range emails {
		user, err := s.services.Auth.UserByEmail(s.ctx, email)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", email, err)
		}
		out = append(out, user.ID)
	}
	return out, nil
}

func allPermissions() map[authz.Permission]bool {
	out := make(map[authz.Permission]bool, len(authz.All))
	for _, p := range authz.All {
		out[p] = true
	}
	return out
}

func hasTemplate(list []checklists.TemplateRead, name string) bool {
	for _, t := range list {
		if strings.EqualFold(t.Name, name) {
			return true
		}
	}
	return false
}

func hasProperty(list []properties.PropertyRead, code string) bool {
	for _, p := range list {
		if p.Code == code {
			return true
		}
	}
	return false
}

func hasEmployee(list []employees.EmployeeRead, userID uuid.UUID) bool {
	for _, e := range list {
		if e.UserID == userID {
			return true
		}
	}
	return false
}

func ptr[T any](v T) *T {
	return &v
}
