package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/notifications"
)

// Avisos: bandeja, preferencias y dispositivos para push (fase03.md §4,
// Persona B). El tiempo real va aparte por WebSocket (GET /api/v1/ws).

type notificationsListInput struct {
	Unread string `query:"unread" enum:"true,false" doc:"true = solo sin leer."`
	Cursor string `query:"cursor"`
	Limit  int    `query:"limit" minimum:"0" maximum:"100"`
}

type notificationsListOutput struct {
	Body notifications.NotificationPage
}

type notificationIDInput struct {
	ID uuid.UUID `path:"notification_id" format:"uuid"`
}

type notificationPrefsOutput struct {
	Body []notifications.Preference
}

type notificationPrefsInput struct {
	Body notifications.PreferencesUpdate
}

type deviceRegisterInput struct {
	Body notifications.DeviceRegister
}

type deviceRemoveInput struct {
	Token string `path:"token" maxLength:"512"`
}

func registerNotifications(v1 huma.API, svc *notifications.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"avisos"}
		op.Security = api.Secured
	})
	member := guard.RequireTenant()

	huma.Register(grp, huma.Operation{
		OperationID: "list-notifications", Method: http.MethodGet, Path: "/notifications", Summary: "Mis avisos",
		Description: "Los mas nuevos primero, con el total sin leer. data.route abre la pantalla en la web y data.app_route en la app.",
		Middlewares: member,
	}, authed(func(ctx context.Context, p auth.Principal, in *notificationsListInput) (*notificationsListOutput, error) {
		out, err := svc.List(ctx, p.Actor(), in.Unread == "true", in.Cursor, in.Limit)
		return &notificationsListOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "read-notification", Method: http.MethodPost, Path: "/notifications/{notification_id}/read",
		Summary: "Marcar un aviso como leido", DefaultStatus: http.StatusNoContent, Middlewares: member,
		Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *notificationIDInput) (*struct{}, error) {
		return noContent(svc.MarkRead(ctx, p.Actor(), in.ID))
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "read-all-notifications", Method: http.MethodPost, Path: "/notifications/read-all",
		Summary: "Marcar todos como leidos", DefaultStatus: http.StatusNoContent, Middlewares: member,
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*struct{}, error) {
		return noContent(svc.MarkAllRead(ctx, p.Actor()))
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "get-notification-preferences", Method: http.MethodGet, Path: "/notifications/preferences",
		Summary: "Mis preferencias de avisos", Middlewares: member,
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*notificationPrefsOutput, error) {
		out, err := svc.Preferences(ctx, p.Actor())
		return &notificationPrefsOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "set-notification-preferences", Method: http.MethodPut, Path: "/notifications/preferences",
		Summary: "Guardar mis preferencias", Description: "Los avisos criticos no se pueden apagar (se ignoran).",
		Middlewares: member, Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *notificationPrefsInput) (*notificationPrefsOutput, error) {
		out, err := svc.SetPreferences(ctx, p.Actor(), in.Body)
		return &notificationPrefsOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "register-device", Method: http.MethodPost, Path: "/devices",
		Summary: "Registrar el telefono para push", Description: "Token de FCM. Si ya existia, pasa a este usuario.",
		DefaultStatus: http.StatusNoContent, Middlewares: member, Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *deviceRegisterInput) (*struct{}, error) {
		return noContent(svc.RegisterDevice(ctx, p.Actor(), in.Body))
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "remove-device", Method: http.MethodDelete, Path: "/devices/{token}",
		Summary: "Quitar el telefono (al cerrar sesion)", DefaultStatus: http.StatusNoContent, Middlewares: member,
		Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *deviceRemoveInput) (*struct{}, error) {
		return noContent(svc.RemoveDevice(ctx, p.Actor(), in.Token))
	}))
}
