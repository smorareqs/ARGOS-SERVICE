package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/app"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/maintenance"
	"github.com/smorareqs/argos-service/internal/modules/properties"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
)

// Semilla de la fase 3 (Persona A): proveedores, activos, preventivos e
// incidencias de las empresas de demostracion. Pasa por los servicios (las
// alertas y la bitacora salen igual que en produccion) y despues corre el job
// de preventivos. Solo el historial de resueltas se fecha hacia atras con SQL
// para que los indicadores (W-G5) tengan seis meses. Si la empresa ya tiene
// proveedores, no se toca.

type demoAsset struct {
	key, property, building, category, name, brand, model, location, vendor, status, installed string
}

type demoPlan struct {
	asset, name, responsible, vendor string
	freq, dueIn, lead                int
}

type demoTicket struct {
	reporter, property, asset, title, description, urgency string
	assignee, vendor                                       string
	state                                                  string // open, assigned, in_progress, resolved, closed
	resolution                                             string
	cost                                                   float64
	ageDays                                                int // dias desde el reporte (solo historial)
	hours                                                  int // horas hasta resolverse (solo historial)
}

type demoMaintenance struct {
	vendors []maintenance.VendorCreate
	assets  []demoAsset
	plans   []demoPlan
	tickets []demoTicket
}

var demoMaintenanceData = map[string]demoMaintenance{
	"garrent": {
		vendors: []maintenance.VendorCreate{
			{Name: "Elevadores del Valle", ServiceType: ptr("Elevadores"), ContactName: ptr("Héctor Juárez"), Phone: ptr("+502 2365 1180"), Email: ptr("servicio@elevadoresdelvalle.gt")},
			{Name: "Fríos del Sur", ServiceType: ptr("Aire acondicionado"), ContactName: ptr("Karla Ordóñez"), Phone: ptr("+502 5521 9043")},
			{Name: "Plomería Hernández", ServiceType: ptr("Plomería"), ContactName: ptr("Don Rigoberto Hernández"), Phone: ptr("+502 4410 2271")},
			{Name: "Electro Servicios Quetzal", ServiceType: ptr("Electricidad"), Phone: ptr("+502 2290 7765"), Email: ptr("citas@electroquetzal.gt")},
			{Name: "Piscinas y Bombas Atitlán", ServiceType: ptr("Piscinas y bombas"), ContactName: ptr("Mario Sicán"), Phone: ptr("+502 3307 5512")},
		},
		assets: []demoAsset{
			{key: "elev-tb", building: "Torre Botánica", category: "elevator", name: "Elevador A", brand: "Schindler", model: "3300", location: "Lobby, torre norte", vendor: "Elevadores del Valle", installed: "2019-06-01"},
			{key: "cctv-tb", building: "Torre Botánica", category: "camera", name: "Cámaras del lobby", brand: "Hikvision", location: "Lobby y parqueo", installed: "2022-02-14"},
			{key: "cist-tb", building: "Torre Botánica", category: "water", name: "Cisterna y bomba", location: "Sótano 1", vendor: "Plomería Hernández", installed: "2019-06-01"},
			{key: "ac-504", property: "TB-504", category: "hvac", name: "A/C de la sala", brand: "LG", model: "Dual Inverter 18k", location: "Sala", vendor: "Fríos del Sur", status: "needs_attention", installed: "2023-04-10"},
			{key: "cal-504", property: "TB-504", category: "water", name: "Calentador de agua", brand: "Rheem", location: "Lavandería", installed: "2021-09-01"},
			{key: "ref-504", property: "TB-504", category: "appliance", name: "Refrigeradora", brand: "Mabe", location: "Cocina", installed: "2022-11-20"},
			{key: "ac-1102", property: "TB-1102", category: "hvac", name: "A/C principal", brand: "Carrier", location: "Dormitorio principal", vendor: "Fríos del Sur", installed: "2024-01-15"},
			{key: "planta-cy", building: "Residencial Cayalá", category: "electrical", name: "Planta eléctrica", brand: "Cummins", location: "Cuarto de máquinas", vendor: "Electro Servicios Quetzal", installed: "2020-03-01"},
			{key: "jac-ph1", property: "CY-PH1", category: "hospitality", name: "Jacuzzi de la terraza", location: "Terraza", installed: "2022-08-05"},
			{key: "bomba-seb", property: "AN-SEB", category: "plumbing", name: "Bomba de agua", location: "Patio trasero", vendor: "Plomería Hernández", installed: "2018-05-12"},
			{key: "pisc-ply", property: "MT-PLY", category: "water", name: "Bomba de la piscina", brand: "Pentair", location: "Cuarto de bombas", vendor: "Piscinas y Bombas Atitlán", status: "out_of_service", installed: "2021-12-01"},
			{key: "solar-vis", property: "AT-VIS", category: "electrical", name: "Paneles solares", location: "Techo", installed: "2023-07-01"},
		},
		plans: []demoPlan{
			{asset: "elev-tb", name: "Mantenimiento mensual", vendor: "Elevadores del Valle", responsible: "luis.torres@garrent.gt", freq: 30, dueIn: 4, lead: 7},
			{asset: "cist-tb", name: "Limpieza de cisterna", vendor: "Plomería Hernández", freq: 180, dueIn: -6, lead: 10},
			{asset: "ac-504", name: "Limpieza de filtros", vendor: "Fríos del Sur", freq: 90, dueIn: 20, lead: 7},
			{asset: "ac-1102", name: "Limpieza de filtros", vendor: "Fríos del Sur", freq: 90, dueIn: 33, lead: 7},
			{asset: "planta-cy", name: "Prueba de arranque", responsible: "diego.castillo@garrent.gt", freq: 30, dueIn: 12, lead: 5},
			{asset: "pisc-ply", name: "Revisión trimestral", vendor: "Piscinas y Bombas Atitlán", freq: 90, dueIn: 45, lead: 10},
			{asset: "cctv-tb", name: "Revisión de grabaciones", responsible: "luis.torres@garrent.gt", freq: 60, dueIn: 26, lead: 7},
		},
		tickets: []demoTicket{
			// Historial resuelto (indicadores de seis meses).
			{reporter: "maria.ramos@garrent.gt", property: "TB-207", title: "Fuga en el lavamanos del baño", urgency: "medium", assignee: "luis.torres@garrent.gt", state: "closed", resolution: "Se cambió el empaque del sifón.", cost: 180, ageDays: 150, hours: 20},
			{reporter: "sofia.mendez@garrent.gt", property: "TB-1102", asset: "ac-1102", title: "El A/C no enfría", urgency: "high", vendor: "Fríos del Sur", state: "closed", resolution: "Recarga de gas y limpieza.", cost: 950, ageDays: 122, hours: 30},
			{reporter: "ricardo.castillo@garrent.gt", property: "AN-SEB", asset: "bomba-seb", title: "Poca presión en la regadera", urgency: "medium", vendor: "Plomería Hernández", state: "closed", resolution: "Se limpió el filtro de la bomba.", cost: 350, ageDays: 96, hours: 52},
			{reporter: "maria.ramos@garrent.gt", property: "CY-PH1", asset: "jac-ph1", title: "El jacuzzi no calienta", urgency: "high", assignee: "luis.torres@garrent.gt", state: "closed", resolution: "Resistencia nueva.", cost: 1450, ageDays: 70, hours: 26},
			{reporter: "diego.castillo@garrent.gt", property: "AT-VIS", asset: "solar-vis", title: "Inversor de los paneles marca error", urgency: "medium", vendor: "Electro Servicios Quetzal", state: "resolved", resolution: "Se reinició y actualizó el inversor.", cost: 600, ageDays: 41, hours: 14},
			{reporter: "maria.ramos@garrent.gt", property: "TB-504", asset: "ref-504", title: "La refrigeradora hace ruido", urgency: "low", assignee: "luis.torres@garrent.gt", state: "closed", resolution: "Se niveló y se limpió el condensador.", cost: 0, ageDays: 18, hours: 9},
			{reporter: "sofia.mendez@garrent.gt", property: "AN-CAR", title: "Chapa de la puerta principal floja", urgency: "low", assignee: "diego.castillo@garrent.gt", state: "resolved", resolution: "Tornillos nuevos.", cost: 45, ageDays: 6, hours: 5},
			// Abiertas hoy.
			{reporter: "maria.ramos@garrent.gt", property: "TB-504", asset: "ac-504", title: "El A/C de la sala gotea", description: "Cae agua sobre el sillón cuando está encendido.", urgency: "high", assignee: "luis.torres@garrent.gt", vendor: "Fríos del Sur", state: "in_progress"},
			{reporter: "luis.torres@garrent.gt", property: "MT-PLY", asset: "pisc-ply", title: "La bomba de la piscina no arranca", description: "Huéspedes llegan el viernes. Revisar breaker y capacitor.", urgency: "critical", vendor: "Piscinas y Bombas Atitlán", state: "assigned"},
			{reporter: "sofia.mendez@garrent.gt", property: "TB-1102", title: "Luz del pasillo fundida", urgency: "low", state: "open"},
			{reporter: "ricardo.castillo@garrent.gt", property: "AN-SEB", title: "La puerta del patio no cierra bien", description: "Hay que empujarla para que llegue el pestillo.", urgency: "medium", state: "open"},
			{reporter: "maria.ramos@garrent.gt", property: "CY-08C", title: "Mancha de humedad en el techo del baño", urgency: "medium", assignee: "diego.castillo@garrent.gt", state: "assigned"},
		},
	},
	"costa_limpia": {
		vendors: []maintenance.VendorCreate{
			{Name: "Mantenimientos Costa Sur", ServiceType: ptr("Mantenimiento general"), ContactName: ptr("Julio Pérez"), Phone: ptr("+502 7881 4402")},
			{Name: "Refrigeración Escuintla", ServiceType: ptr("Aire acondicionado"), Phone: ptr("+502 7888 1190")},
		},
		assets: []demoAsset{
			{key: "pisc-mb", building: "Condominio Marbella", category: "water", name: "Bomba de la piscina comunitaria", location: "Área de piscina", vendor: "Mantenimientos Costa Sur", installed: "2020-01-10"},
			{key: "ac-3b", property: "MB-3B", category: "hvac", name: "A/C del dormitorio", brand: "Samsung", vendor: "Refrigeración Escuintla", status: "out_of_service", installed: "2022-05-01"},
			{key: "cal-sol", property: "MT-SOL", category: "water", name: "Calentador", location: "Cocina", installed: "2021-03-15"},
		},
		plans: []demoPlan{
			{asset: "pisc-mb", name: "Revisión mensual", vendor: "Mantenimientos Costa Sur", freq: 30, dueIn: 2, lead: 5},
			{asset: "ac-3b", name: "Limpieza de filtros", vendor: "Refrigeración Escuintla", freq: 90, dueIn: 60, lead: 7},
		},
		tickets: []demoTicket{
			{reporter: "rosa.xol@costalimpia.gt", property: "MT-SOL", asset: "cal-sol", title: "No hay agua caliente", urgency: "high", vendor: "Mantenimientos Costa Sur", state: "closed", resolution: "Termostato nuevo.", cost: 420, ageDays: 64, hours: 22},
			{reporter: "pedro.aju@costalimpia.gt", property: "MB-3B", asset: "ac-3b", title: "El A/C del dormitorio no enciende", urgency: "critical", vendor: "Refrigeración Escuintla", state: "assigned"},
			{reporter: "rosa.xol@costalimpia.gt", property: "MB-5A", title: "Cortina del balcón rota", urgency: "low", state: "open"},
		},
	},
}

// seedMaintenance siembra la fase 3 en cada empresa de demostracion.
func seedMaintenance(ctx context.Context, services app.Services, db *database.DB, ids map[string]tenants.Tenant) error {
	for slug, data := range demoMaintenanceData {
		t := ids[slug]
		admin, err := services.Auth.UserByEmail(ctx, demoOperations[slug].admin)
		if err != nil {
			return err
		}
		s := maintSeeder{
			services: services, db: db, ctx: database.WithTenantSchema(ctx, t.SchemaName), tenant: t,
			admin:  authz.Actor{UserID: admin.ID, TenantID: t.ID, Permissions: allPermissions()},
			assets: map[string]uuid.UUID{}, vendors: map[string]uuid.UUID{},
		}
		if err := s.run(data); err != nil {
			return fmt.Errorf("mantenimiento de %s: %w", slug, err)
		}
	}
	fmt.Println("Proveedores, activos, preventivos e incidencias de demostracion listos.")
	return nil
}

type maintSeeder struct {
	services app.Services
	db       *database.DB
	ctx      context.Context
	tenant   tenants.Tenant
	admin    authz.Actor
	assets   map[string]uuid.UUID
	vendors  map[string]uuid.UUID
	props    map[string]uuid.UUID
}

func (s *maintSeeder) run(data demoMaintenance) error {
	existing, err := s.services.Maintenance.Vendors(s.ctx, nil)
	if err != nil || len(existing) > 0 {
		return err
	}
	if err := s.loadPlaces(); err != nil {
		return err
	}
	for _, v := range data.vendors {
		created, err := s.services.Maintenance.CreateVendor(s.ctx, s.admin, v)
		if err != nil {
			return err
		}
		s.vendors[v.Name] = created.ID
	}
	for _, a := range data.assets {
		if err := s.asset(a); err != nil {
			return fmt.Errorf("activo %s: %w", a.name, err)
		}
	}
	for _, p := range data.plans {
		if err := s.plan(p); err != nil {
			return fmt.Errorf("preventivo %s: %w", p.name, err)
		}
	}
	for _, t := range data.tickets {
		if err := s.ticket(t); err != nil {
			return fmt.Errorf("incidencia %q: %w", t.title, err)
		}
	}
	// Los preventivos que ya entraron en su anticipacion generan su ticket.
	return s.services.Maintenance.HandleJob(s.ctx, jobsRunningFor(maintenance.JobMaintenanceDue, s.tenant.ID))
}

// loadPlaces carga los ids de propiedades y edificios por codigo y nombre.
func (s *maintSeeder) loadPlaces() error {
	s.props = map[string]uuid.UUID{}
	list, _, err := s.services.Properties.List(s.ctx, s.admin, properties.ListFilter{Limit: 500})
	if err != nil {
		return err
	}
	for _, p := range list {
		s.props[p.Code] = p.ID
	}
	buildings, err := s.services.Properties.Buildings(s.ctx, s.admin)
	if err != nil {
		return err
	}
	for _, b := range buildings {
		s.props["building:"+b.Name] = b.ID
	}
	return nil
}

func (s *maintSeeder) asset(a demoAsset) error {
	in := maintenance.AssetCreate{
		Category: a.category, Name: a.name, Brand: optStr(a.brand), Model: optStr(a.model), Location: optStr(a.location),
		InstalledAt: optStr(a.installed), Status: a.status,
	}
	if a.installed != "" {
		in.WarrantyUntil = ptr(addYears(a.installed, 3))
	}
	if a.property != "" {
		in.PropertyID = ptr(s.props[a.property])
	} else {
		in.BuildingID = ptr(s.props["building:"+a.building])
	}
	if a.vendor != "" {
		in.VendorID = ptr(s.vendors[a.vendor])
	}
	created, err := s.services.Maintenance.CreateAsset(s.ctx, s.admin, in)
	if err != nil {
		return err
	}
	s.assets[a.key] = created.ID
	return nil
}

func (s *maintSeeder) plan(p demoPlan) error {
	in := maintenance.PlanCreate{
		Name: p.name, FrequencyDays: p.freq, LeadDays: ptr(p.lead),
		NextDueAt: ptr(time.Now().AddDate(0, 0, p.dueIn).Format(time.DateOnly)),
	}
	if p.vendor != "" {
		in.VendorID = ptr(s.vendors[p.vendor])
	}
	if p.responsible != "" {
		u, err := s.services.Auth.UserByEmail(s.ctx, p.responsible)
		if err != nil {
			return err
		}
		in.ResponsibleID = &u.ID
	}
	_, err := s.services.Maintenance.CreatePlan(s.ctx, s.admin, s.assets[p.asset], in)
	return err
}

func (s *maintSeeder) ticket(t demoTicket) error {
	reporter, err := s.services.Auth.UserByEmail(s.ctx, t.reporter)
	if err != nil {
		return err
	}
	who := s.admin
	who.UserID = reporter.ID
	in := maintenance.TicketCreate{
		PropertyID: s.props[t.property], Title: t.title, Description: t.description, Urgency: t.urgency,
	}
	if t.asset != "" {
		in.AssetID = ptr(s.assets[t.asset])
	}
	created, err := s.services.Maintenance.CreateTicket(s.ctx, who, in)
	if err != nil {
		return err
	}
	id := created.ID
	if t.state == "open" {
		return nil
	}
	assign := maintenance.TicketAssign{}
	if t.assignee != "" {
		u, err := s.services.Auth.UserByEmail(s.ctx, t.assignee)
		if err != nil {
			return err
		}
		assign.AssigneeID = &u.ID
	}
	if t.vendor != "" {
		assign.VendorID = ptr(s.vendors[t.vendor])
	}
	if _, err := s.services.Maintenance.Assign(s.ctx, s.admin, id, assign); err != nil {
		return err
	}
	switch t.state {
	case "in_progress":
		_, err = s.services.Maintenance.Start(s.ctx, s.admin, id)
	case "resolved", "closed":
		err = s.finish(id, t)
	}
	return err
}

// finish resuelve (y cierra) una incidencia del historial y la fecha hacia atras.
func (s *maintSeeder) finish(id uuid.UUID, t demoTicket) error {
	if _, err := s.services.Maintenance.Resolve(s.ctx, s.admin, id, maintenance.TicketResolve{
		Resolution: t.resolution, Cost: &t.cost, AssetOperational: false,
	}); err != nil {
		return err
	}
	if t.state == "closed" {
		if _, err := s.services.Maintenance.Close(s.ctx, s.admin, id, maintenance.TicketReason{}); err != nil {
			return err
		}
	}
	reported := time.Now().AddDate(0, 0, -t.ageDays)
	resolved := reported.Add(time.Duration(t.hours) * time.Hour)
	return s.db.InTx(s.ctx, func(q database.Querier) error {
		if _, err := q.Exec(s.ctx, `
			UPDATE maintenance_tickets SET created_at = $2, resolved_at = $3,
			       closed_at = CASE WHEN closed_at IS NOT NULL THEN $3::timestamptz + interval '1 day' END
			WHERE id = $1`, id, reported, resolved); err != nil {
			return err
		}
		_, err := q.Exec(s.ctx, `
			UPDATE ticket_events SET created_at = CASE type
			    WHEN 'created' THEN $2::timestamptz
			    WHEN 'assigned' THEN $2::timestamptz + interval '1 hour'
			    WHEN 'resolved' THEN $3::timestamptz
			    ELSE $3::timestamptz + interval '1 day' END
			WHERE ticket_id = $1`, id, reported, resolved)
		return err
	})
}

func optStr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func addYears(d string, n int) string {
	t, _ := time.Parse(time.DateOnly, d)
	return t.AddDate(n, 0, 0).Format(time.DateOnly)
}

// jobsRunningFor es un trabajo de una empresa, para correrlo sin el worker.
func jobsRunningFor(jobType string, tenantID uuid.UUID) jobs.Running {
	return jobs.Running{ID: uuid.New(), Type: jobType, TenantID: &tenantID, Attempt: 1}
}
