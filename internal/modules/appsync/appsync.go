// Package appsync es el modulo "sync" de fase02.md §5: lo que la app guarda en
// Drift para trabajar sin conexion. GET /sync/bootstrap devuelve, en una sola
// respuesta y ya filtrado por el alcance de la persona, sus propiedades (sin
// la informacion de acceso), sus edificios, las plantillas de checklist que
// usan, los activos donde se puede reportar una incidencia y su horario.
//
// Las tareas se sincronizan aparte con GET /tasks?from=&to=&updated_since=
// (modulo tasks, Persona B). Persona B agrega aqui `inventory_items` cuando
// exista su modulo.
package appsync

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/checklists"
	"github.com/smorareqs/argos-service/internal/modules/employees"
	"github.com/smorareqs/argos-service/internal/modules/inventory"
	"github.com/smorareqs/argos-service/internal/modules/maintenance"
	"github.com/smorareqs/argos-service/internal/modules/properties"
)

// maxProperties es el tope de propiedades que viajan en el bootstrap.
const maxProperties = 5000

// Bootstrap es la foto inicial para la app.
type Bootstrap struct {
	ServerTime         time.Time                 `json:"server_time" doc:"Hora del servidor (para el reloj local y updated_since)."`
	Scoped             bool                      `json:"scoped" doc:"true = solo trae sus propiedades asignadas o propias."`
	Properties         []properties.PropertyRead `json:"properties"`
	Buildings          []properties.BuildingRead `json:"buildings"`
	ChecklistTemplates []checklists.TemplateRead `json:"checklist_templates"`
	Schedule           []employees.ScheduleBlock `json:"schedule" doc:"Horario semanal propio (vacio si no tiene ficha laboral)."`
	Assets             []maintenance.AssetRef    `json:"assets" doc:"Activos del alcance para reportar incidencias (fase 3; requiere maintenance.report o maintenance.read)."`
	// InventoryItems son los articulos activos (Persona B; requiere inventory.read).
	InventoryItems []inventory.InventoryItem `json:"inventory_items" doc:"Articulos activos del inventario (requiere inventory.read)."`
}

// Service arma el bootstrap.
type Service struct {
	db    *database.DB
	props *properties.Service
	log   *slog.Logger
	now   func() time.Time
}

// NewService crea el servicio.
func NewService(db *database.DB, props *properties.Service, log *slog.Logger) *Service {
	return &Service{db: db, props: props, log: log, now: time.Now}
}

// Bootstrap devuelve la foto de la persona. Sin properties.read, las listas
// de propiedades, edificios y plantillas vienen vacias.
func (s *Service) Bootstrap(ctx context.Context, actor authz.Actor) (Bootstrap, error) {
	out := Bootstrap{
		ServerTime: s.now().UTC(), Scoped: actor.Scoped,
		Properties: []properties.PropertyRead{}, Buildings: []properties.BuildingRead{},
		ChecklistTemplates: []checklists.TemplateRead{}, Schedule: []employees.ScheduleBlock{},
		Assets: []maintenance.AssetRef{}, InventoryItems: []inventory.InventoryItem{},
	}
	if actor.Permissions[authz.PropertiesRead] {
		if err := s.catalog(ctx, actor, &out); err != nil {
			return Bootstrap{}, err
		}
	}
	err := s.db.InTx(ctx, func(q database.Querier) error {
		schedule, err := ownSchedule(ctx, q, actor.UserID)
		if err != nil {
			return err
		}
		out.Schedule = schedule
		if actor.Permissions[authz.InventoryRead] {
			if out.InventoryItems, err = inventory.ActiveItems(ctx, q); err != nil {
				return err
			}
		}
		if !actor.Permissions[authz.MaintenanceReport] && !actor.Permissions[authz.MaintenanceRead] {
			return nil
		}
		scope, err := authz.ResolvePropertyScope(ctx, q, actor)
		if err != nil {
			return err
		}
		out.Assets, err = maintenance.AssetRefs(ctx, q, scope)
		return err
	})
	return out, err
}

func (s *Service) catalog(ctx context.Context, actor authz.Actor, out *Bootstrap) error {
	cursor := ""
	for len(out.Properties) < maxProperties {
		page, next, err := s.props.List(ctx, actor, properties.ListFilter{Cursor: cursor, Limit: 500})
		if err != nil {
			return err
		}
		out.Properties = append(out.Properties, page...)
		if next == nil {
			break
		}
		cursor = *next
	}
	buildings, err := s.props.Buildings(ctx, actor)
	if err != nil {
		return err
	}
	out.Buildings = buildings
	ids := []uuid.UUID{}
	for _, p := range out.Properties {
		if p.ChecklistTemplateID != nil && !slices.Contains(ids, *p.ChecklistTemplateID) {
			ids = append(ids, *p.ChecklistTemplateID)
		}
	}
	templates, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]checklists.TemplateRead, error) {
		return checklists.List(ctx, q, ids)
	})
	if err != nil {
		return err
	}
	out.ChecklistTemplates = templates
	return nil
}

func ownSchedule(ctx context.Context, q database.Querier, userID uuid.UUID) ([]employees.ScheduleBlock, error) {
	rows, err := q.Query(ctx, `
		SELECT s.weekday::int AS weekday, to_char(s.starts_at, 'HH24:MI') AS starts_at, to_char(s.ends_at, 'HH24:MI') AS ends_at
		FROM employee_schedules s JOIN employees e ON e.id = s.employee_id
		WHERE e.user_id = $1 ORDER BY s.weekday, s.starts_at`, userID)
	if err != nil {
		return nil, err
	}
	type row struct {
		Weekday  int    `db:"weekday"`
		StartsAt string `db:"starts_at"`
		EndsAt   string `db:"ends_at"`
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[row])
	if err != nil {
		return nil, err
	}
	out := make([]employees.ScheduleBlock, 0, len(list))
	for _, r := range list {
		out = append(out, employees.ScheduleBlock{Weekday: r.Weekday, StartsAt: r.StartsAt, EndsAt: r.EndsAt})
	}
	return out, nil
}
