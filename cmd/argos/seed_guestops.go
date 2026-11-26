package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/app"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/guestaccess"
	"github.com/smorareqs/argos-service/internal/modules/parking"
	"github.com/smorareqs/argos-service/internal/modules/sharedareas"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
)

// Semilla de la fase 4 de Persona B: espacios de parqueo por edificio (con
// placas para las estancias de hoy), areas comunes y algun bloque reservado.
// Las alarmas de salida ya las programo la confirmacion de cada reserva.

type demoArea struct {
	building, name, opens, closes, rules, photo string
	capacity, block, maxPerDay, advance           int
}

var demoAreas = map[string][]demoArea{
	"garrent": {
		{"Torre Botánica", "Piscina de la terraza", "07:00", "21:00", "Ducha antes de entrar. Niños con un adulto. Sin envases de vidrio.", "beachPool", 12, 60, 2, 2},
		{"Residencial Cayalá", "Salón social", "09:00", "22:00", "Música hasta las 21:30. Se entrega limpio.", "brightRoom", 30, 180, 1, 24},
		{"Hostal Antigua", "Patio colonial", "08:00", "21:00", "Desayunos de 8 a 10 para todos los huéspedes.", "antiguaArch", 14, 90, 2, 2},
	},
	"costa_limpia": {
		{"Condominio Marbella", "Piscina Marbella", "08:00", "20:00", "Niños con un adulto. Sin envases de vidrio.", "beachPool", 16, 120, 2, 2},
	},
}

// demoGuestCodes es el codigo fijo de entrada a la app de un huesped en casa
// por empresa (avance.md: datos de prueba).
var demoGuestCodes = map[string]string{"garrent": "HUESPED-GAR", "costa_limpia": "HUESPED-CST"}

var demoPlates = []string{"P512KHT", "P238BDF", "P904LMN", "P117RZS", "P660WXC", "P381JTV"}

func seedGuestOps(ctx context.Context, services app.Services, ids map[string]tenants.Tenant) error {
	for slug, t := range ids {
		ops, ok := demoOperations[slug]
		if !ok {
			continue
		}
		admin, err := services.Auth.UserByEmail(ctx, ops.admin)
		if err != nil {
			return err
		}
		tctx := database.WithTenantSchema(ctx, t.SchemaName)
		actor := authz.Actor{UserID: admin.ID, TenantID: t.ID, Permissions: allPermissions()}
		if err := seedGuestOpsTenant(tctx, services, actor, slug); err != nil {
			return fmt.Errorf("parqueos y áreas de %s: %w", slug, err)
		}
		if err := seedGuestCode(tctx, services, slug); err != nil {
			return fmt.Errorf("acceso de huésped de %s: %w", slug, err)
		}
	}
	fmt.Println("Parqueos, áreas comunes y alarmas de salida de demostracion listos.")
	return nil
}

func seedGuestOpsTenant(ctx context.Context, services app.Services, actor authz.Actor, slug string) error {
	existing, err := services.Parking.Spaces(ctx, true)
	if err != nil || len(existing) > 0 {
		return err
	}
	type building struct {
		ID   uuid.UUID
		Name string
	}
	var buildings []building
	if err := services.Parking.DB().InTx(ctx, func(q database.Querier) error {
		rows, err := q.Query(ctx, `SELECT id, name FROM buildings ORDER BY name`)
		if err != nil {
			return err
		}
		buildings, err = pgx.CollectRows(rows, pgx.RowToStructByPos[building])
		return err
	}); err != nil {
		return err
	}
	byName := map[string]uuid.UUID{}
	for bi, b := range buildings {
		byName[b.Name] = b.ID
		level := []string{"Sótano 1", "Sótano 2", "Exterior"}[bi%3]
		for i := 1; i <= 8; i++ {
			kind := "guest"
			if i == 1 {
				kind = "staff"
			} else if i >= 7 {
				kind = "visitor"
			}
			id := b.ID
			if _, err := services.Parking.CreateSpace(ctx, actor, parking.SpaceCreate{
				BuildingID: &id, Code: fmt.Sprintf("%c-%02d", []rune(b.Name)[0], i), Level: level, Kind: kind, Covered: level != "Exterior",
			}); err != nil {
				return err
			}
		}
	}
	// Placas para las estancias que tienen edificio, en orden.
	stays, err := services.Parking.Stays(ctx)
	if err != nil {
		return err
	}
	spaces, err := services.Parking.Spaces(ctx, false)
	if err != nil {
		return err
	}
	used := map[uuid.UUID]bool{}
	plate := 0
	for _, st := range stays {
		if plate >= len(demoPlates) {
			break
		}
		for _, sp := range spaces {
			if used[sp.ID] || sp.Kind != "guest" || sp.Building != st.Building {
				continue
			}
			if _, err := services.Parking.Assign(ctx, actor, parking.AssignmentCreate{
				SpaceID: sp.ID, ReservationCode: st.ReservationCode, Plate: demoPlates[plate],
			}); err != nil {
				return err
			}
			used[sp.ID] = true
			plate++
			break
		}
	}
	for _, a := range demoAreas[slug] {
		bid, ok := byName[a.building]
		if !ok {
			continue
		}
		if _, err := services.SharedAreas.CreateArea(ctx, actor, sharedareas.AreaWrite{
			BuildingID: &bid, Name: a.name, Capacity: a.capacity, Opens: a.opens, Closes: a.closes, Block: a.block,
			MaxPerDay: a.maxPerDay, AdvanceHours: a.advance, Rules: a.rules, Photo: a.photo,
		}); err != nil {
			return err
		}
	}
	// Un bloque manana para cada huesped en casa cuyo edificio tiene area.
	guests, err := services.SharedAreas.InHouse(ctx)
	if err != nil {
		return err
	}
	areas, err := services.SharedAreas.Areas(ctx, false)
	if err != nil {
		return err
	}
	tomorrow := services.SharedAreas.Now().AddDate(0, 0, 1).Format("2006-01-02")
	for i, g := range guests {
		for _, a := range areas {
			start := fmt.Sprintf("%02d:00", 10+i%6)
			rid := g.ReservationID
			if _, err := services.SharedAreas.Book(ctx, actor, sharedareas.BookingCreate{
				AreaID: a.ID, Date: tomorrow, Start: start, ReservationID: &rid, People: 2,
			}); err == nil {
				break
			}
		}
	}
	return nil
}

// seedGuestCode da el codigo fijo al primer huesped en casa con correo.
func seedGuestCode(ctx context.Context, services app.Services, slug string) error {
	code, ok := demoGuestCodes[slug]
	if !ok {
		return nil
	}
	guests, err := services.SharedAreas.InHouse(ctx)
	if err != nil {
		return err
	}
	return services.Parking.DB().InTx(ctx, func(q database.Querier) error {
		for _, g := range guests {
			email, err := guestaccess.DemoCode(ctx, q, g.ReservationID, code)
			if errors.Is(err, pgx.ErrNoRows) {
				continue // ya tenia el codigo o no tiene huesped
			}
			if err != nil {
				return err
			}
			if email != "" {
				fmt.Printf("Huésped de prueba de %s: %s · código %s (%s en %s)\n", slug, email, code, g.Name, g.PropertyCode)
				return nil
			}
		}
		return nil
	})
}
