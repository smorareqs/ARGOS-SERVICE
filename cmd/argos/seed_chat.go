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
	"github.com/smorareqs/argos-service/internal/modules/chat"
	"github.com/smorareqs/argos-service/internal/modules/guestaccess"
	"github.com/smorareqs/argos-service/internal/modules/locks"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
)

// Semilla de la fase 6 de Persona B (solo Garrent, que tiene chat con
// huespedes y cerraduras): una 1:1 y un grupo con mensajes, la conversacion
// del huesped de prueba (HUESPED-GAR) con una pregunta y su respuesta, y una
// cerradura simulada en su propiedad con el codigo ya programado.

type demoLine struct {
	from string // correo; "" = el huesped
	body string
}

func seedChatAndLocks(ctx context.Context, services app.Services, ids map[string]tenants.Tenant) error {
	t, ok := ids["garrent"]
	if !ok {
		return nil
	}
	tctx := database.WithTenantSchema(ctx, t.SchemaName)
	db := services.Parking.DB()
	var done bool
	if err := db.InTx(tctx, func(q database.Querier) error {
		return q.QueryRow(tctx, `SELECT EXISTS (SELECT 1 FROM conversations WHERE kind = 'internal')`).Scan(&done)
	}); err != nil || done {
		return err
	}
	users := map[string]uuid.UUID{}
	for _, email := range []string{"ana.lopez@garrent.gt", "jorge.pena@garrent.gt", "maria.ramos@garrent.gt", "luis.torres@garrent.gt"} {
		u, err := services.Auth.UserByEmail(ctx, email)
		if err != nil {
			return err
		}
		users[email] = u.ID
	}
	actor := func(email string) authz.Actor {
		return authz.Actor{UserID: users[email], TenantID: t.ID, Permissions: allPermissions()}
	}
	talk := func(convID uuid.UUID, lines []demoLine) error {
		for _, l := range lines {
			if _, err := services.Chat.Send(tctx, actor(l.from), convID, chat.MessageCreate{Body: l.body}); err != nil {
				return err
			}
		}
		return nil
	}

	dm, err := services.Chat.Create(tctx, actor("jorge.pena@garrent.gt"), chat.ConversationCreate{UserIDs: []uuid.UUID{users["maria.ramos@garrent.gt"]}})
	if err != nil {
		return err
	}
	if err := talk(dm.ID, []demoLine{
		{"jorge.pena@garrent.gt", "María, hoy entra un huésped temprano a TB-504. ¿La puedes hacer primero?"},
		{"maria.ramos@garrent.gt", "Claro, voy para allá en cuanto termine la de Cayalá."},
		{"jorge.pena@garrent.gt", "Gracias 🙌 Avísame si falta algo de insumos."},
	}); err != nil {
		return err
	}
	title := "Mantenimiento Garrent"
	group, err := services.Chat.Create(tctx, actor("ana.lopez@garrent.gt"), chat.ConversationCreate{
		Title: &title, UserIDs: []uuid.UUID{users["jorge.pena@garrent.gt"], users["luis.torres@garrent.gt"]},
	})
	if err != nil {
		return err
	}
	if err := talk(group.ID, []demoLine{
		{"luis.torres@garrent.gt", "Ya cambié el empaque de la llave del baño en Cayalá 12."},
		{"ana.lopez@garrent.gt", "Perfecto. ¿Dejaste la foto en la incidencia?"},
		{"luis.torres@garrent.gt", "Sí, ya está con el antes y el después."},
	}); err != nil {
		return err
	}

	// Huesped de prueba: su conversacion y la cerradura de su propiedad.
	var reservationID, propertyID, guestID uuid.UUID
	err = db.InTx(tctx, func(q database.Querier) error {
		id, err := guestaccess.DemoReservation(tctx, q, demoGuestCodes["garrent"])
		if err != nil {
			return err
		}
		reservationID = id
		return q.QueryRow(tctx, `SELECT property_id, primary_guest_id FROM reservations WHERE id = $1`, id).Scan(&propertyID, &guestID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		fmt.Println("Chat de demostración listo (sin huésped de prueba).")
		return nil
	}
	if err != nil {
		return err
	}
	guest := chat.Guest{TenantID: t.ID, TenantName: t.Name, ReservationID: reservationID, GuestID: guestID, Enabled: true}
	if _, err := services.Chat.GuestSend(tctx, guest, chat.MessageCreate{Body: "Hola, ¿me pueden dejar dos toallas extra?"}); err != nil {
		return err
	}
	view, err := services.Chat.GuestView(tctx, guest)
	if err != nil {
		return err
	}
	if _, err := services.Chat.Send(tctx, actor("jorge.pena@garrent.gt"), view.ID, chat.MessageCreate{
		Body: "¡Claro! En la tarde pasa María a dejarlas. ¿Algo más?",
	}); err != nil {
		return err
	}
	admin := actor("ana.lopez@garrent.gt")
	if _, err := services.Locks.CreateDevice(tctx, admin, locks.DeviceCreate{
		PropertyID: propertyID, Name: "Puerta principal", ExternalID: "nexxt-demo-" + reservationID.String()[:8], Provider: "fake",
	}); err != nil {
		return err
	}
	if _, err := services.Locks.Provision(tctx, t.ID, reservationID); err != nil {
		return err
	}
	fmt.Println("Chat y cerradura simulada de demostración listos.")
	return nil
}
