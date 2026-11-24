package properties

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/optional"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Edificios (W-C3). Quien solo ve sus propiedades ve los edificios donde
// tiene alguna.

const selectBuilding = `SELECT b.id, b.name, b.address, b.zone, b.lat::float8 AS lat, b.lng::float8 AS lng,
       b.notes, b.is_active, b.created_at, b.updated_at,
       (SELECT count(*) FROM properties p WHERE p.building_id = b.id AND p.deleted_at IS NULL)::int AS properties_count
FROM buildings b`

// Buildings devuelve los edificios del alcance, por nombre.
func (s *Service) Buildings(ctx context.Context, actor authz.Actor) ([]BuildingRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]BuildingRead, error) {
		scope, err := authz.ResolvePropertyScope(ctx, q, actor)
		if err != nil {
			return nil, err
		}
		all, ids := scope.SQLArgs()
		rows, err := q.Query(ctx, selectBuilding+`
			WHERE $1::bool OR EXISTS (SELECT 1 FROM properties p
			                          WHERE p.building_id = b.id AND p.id = ANY($2::uuid[]) AND p.deleted_at IS NULL)
			ORDER BY lower(b.name)`, all, ids)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[BuildingRead])
	})
}

// Building devuelve un edificio del alcance.
func (s *Service) Building(ctx context.Context, actor authz.Actor, id uuid.UUID) (BuildingRead, error) {
	list, err := s.Buildings(ctx, actor)
	if err != nil {
		return BuildingRead{}, err
	}
	for _, b := range list {
		if b.ID == id {
			return b, nil
		}
	}
	return BuildingRead{}, ErrBuildingNotFound
}

type buildingData struct {
	Name          string
	Address, Zone *string
	Lat, Lng      *float64
	Notes         *string
	IsActive      bool
}

// CreateBuilding da de alta un edificio.
func (s *Service) CreateBuilding(ctx context.Context, actor authz.Actor, in BuildingCreate) (BuildingRead, error) {
	d := buildingData{Name: in.Name, Address: in.Address, Zone: in.Zone, Lat: in.Lat, Lng: in.Lng, Notes: in.Notes, IsActive: true}
	if err := prepareBuilding(&d); err != nil {
		return BuildingRead{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (BuildingRead, error) {
		if err := uniqueBuildingName(ctx, q, d.Name, nil); err != nil {
			return BuildingRead{}, err
		}
		var id uuid.UUID
		if err := q.QueryRow(ctx, `
			INSERT INTO buildings (name, address, zone, lat, lng, notes) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			d.Name, d.Address, d.Zone, d.Lat, d.Lng, d.Notes).Scan(&id); err != nil {
			return BuildingRead{}, err
		}
		b, err := buildingByID(ctx, q, id)
		if err != nil {
			return BuildingRead{}, err
		}
		return b, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "building.created", Entity: "building", EntityID: &id, After: buildingSnapshot(b),
		})
	})
}

// UpdateBuilding cambia los datos que vienen en el cuerpo.
func (s *Service) UpdateBuilding(ctx context.Context, actor authz.Actor, id uuid.UUID, in BuildingUpdate) (BuildingRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (BuildingRead, error) {
		cur, err := buildingByID(ctx, q, id)
		if err != nil {
			return BuildingRead{}, err
		}
		d := buildingData{
			Name: deref(in.Name, cur.Name), IsActive: deref(in.IsActive, cur.IsActive),
			Address: optional.Apply(cur.Address, in.Address), Zone: optional.Apply(cur.Zone, in.Zone),
			Lat: optional.Apply(cur.Lat, in.Lat), Lng: optional.Apply(cur.Lng, in.Lng), Notes: optional.Apply(cur.Notes, in.Notes),
		}
		if err := prepareBuilding(&d); err != nil {
			return BuildingRead{}, err
		}
		if err := uniqueBuildingName(ctx, q, d.Name, &id); err != nil {
			return BuildingRead{}, err
		}
		if _, err := q.Exec(ctx, `
			UPDATE buildings SET name = $2, address = $3, zone = $4, lat = $5, lng = $6, notes = $7, is_active = $8,
			       updated_at = now()
			WHERE id = $1`, id, d.Name, d.Address, d.Zone, d.Lat, d.Lng, d.Notes, d.IsActive); err != nil {
			return BuildingRead{}, err
		}
		b, err := buildingByID(ctx, q, id)
		if err != nil {
			return BuildingRead{}, err
		}
		return b, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "building.updated", Entity: "building", EntityID: &id,
			Before: buildingSnapshot(cur), After: buildingSnapshot(b),
		})
	})
}

// DeleteBuilding borra un edificio sin propiedades.
func (s *Service) DeleteBuilding(ctx context.Context, actor authz.Actor, id uuid.UUID) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		cur, err := buildingByID(ctx, q, id)
		if err != nil {
			return err
		}
		var used bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM properties WHERE building_id = $1)`, id).Scan(&used); err != nil {
			return err
		}
		if used {
			return ErrBuildingInUse
		}
		if _, err := q.Exec(ctx, `DELETE FROM buildings WHERE id = $1`, id); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "building.deleted", Entity: "building", EntityID: &id, Before: buildingSnapshot(cur),
		})
	})
}

func buildingByID(ctx context.Context, q database.Querier, id uuid.UUID) (BuildingRead, error) {
	rows, err := q.Query(ctx, selectBuilding+` WHERE b.id = $1`, id)
	if err != nil {
		return BuildingRead{}, err
	}
	b, err := collectOne[BuildingRead](rows)
	if err != nil {
		return BuildingRead{}, err
	}
	if b == nil {
		return BuildingRead{}, ErrBuildingNotFound
	}
	return *b, nil
}

func prepareBuilding(d *buildingData) error {
	name, err := cleanName(d.Name, "body.name")
	if err != nil {
		return err
	}
	d.Name = name
	d.Address, d.Zone, d.Notes = cleanText(d.Address), cleanText(d.Zone), cleanText(d.Notes)
	if (d.Lat == nil) != (d.Lng == nil) {
		return ErrCoordinates.WithField("body.lat")
	}
	return nil
}

func uniqueBuildingName(ctx context.Context, q database.Querier, name string, except *uuid.UUID) error {
	var taken bool
	if err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM buildings WHERE lower(name) = lower($1) AND ($2::uuid IS NULL OR id <> $2))`,
		name, except).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return ErrBuildingNameTaken.WithField("body.name")
	}
	return nil
}

func buildingSnapshot(b BuildingRead) map[string]any {
	return map[string]any{
		"name": b.Name, "address": b.Address, "zone": b.Zone, "lat": b.Lat, "lng": b.Lng, "notes": b.Notes,
		"is_active": b.IsActive,
	}
}
