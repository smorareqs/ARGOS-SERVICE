// Package dashboard arma el Inicio de la web (W-B1, GET /dashboard): un solo
// agregado con lo que pide atencion hoy. Cada bloque respeta el alcance y los
// permisos de quien consulta; si no tiene el permiso, el bloque viene vacio.
//
// Las limpiezas del dia (tareas, Persona B) entran por TasksSource; mientras
// no se registre, tasks.available es false y la web muestra su aviso.
package dashboard

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/alerts"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/maintenance"
	"github.com/smorareqs/argos-service/internal/modules/reservations"
)

// RankRow es una persona en el ranking del dia.
type RankRow struct {
	UserID   uuid.UUID `json:"user_id" format:"uuid"`
	FullName string    `json:"full_name"`
	Done     int       `json:"done"`
	Total    int       `json:"total"`
	Score    *float64  `json:"score" nullable:"true" doc:"Calificacion promedio de sus validaciones (0 a 5)."`
}

// Tasks es el bloque de limpiezas del dia. Lo llena el modulo de tareas.
type Tasks struct {
	Available bool `json:"available" doc:"false mientras el modulo de tareas no este conectado."`
	Done      int  `json:"done"`
	Total     int  `json:"total"`
	InProcess int  `json:"in_progress"`
	Late      int  `json:"late"`
	Pending   int  `json:"pending"`
	// InField son las personas con una tarea iniciada ahora.
	InField int       `json:"in_field"`
	Ranking []RankRow `json:"ranking"`
	// Status es el estado de hoy por propiedad (ready, cleaning, pending, late).
	Status map[uuid.UUID]string `json:"-"`
}

// TasksSource calcula el bloque de tareas de hoy (fecha AAAA-MM-DD de la
// empresa) con la transaccion del Inicio.
type TasksSource func(ctx context.Context, q database.Querier, actor authz.Actor, scope authz.PropertyScope, today string) (Tasks, error)

// Incidents son los contadores de incidencias abiertas.
type Incidents struct {
	Open       int `json:"open"`
	Urgent     int `json:"urgent" doc:"Urgencia alta o critica."`
	Unassigned int `json:"unassigned"`
}

// Pin es una propiedad en el mapa del Inicio.
type Pin struct {
	ID          uuid.UUID `db:"id" json:"id" format:"uuid"`
	Code        string    `db:"code" json:"code"`
	Name        string    `db:"name" json:"name"`
	Type        string    `db:"type" json:"type" enum:"apartment,house,room,other" doc:"Tipo de propiedad (icono del pin)."`
	Building    *string   `db:"building" json:"building" nullable:"true" doc:"Nombre del edificio, si pertenece a uno."`
	Zone        *string   `db:"zone" json:"zone" nullable:"true"`
	Lat         *float64  `db:"lat" json:"lat" nullable:"true" doc:"La de la propiedad o la de su edificio."`
	Lng         *float64  `db:"lng" json:"lng" nullable:"true"`
	OpenTickets int       `db:"open_tickets" json:"open_tickets"`
	Status      string    `db:"-" json:"status" enum:"incident,cleaning,late,pending,occupied,ready" doc:"incident si tiene incidencias abiertas; cleaning, late y pending vienen de tareas; occupied si hay huesped en casa (reservas)."`
}

// Dashboard es la respuesta de GET /dashboard.
type Dashboard struct {
	Date      string                 `json:"date" format:"date" doc:"Hoy en la zona horaria de la empresa."`
	Alerts    alerts.Summary         `json:"alerts"`
	TopAlerts []alerts.AlertRead     `json:"top_alerts" doc:"Hasta 3: severidad alta primero."`
	Incidents Incidents              `json:"incidents"`
	Upcoming  []maintenance.PlanRead `json:"upcoming" doc:"Preventivos de los proximos 30 dias (incluye vencidos), hasta 5."`
	Pins      []Pin                  `json:"pins" doc:"Propiedades activas del alcance."`
	Activity  []audit.EntryRead      `json:"activity" doc:"Los 8 cambios mas recientes (requiere audit.read)."`
	Tasks     Tasks                  `json:"tasks"`
	// Reservations: llegadas, salidas y estancias de hoy (fase 4). available
	// es false sin reservations.read o sin la bandera reservations.
	Reservations reservations.Today `json:"reservations"`
}

// Service arma el Inicio.
type Service struct {
	db          *database.DB
	maintenance *maintenance.Service
	log         *slog.Logger
	tasks       TasksSource
	now         func() time.Time
}

// NewService crea el servicio.
func NewService(db *database.DB, m *maintenance.Service, log *slog.Logger) *Service {
	return &Service{db: db, maintenance: m, log: log, now: time.Now}
}

// SetTasksSource registra el bloque de limpiezas del modulo de tareas.
func (s *Service) SetTasksSource(fn TasksSource) {
	s.tasks = fn
}

// Get arma el Inicio de quien consulta.
func (s *Service) Get(ctx context.Context, actor authz.Actor) (Dashboard, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Dashboard, error) {
		scope, err := authz.ResolvePropertyScope(ctx, q, actor)
		if err != nil {
			return Dashboard{}, err
		}
		d := Dashboard{
			TopAlerts: []alerts.AlertRead{}, Upcoming: []maintenance.PlanRead{}, Pins: []Pin{},
			Activity: []audit.EntryRead{}, Alerts: alerts.Summary{ByType: []alerts.TypeCount{}},
			Tasks: Tasks{Ranking: []RankRow{}},
		}
		if err := q.QueryRow(ctx, `
			SELECT to_char(($1::timestamptz AT TIME ZONE timezone)::date, 'YYYY-MM-DD')
			FROM shared.tenants WHERE id = $2`, s.now(), actor.TenantID).Scan(&d.Date); err != nil {
			return Dashboard{}, err
		}
		if err := s.fill(ctx, q, actor, scope, &d); err != nil {
			return Dashboard{}, err
		}
		return d, nil
	})
}

func (s *Service) fill(ctx context.Context, q database.Querier, actor authz.Actor, scope authz.PropertyScope, d *Dashboard) error {
	var err error
	can := actor.Permissions
	if can[authz.AlertsRead] {
		if err := fillAlerts(ctx, q, scope, d); err != nil {
			return err
		}
	}
	if can[authz.MaintenanceRead] {
		if err := s.fillMaintenance(ctx, q, actor, scope, d); err != nil {
			return err
		}
	}
	if can[authz.AuditRead] {
		if d.Activity, _, err = audit.List(ctx, q, actor.TenantID, audit.Filter{Limit: 8}); err != nil {
			return err
		}
	}
	if err := s.fillTasks(ctx, q, actor, scope, d); err != nil {
		return err
	}
	occupied, err := s.fillReservations(ctx, q, actor, scope, d)
	if err != nil {
		return err
	}
	if can[authz.PropertiesRead] {
		if d.Pins, err = pins(ctx, q, scope, d.Tasks.Status, occupied); err != nil {
			return err
		}
	}
	return nil
}

func fillAlerts(ctx context.Context, q database.Querier, scope authz.PropertyScope, d *Dashboard) error {
	var err error
	if d.Alerts, err = alerts.Summarize(ctx, q, scope); err != nil {
		return err
	}
	d.TopAlerts, err = alerts.Top(ctx, q, scope, 3)
	return err
}

func (s *Service) fillMaintenance(ctx context.Context, q database.Querier, actor authz.Actor, scope authz.PropertyScope, d *Dashboard) error {
	var err error
	if d.Incidents.Open, d.Incidents.Urgent, d.Incidents.Unassigned, err = maintenance.IncidentCounts(ctx, q, scope, actor); err != nil {
		return err
	}
	d.Upcoming, err = s.maintenance.Upcoming(ctx, q, scope, 30, 5)
	return err
}

func (s *Service) fillTasks(ctx context.Context, q database.Querier, actor authz.Actor, scope authz.PropertyScope, d *Dashboard) error {
	if s.tasks == nil {
		return nil
	}
	t, err := s.tasks(ctx, q, actor, scope, d.Date)
	if err != nil {
		return err
	}
	t.Available = true
	if t.Ranking == nil {
		t.Ranking = []RankRow{}
	}
	d.Tasks = t
	return nil
}

// fillReservations llena el bloque de reservas y devuelve las propiedades con
// huesped en casa (vacio sin permiso o sin la bandera).
func (s *Service) fillReservations(ctx context.Context, q database.Querier, actor authz.Actor, scope authz.PropertyScope, d *Dashboard) (map[uuid.UUID]bool, error) {
	if !actor.Permissions[authz.ReservationsRead] {
		return nil, nil
	}
	var on bool
	if err := q.QueryRow(ctx, `SELECT coalesce((features->>'reservations')::bool, false) FROM shared.tenants WHERE id = $1`,
		actor.TenantID).Scan(&on); err != nil || !on {
		return nil, err
	}
	var err error
	if d.Reservations, err = reservations.TodaySummary(ctx, q, scope, d.Date); err != nil {
		return nil, err
	}
	return reservations.OccupiedToday(ctx, q, d.Date)
}

func pins(ctx context.Context, q database.Querier, scope authz.PropertyScope, status map[uuid.UUID]string, occupied map[uuid.UUID]bool) ([]Pin, error) {
	all, ids := scope.SQLArgs()
	rows, err := q.Query(ctx, `
		SELECT p.id, p.code, p.name, p.type, b.name AS building, coalesce(p.zone, b.zone) AS zone,
		       coalesce(p.lat, b.lat)::float8 AS lat, coalesce(p.lng, b.lng)::float8 AS lng,
		       (SELECT count(*)::int FROM maintenance_tickets t
		         WHERE t.property_id = p.id AND t.status IN ('open', 'assigned', 'in_progress')) AS open_tickets
		FROM properties p LEFT JOIN buildings b ON b.id = p.building_id
		WHERE p.deleted_at IS NULL AND p.is_active AND ($1::bool OR p.id = ANY($2::uuid[]))
		ORDER BY upper(p.code)`, all, ids)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[Pin])
	if err != nil {
		return nil, err
	}
	for i := range list {
		switch {
		case list[i].OpenTickets > 0:
			list[i].Status = "incident"
		case status[list[i].ID] != "" && status[list[i].ID] != "ready":
			list[i].Status = status[list[i].ID]
		case occupied[list[i].ID]:
			list[i].Status = "occupied"
		default:
			list[i].Status = "ready"
		}
	}
	return list, nil
}
