package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/chat"
	"github.com/smorareqs/argos-service/internal/modules/guestaccess"
	"github.com/smorareqs/argos-service/internal/modules/locks"
)

// Chat, cerraduras y lo que ve el huesped de ambos (fase06.md §2 y §4.2,
// Persona B). El tiempo real del chat va por /ws (chat.message).

type chatListInput struct {
	Kind string `query:"kind" enum:"internal,guest" default:"internal"`
}
type chatListOutput struct{ Body chat.ConversationPage }
type chatConversationOutput struct{ Body chat.Conversation }
type chatCreateInput struct{ Body chat.ConversationCreate }
type chatPath struct {
	ID uuid.UUID `path:"conversation_id" format:"uuid"`
}
type chatMessagesInput struct {
	ID     uuid.UUID `path:"conversation_id" format:"uuid"`
	Before string    `query:"before" maxLength:"512" doc:"next_cursor de la pagina anterior."`
	Limit  int       `query:"limit" minimum:"1" maximum:"200" default:"50"`
}
type chatMessagesOutput struct{ Body chat.MessagePage }
type chatSendInput struct {
	ID   uuid.UUID `path:"conversation_id" format:"uuid"`
	Body chat.MessageCreate
}
type chatMessageOutput struct{ Body chat.ChatMessage }
type chatAddInput struct {
	ID   uuid.UUID `path:"conversation_id" format:"uuid"`
	Body chat.ParticipantAdd
}
type chatRemoveInput struct {
	ID   uuid.UUID `path:"conversation_id" format:"uuid"`
	User uuid.UUID `path:"user_id" format:"uuid"`
}
type chatMuteInput struct {
	ID   uuid.UUID `path:"conversation_id" format:"uuid"`
	Body chat.ConversationMute
}
type chatPeopleOutput struct{ Body []chat.ChatPerson }

func registerChat(v1 huma.API, svc *chat.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"chat"}
		op.Security = api.Secured
	})
	member := guard.RequireAnyPermission(authz.ChatInternal, authz.ChatGuest)
	internal := guard.RequirePermission(authz.ChatInternal)
	huma.Register(grp, huma.Operation{
		OperationID: "list-conversations", Method: http.MethodGet, Path: "/chat/conversations", Summary: "Conversaciones",
		Description: "internal: donde participo. guest: las de huespedes de mis propiedades (chat.guest). Con mensajes nuevos primero; las archivadas salen 30 dias.",
		Middlewares: member,
	}, authed(func(ctx context.Context, p auth.Principal, in *chatListInput) (*chatListOutput, error) {
		out, err := svc.List(ctx, p.Actor(), in.Kind)
		return &chatListOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-conversation", Method: http.MethodPost, Path: "/chat/conversations", Summary: "Nueva conversacion interna",
		Description:   "Con una persona y sin titulo es 1:1 (si ya existe, devuelve esa). Con titulo o varias personas es un grupo.",
		DefaultStatus: http.StatusCreated, Middlewares: internal, Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *chatCreateInput) (*chatConversationOutput, error) {
		out, err := svc.Create(ctx, p.Actor(), in.Body)
		return &chatConversationOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "chat-people", Method: http.MethodGet, Path: "/chat/people", Summary: "Personas para chatear",
		Description: "Quienes tienen chat interno en la empresa (menos yo).", Middlewares: internal,
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*chatPeopleOutput, error) {
		out, err := svc.People(ctx, p.Actor())
		return &chatPeopleOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "get-conversation", Method: http.MethodGet, Path: "/chat/conversations/{conversation_id}", Summary: "Una conversacion",
		Middlewares: member, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *chatPath) (*chatConversationOutput, error) {
		out, err := svc.Get(ctx, p.Actor(), in.ID)
		return &chatConversationOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "list-messages", Method: http.MethodGet, Path: "/chat/conversations/{conversation_id}/messages", Summary: "Mensajes",
		Description: "Los antiguos primero; next_cursor pide los anteriores con ?before=.", Middlewares: member,
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *chatMessagesInput) (*chatMessagesOutput, error) {
		out, err := svc.Messages(ctx, p.Actor(), in.ID, in.Before, in.Limit)
		return &chatMessagesOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "send-message", Method: http.MethodPost, Path: "/chat/conversations/{conversation_id}/messages", Summary: "Mandar un mensaje",
		Description:   "Texto y/o una foto (files module=chat, kind=image). Sale por tiempo real (chat.message) y push. 422 si esta archivada.",
		DefaultStatus: http.StatusCreated, Middlewares: member, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *chatSendInput) (*chatMessageOutput, error) {
		out, err := svc.Send(ctx, p.Actor(), in.ID, in.Body)
		return &chatMessageOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "read-conversation", Method: http.MethodPost, Path: "/chat/conversations/{conversation_id}/read", Summary: "Marcar como leida",
		DefaultStatus: http.StatusNoContent, Middlewares: member, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *chatPath) (*struct{}, error) {
		return noContent(svc.Read(ctx, p.Actor(), in.ID))
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "mute-conversation", Method: http.MethodPost, Path: "/chat/conversations/{conversation_id}/mute", Summary: "Silenciar o no el push",
		Middlewares: member, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *chatMuteInput) (*chatConversationOutput, error) {
		out, err := svc.Mute(ctx, p.Actor(), in.ID, in.Body.Muted)
		return &chatConversationOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "add-participant", Method: http.MethodPost, Path: "/chat/conversations/{conversation_id}/participants", Summary: "Agregar a un grupo",
		Description: "Un participante, o quien administra usuarios (tambien para unirse). Solo internas.",
		Middlewares: internal, Errors: []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *chatAddInput) (*chatConversationOutput, error) {
		out, err := svc.AddParticipant(ctx, p.Actor(), in.ID, in.Body)
		return &chatConversationOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "remove-participant", Method: http.MethodDelete, Path: "/chat/conversations/{conversation_id}/participants/{user_id}",
		Summary: "Sacar de un grupo", Description: "Uno mismo, quien lo creo o quien administra usuarios.",
		DefaultStatus: http.StatusNoContent, Middlewares: internal,
		Errors: []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *chatRemoveInput) (*struct{}, error) {
		return noContent(svc.RemoveParticipant(ctx, p.Actor(), in.ID, in.User))
	}))
}

type lockDevicesOutput struct{ Body []locks.DeviceRead }
type lockDeviceOutput struct{ Body locks.DeviceRead }
type lockDeviceCreateInput struct{ Body locks.DeviceCreate }
type lockDeviceUpdateInput struct {
	ID   uuid.UUID `path:"device_id" format:"uuid"`
	Body locks.DeviceUpdate
}
type lockCodesInput struct {
	Reservation uuid.UUID `query:"reservation_id" format:"uuid" doc:"Solo esa reserva."`
}
type lockCodesOutput struct{ Body []locks.CodeRead }
type lockProvisionInput struct {
	ID uuid.UUID `path:"reservation_id" format:"uuid"`
}

func registerLocks(v1 huma.API, svc *locks.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"cerraduras"}
		op.Security = api.Secured
	})
	need := func(p authz.Permission) huma.Middlewares {
		return guard.RequireFeature(authz.FeatureSmartLocks, guard.RequirePermission(p))
	}
	huma.Register(grp, huma.Operation{
		OperationID: "list-lock-devices", Method: http.MethodGet, Path: "/locks/devices", Summary: "Cerraduras",
		Middlewares: need(authz.PropertiesManage),
	}, authed(func(ctx context.Context, _ auth.Principal, _ *struct{}) (*lockDevicesOutput, error) {
		out, err := svc.Devices(ctx)
		return &lockDevicesOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-lock-device", Method: http.MethodPost, Path: "/locks/devices", Summary: "Registrar una cerradura",
		DefaultStatus: http.StatusCreated, Middlewares: need(authz.PropertiesManage),
		Errors: []int{http.StatusConflict, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *lockDeviceCreateInput) (*lockDeviceOutput, error) {
		out, err := svc.CreateDevice(ctx, p.Actor(), in.Body)
		return &lockDeviceOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-lock-device", Method: http.MethodPatch, Path: "/locks/devices/{device_id}", Summary: "Editar una cerradura",
		Middlewares: need(authz.PropertiesManage), Errors: []int{http.StatusConflict, http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *lockDeviceUpdateInput) (*lockDeviceOutput, error) {
		out, err := svc.UpdateDevice(ctx, p.Actor(), in.ID, in.Body)
		return &lockDeviceOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "list-lock-codes", Method: http.MethodGet, Path: "/locks/codes", Summary: "Codigos de acceso",
		Description: "Estado de cada codigo (solo los dos ultimos digitos).", Middlewares: need(authz.ReservationsRead),
	}, authed(func(ctx context.Context, _ auth.Principal, in *lockCodesInput) (*lockCodesOutput, error) {
		var id *uuid.UUID
		if in.Reservation != uuid.Nil {
			id = &in.Reservation
		}
		out, err := svc.Codes(ctx, id)
		return &lockCodesOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "provision-lock-codes", Method: http.MethodPost, Path: "/locks/reservations/{reservation_id}/provision",
		Summary:     "Generar o reintentar los codigos de una reserva",
		Description: "Si una cerradura falla, su codigo queda en failed con last_error.",
		Middlewares: need(authz.ReservationsManage), Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *lockProvisionInput) (*lockCodesOutput, error) {
		out, err := svc.Reprovision(ctx, p.Actor(), in.ID)
		return &lockCodesOutput{Body: out}, err
	}))
}

type guestChatOutput struct{ Body chat.GuestChat }
type guestChatSendInput struct{ Body chat.MessageCreate }
type guestLockOutput struct{ Body locks.GuestLock }

func guestChatter(p guestaccess.Principal) chat.Guest {
	return chat.Guest{
		TenantID: p.TenantID, TenantName: p.TenantName, ReservationID: p.ReservationID, GuestID: p.GuestID,
		Enabled: p.Features["guest_chat"],
	}
}

func registerGuestChat(secured huma.API, svc *chat.Service, lockSvc *locks.Service) {
	huma.Register(secured, huma.Operation{
		OperationID: "guest-chat", Method: http.MethodGet, Path: "/guest/chat", Summary: "Mi chat con la empresa",
		Description: "La conversacion de la reserva (se crea la primera vez) con los ultimos 100 mensajes. 403 sin la bandera guest_chat.",
		Errors:      []int{http.StatusForbidden},
	}, asGuest(func(ctx context.Context, p guestaccess.Principal, _ *struct{}) (*guestChatOutput, error) {
		out, err := svc.GuestView(ctx, guestChatter(p))
		return &guestChatOutput{Body: out}, err
	}))
	huma.Register(secured, huma.Operation{
		OperationID: "guest-chat-send", Method: http.MethodPost, Path: "/guest/chat/messages", Summary: "Escribir a la empresa",
		Description: "Solo texto. Llega al instante a la web y la app del personal.", DefaultStatus: http.StatusCreated,
		Errors: []int{http.StatusForbidden, http.StatusUnprocessableEntity},
	}, asGuest(func(ctx context.Context, p guestaccess.Principal, in *guestChatSendInput) (*chatMessageOutput, error) {
		out, err := svc.GuestSend(ctx, guestChatter(p), in.Body)
		return &chatMessageOutput{Body: out}, err
	}))
	huma.Register(secured, huma.Operation{
		OperationID: "guest-chat-read", Method: http.MethodPost, Path: "/guest/chat/read", Summary: "Marcar mi chat como leido",
		DefaultStatus: http.StatusNoContent, Errors: []int{http.StatusForbidden},
	}, asGuest(func(ctx context.Context, p guestaccess.Principal, _ *struct{}) (*struct{}, error) {
		return noContent(svc.GuestRead(ctx, guestChatter(p)))
	}))
	huma.Register(secured, huma.Operation{
		OperationID: "guest-lock", Method: http.MethodGet, Path: "/guest/lock", Summary: "El codigo de mi puerta",
		Description: "Desde 24 h antes de la entrada y hasta 2 h despues de la salida. Sin cerradura: has_lock=false y un mensaje.",
	}, asGuest(func(ctx context.Context, p guestaccess.Principal, _ *struct{}) (*guestLockOutput, error) {
		out, err := lockSvc.ForGuest(ctx, p.TenantID, p.ReservationID)
		return &guestLockOutput{Body: out}, err
	}))
}
