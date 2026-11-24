package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/reservations"
)

// Reservas, huespedes, depositos e inspecciones (fase04.md §4). Todo exige la
// bandera reservations de la empresa (403 sin ella).

type reservationPath struct {
	ReservationID uuid.UUID `path:"reservation_id" format:"uuid"`
}

type reservationDetailOutput struct {
	Body reservations.ReservationDetail
}

type reservationsInput struct {
	From       string    `query:"from" format:"date" doc:"Estancias que salen despues de esta fecha (calendario)."`
	To         string    `query:"to" format:"date" doc:"Estancias que entran antes de esta fecha (exclusiva)."`
	PropertyID uuid.UUID `query:"property_id" format:"uuid"`
	ChannelID  uuid.UUID `query:"channel_id" format:"uuid"`
	GuestID    uuid.UUID `query:"guest_id" format:"uuid"`
	Status     string    `query:"status" maxLength:"128" doc:"Lista separada por comas (pending, confirmed, checked_in, checked_out, cancelled, no_show). Vacio = todas."`
	Incomplete bool      `query:"incomplete" doc:"Solo las importadas sin huesped principal."`
	Query      string    `query:"q" maxLength:"64" doc:"Codigo, propiedad, huesped o resumen iCal."`
	Order      string    `query:"order" enum:"asc,desc" doc:"Por fecha de entrada; por defecto asc."`
	Cursor     string    `query:"cursor" maxLength:"512"`
	Limit      int       `query:"limit" minimum:"0" maximum:"500" doc:"Por defecto 50."`
}

type reservationsOutput struct {
	Body page[reservations.ReservationRead]
}

func registerReservations(v1 huma.API, svc *reservations.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"reservas"}
		op.Security = api.Secured
	})
	need := func(perms ...authz.Permission) huma.Middlewares {
		return guard.RequireFeature(authz.FeatureReservations, guard.RequirePermission(perms...))
	}
	either := func(perms ...authz.Permission) huma.Middlewares {
		return guard.RequireFeature(authz.FeatureReservations, guard.RequireAnyPermission(perms...))
	}
	registerChannels(grp, svc, need)
	registerFeeds(grp, svc, need)
	registerReservationReads(grp, svc, need)
	registerReservationWrites(grp, svc, need)
	registerReservationFlow(grp, svc, need)
	registerGuests(grp, svc, need, either)
	registerDeposits(grp, svc, need)
	registerInspections(grp, svc, need)
}

type guardFn func(perms ...authz.Permission) huma.Middlewares

// Parametros de ruta para embeber junto a un Body: huma solo lee los campos de
// un struct embebido si su tipo es exportado.
type (
	// ReservationParam es el id de la reserva en la ruta.
	ReservationParam struct {
		ReservationID uuid.UUID `path:"reservation_id" format:"uuid"`
	}
	// GuestParam es el id del huesped en la ruta.
	GuestParam struct {
		GuestID uuid.UUID `path:"guest_id" format:"uuid"`
	}
	// FeedParam es el id del calendario iCal en la ruta.
	FeedParam struct {
		FeedID uuid.UUID `path:"feed_id" format:"uuid"`
	}
	// ChannelParam es el id del canal en la ruta.
	ChannelParam struct {
		ChannelID uuid.UUID `path:"channel_id" format:"uuid"`
	}
	// PropertyParam es el id de la propiedad en la ruta.
	PropertyParam struct {
		PropertyID uuid.UUID `path:"property_id" format:"uuid"`
	}
	// InspectionParam es el id de la inspeccion en la ruta.
	InspectionParam struct {
		InspectionID uuid.UUID `path:"inspection_id" format:"uuid"`
	}
)

type (
	channelCreateInput     struct{ Body reservations.ChannelCreate }
	reservationCreateInput struct {
		Body reservations.ReservationCreate
	}
	guestCreateInput  struct{ Body reservations.GuestCreate }
	documentURLOutput struct{ Body reservations.DocumentURL }
)

// --- Canales -------------------------------------------------------------------------

type channelOutput struct {
	Body reservations.ChannelRead
}

type channelsOutput struct {
	Body page[reservations.ChannelRead]
}

func registerChannels(grp huma.API, svc *reservations.Service, need guardFn) {
	huma.Register(grp, huma.Operation{
		OperationID: "list-channels", Method: http.MethodGet, Path: "/channels", Summary: "Canales de venta",
		Description: "Con sus reglas de deposito y check-in (W-H6).", Middlewares: need(authz.ReservationsRead),
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*channelsOutput, error) {
		list, err := svc.Channels(ctx, p.Actor())
		return &channelsOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-channel", Method: http.MethodPost, Path: "/channels", Summary: "Nuevo canal",
		DefaultStatus: http.StatusCreated, Middlewares: need(authz.ReservationsManage), Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *channelCreateInput) (*channelOutput, error) {
		out, err := svc.CreateChannel(ctx, p.Actor(), in.Body)
		return &channelOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-channel", Method: http.MethodPatch, Path: "/channels/{channel_id}", Summary: "Editar el canal",
		Description: "Las reservas existentes conservan su deposito.", Middlewares: need(authz.ReservationsManage),
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		ChannelParam
		Body reservations.ChannelUpdate
	},
	) (*channelOutput, error) {
		out, err := svc.UpdateChannel(ctx, p.Actor(), in.ChannelID, in.Body)
		return &channelOutput{Body: out}, err
	}))
}

// --- Calendarios iCal -----------------------------------------------------------------

type feedPath struct {
	FeedID uuid.UUID `path:"feed_id" format:"uuid"`
}

type feedOutput struct {
	Body reservations.FeedRead
}

type feedsOutput struct {
	Body page[reservations.FeedRead]
}

type syncOutput struct {
	Body page[reservations.SyncResult]
}

func registerFeeds(grp huma.API, svc *reservations.Service, need guardFn) {
	manage := need(authz.ReservationsManage)
	huma.Register(grp, huma.Operation{
		OperationID: "list-ical-feeds", Method: http.MethodGet, Path: "/ical-feeds", Summary: "Calendarios iCal",
		Description: "Todos los de la empresa, los que tienen error primero (W-H6).", Middlewares: manage,
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*feedsOutput, error) {
		list, err := svc.Feeds(ctx, p.Actor(), nil)
		return &feedsOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "list-property-ical-feeds", Method: http.MethodGet, Path: "/properties/{property_id}/ical-feeds",
		Summary: "Calendarios iCal de la propiedad", Middlewares: manage, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *propertyPath) (*feedsOutput, error) {
		list, err := svc.Feeds(ctx, p.Actor(), &in.PropertyID)
		return &feedsOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-ical-feed", Method: http.MethodPost, Path: "/properties/{property_id}/ical-feeds",
		Summary: "Agregar un calendario iCal", Description: "Una URL por canal y propiedad (https, o webcal://).",
		DefaultStatus: http.StatusCreated, Middlewares: manage, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		PropertyParam
		Body reservations.FeedCreate
	},
	) (*feedOutput, error) {
		out, err := svc.CreateFeed(ctx, p.Actor(), in.PropertyID, in.Body)
		return &feedOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-ical-feed", Method: http.MethodPatch, Path: "/ical-feeds/{feed_id}", Summary: "Editar el calendario",
		Middlewares: manage, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		FeedParam
		Body reservations.FeedUpdate
	},
	) (*feedOutput, error) {
		out, err := svc.UpdateFeed(ctx, p.Actor(), in.FeedID, in.Body)
		return &feedOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "delete-ical-feed", Method: http.MethodDelete, Path: "/ical-feeds/{feed_id}", Summary: "Quitar el calendario",
		Description: "Las reservas que ya importo se quedan.", Middlewares: manage, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *feedPath) (*struct{}, error) {
		return noContent(svc.DeleteFeed(ctx, p.Actor(), in.FeedID))
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "sync-ical-feeds", Method: http.MethodPost, Path: "/ical-feeds/sync", Summary: "Sincronizar iCal ahora",
		Description: "Lee todos los calendarios activos y devuelve lo que hizo cada uno.", Middlewares: manage,
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*syncOutput, error) {
		list, err := svc.SyncNow(ctx, p.Actor(), nil)
		return &syncOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "sync-ical-feed", Method: http.MethodPost, Path: "/ical-feeds/{feed_id}/sync", Summary: "Sincronizar un calendario",
		Middlewares: manage, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *feedPath) (*syncOutput, error) {
		list, err := svc.SyncNow(ctx, p.Actor(), &in.FeedID)
		return &syncOutput{Body: pageOf(list)}, err
	}))
}

// --- Reservas -----------------------------------------------------------------------------

type todayOutput struct {
	Body reservations.Today
}

func registerReservationReads(grp huma.API, svc *reservations.Service, need guardFn) {
	read := need(authz.ReservationsRead)
	huma.Register(grp, huma.Operation{
		OperationID: "list-reservations", Method: http.MethodGet, Path: "/reservations", Summary: "Reservas",
		Description: "Del alcance. Seguridad solo ve las de hoy y las que estan en curso.", Middlewares: read,
		Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *reservationsInput) (*reservationsOutput, error) {
		items, next, err := svc.Reservations(ctx, p.Actor(), reservations.ReservationFilter{
			From: in.From, To: in.To, PropertyID: optionalID(in.PropertyID), ChannelID: optionalID(in.ChannelID),
			GuestID: optionalID(in.GuestID), Statuses: splitList(in.Status), Incomplete: in.Incomplete, Query: in.Query,
			Desc: in.Order == "desc", Cursor: in.Cursor, Limit: in.Limit,
		})
		if err != nil {
			return nil, err
		}
		return &reservationsOutput{Body: page[reservations.ReservationRead]{Items: items, NextCursor: next}}, nil
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "reservations-today", Method: http.MethodGet, Path: "/reservations/today", Summary: "Hoy en reservas",
		Description: "Llegadas, salidas, estancias en curso y por completar.", Middlewares: read,
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*todayOutput, error) {
		out, err := svc.Today(ctx, p.Actor())
		return &todayOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "get-reservation", Method: http.MethodGet, Path: "/reservations/{reservation_id}", Summary: "Ficha de la reserva",
		Description: "Huespedes con su registro, linea de tiempo, deposito (con deposits.read) e inspecciones.",
		Middlewares: read, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *reservationPath) (*reservationDetailOutput, error) {
		out, err := svc.Reservation(ctx, p.Actor(), in.ReservationID)
		return &reservationDetailOutput{Body: out}, err
	}))
}

func registerReservationWrites(grp huma.API, svc *reservations.Service, need guardFn) {
	manage := need(authz.ReservationsManage)
	errs := []int{http.StatusNotFound, http.StatusUnprocessableEntity}
	huma.Register(grp, huma.Operation{
		OperationID: "create-reservation", Method: http.MethodPost, Path: "/reservations", Summary: "Nueva reserva",
		Description:   "Con huesped principal queda confirmada (publica reservation.confirmed y manda el correo si send_email); sin el, por confirmar. 422 body.check_in_date si se cruza con otra.",
		DefaultStatus: http.StatusCreated, Middlewares: manage, Errors: errs,
	}, authed(func(ctx context.Context, p auth.Principal, in *reservationCreateInput) (*reservationDetailOutput, error) {
		out, err := svc.Create(ctx, p.Actor(), in.Body)
		return &reservationDetailOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-reservation", Method: http.MethodPatch, Path: "/reservations/{reservation_id}", Summary: "Editar la reserva",
		Description: "Fechas antes del check-in (la salida tambien con el huesped en casa). Publica reservation.updated si movio la estancia.",
		Middlewares: manage, Errors: errs,
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		ReservationParam
		Body reservations.ReservationUpdate
	},
	) (*reservationDetailOutput, error) {
		out, err := svc.Update(ctx, p.Actor(), in.ReservationID, in.Body)
		return &reservationDetailOutput{Body: out}, err
	}))
}

func registerReservationFlow(grp huma.API, svc *reservations.Service, need guardFn) {
	errs := []int{http.StatusNotFound, http.StatusUnprocessableEntity}
	huma.Register(grp, huma.Operation{
		OperationID: "confirm-reservation", Method: http.MethodPost, Path: "/reservations/{reservation_id}/confirm", Summary: "Confirmar",
		Description: "Necesita huesped principal. Publica reservation.confirmed.", Middlewares: need(authz.ReservationsManage), Errors: errs,
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		ReservationParam
		Body reservations.ReservationConfirm
	},
	) (*reservationDetailOutput, error) {
		out, err := svc.Confirm(ctx, p.Actor(), in.ReservationID, in.Body)
		return &reservationDetailOutput{Body: out}, err
	}))
	for _, op := range []struct {
		id, path, summary, doc string
		fn                     func(context.Context, authz.Actor, uuid.UUID, reservations.Reason) (reservations.ReservationDetail, error)
	}{
		{"cancel-reservation", "cancel", "Cancelar", "Desde pending o confirmed; reason obligatorio. Publica reservation.cancelled.", svc.Cancel},
		{"no-show-reservation", "no-show", "No llego", "Confirmada, el dia de entrada o despues. Publica reservation.cancelled.", svc.NoShow},
	} {
		fn := op.fn
		huma.Register(grp, huma.Operation{
			OperationID: op.id, Method: http.MethodPost, Path: "/reservations/{reservation_id}/" + op.path, Summary: op.summary,
			Description: op.doc, Middlewares: need(authz.ReservationsManage), Errors: errs,
		}, authed(func(ctx context.Context, p auth.Principal, in *struct {
			ReservationParam
			Body reservations.Reason
		},
		) (*reservationDetailOutput, error) {
			out, err := fn(ctx, p.Actor(), in.ReservationID, in.Body)
			return &reservationDetailOutput{Body: out}, err
		}))
	}
	for _, op := range []struct {
		id, path, summary, doc string
		perm                   authz.Permission
		fn                     func(context.Context, authz.Actor, uuid.UUID) (reservations.ReservationDetail, error)
	}{
		{"check-in-reservation", "check-in", "Check-in", "En canales con check-in formal exige huespedes registrados y el deposito recibido. Publica reservation.checked_in.", authz.ReservationsCheckin, svc.CheckIn},
		{"check-out-reservation", "check-out", "Check-out", "Publica reservation.checked_out. Las fotos del documento se borran a los 90 dias.", authz.ReservationsCheckin, svc.CheckOut},
		{"resend-reservation-email", "resend", "Reenviar el correo", "Confirmacion al huesped principal (422 si no tiene correo).", authz.ReservationsManage, svc.Resend},
	} {
		fn := op.fn
		huma.Register(grp, huma.Operation{
			OperationID: op.id, Method: http.MethodPost, Path: "/reservations/{reservation_id}/" + op.path, Summary: op.summary,
			Description: op.doc, Middlewares: need(op.perm), Errors: errs,
		}, authed(func(ctx context.Context, p auth.Principal, in *reservationPath) (*reservationDetailOutput, error) {
			out, err := fn(ctx, p.Actor(), in.ReservationID)
			return &reservationDetailOutput{Body: out}, err
		}))
	}
}

// --- Huespedes -------------------------------------------------------------------------

type guestPath struct {
	GuestID uuid.UUID `path:"guest_id" format:"uuid"`
}

type guestOutput struct {
	Body reservations.GuestRead
}

type guestDetailOutput struct {
	Body reservations.GuestDetail
}

type guestsInput struct {
	Query       string `query:"q" maxLength:"64" doc:"Nombre, correo, telefono o documento."`
	Blacklisted string `query:"blacklisted" enum:"true,false"`
	Cursor      string `query:"cursor" maxLength:"512"`
	Limit       int    `query:"limit" minimum:"0" maximum:"500"`
}

type guestsOutput struct {
	Body page[reservations.GuestRead]
}

func registerGuests(grp huma.API, svc *reservations.Service, need, either guardFn) {
	errs := []int{http.StatusNotFound, http.StatusUnprocessableEntity}
	huma.Register(grp, huma.Operation{
		OperationID: "list-guests", Method: http.MethodGet, Path: "/guests", Summary: "Huespedes", Middlewares: need(authz.GuestsRead),
	}, authed(func(ctx context.Context, p auth.Principal, in *guestsInput) (*guestsOutput, error) {
		var black *bool
		if in.Blacklisted != "" {
			v := in.Blacklisted == "true"
			black = &v
		}
		items, next, err := svc.Guests(ctx, p.Actor(), reservations.GuestFilter{Query: in.Query, Blacklisted: black, Cursor: in.Cursor, Limit: in.Limit})
		if err != nil {
			return nil, err
		}
		return &guestsOutput{Body: page[reservations.GuestRead]{Items: items, NextCursor: next}}, nil
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "get-guest", Method: http.MethodGet, Path: "/guests/{guest_id}", Summary: "Ficha del huesped",
		Middlewares: need(authz.GuestsRead), Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *guestPath) (*guestDetailOutput, error) {
		out, err := svc.Guest(ctx, p.Actor(), in.GuestID)
		return &guestDetailOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-guest", Method: http.MethodPost, Path: "/guests", Summary: "Nuevo huesped",
		DefaultStatus: http.StatusCreated, Middlewares: need(authz.GuestsManage), Errors: errs,
	}, authed(func(ctx context.Context, p auth.Principal, in *guestCreateInput) (*guestOutput, error) {
		out, err := svc.CreateGuest(ctx, p.Actor(), in.Body)
		return &guestOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-guest", Method: http.MethodPatch, Path: "/guests/{guest_id}", Summary: "Editar el huesped",
		Description: "Quien hace check-ins registra el documento; la lista negra pide guests.manage.",
		Middlewares: either(authz.GuestsManage, authz.ReservationsCheckin), Errors: append(errs, http.StatusForbidden),
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		GuestParam
		Body reservations.GuestUpdate
	},
	) (*guestOutput, error) {
		out, err := svc.UpdateGuest(ctx, p.Actor(), in.GuestID, in.Body)
		return &guestOutput{Body: out}, err
	}))
	registerGuestDocuments(grp, svc, need, either)
	registerReservationGuests(grp, svc, either)
}

func registerGuestDocuments(grp huma.API, svc *reservations.Service, need, either guardFn) {
	huma.Register(grp, huma.Operation{
		OperationID: "attach-guest-document", Method: http.MethodPost, Path: "/guests/{guest_id}/documents",
		Summary: "Foto del documento", Description: "Frente o reverso (archivos guests/id_front o guests/id_back). Reemplaza la anterior del mismo lado.",
		DefaultStatus: http.StatusCreated, Middlewares: either(authz.GuestsManage, authz.ReservationsCheckin),
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		GuestParam
		Body reservations.GuestDocumentAttach
	},
	) (*guestDetailOutput, error) {
		out, err := svc.AttachDocument(ctx, p.Actor(), in.GuestID, in.Body)
		return &guestDetailOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "guest-document-url", Method: http.MethodGet, Path: "/guests/{guest_id}/documents/{file_id}/url",
		Summary: "Ver el documento", Description: "Descarga firmada (5 min). Cada consulta queda en la bitacora (guest.document_viewed).",
		Middlewares: need(authz.GuestsDocuments), Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		GuestParam
		FileID uuid.UUID `path:"file_id" format:"uuid"`
	},
	) (*documentURLOutput, error) {
		out, err := svc.DocumentURL(ctx, p.Actor(), in.GuestID, in.FileID)
		return &documentURLOutput{Body: out}, err
	}))
}

func registerReservationGuests(grp huma.API, svc *reservations.Service, either guardFn) {
	perm := either(authz.GuestsManage, authz.ReservationsCheckin)
	huma.Register(grp, huma.Operation{
		OperationID: "add-reservation-guest", Method: http.MethodPost, Path: "/reservations/{reservation_id}/guests",
		Summary: "Agregar huesped", Description: "Uno existente (guest_id) o nuevo (guest). El primero o con is_primary queda como principal.",
		Middlewares: perm, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		ReservationParam
		Body reservations.GuestLink
	},
	) (*reservationDetailOutput, error) {
		out, err := svc.LinkGuest(ctx, p.Actor(), in.ReservationID, in.Body)
		return &reservationDetailOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "remove-reservation-guest", Method: http.MethodDelete, Path: "/reservations/{reservation_id}/guests/{guest_id}",
		Summary: "Quitar acompanante", Description: "El principal no se quita (422).", Middlewares: perm,
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		ReservationParam
		GuestParam
	},
	) (*reservationDetailOutput, error) {
		out, err := svc.UnlinkGuest(ctx, p.Actor(), in.ReservationID, in.GuestID)
		return &reservationDetailOutput{Body: out}, err
	}))
}

// --- Deposito e inspecciones -----------------------------------------------------------

type depositOutput struct {
	Body reservations.DepositRead
}

func registerDeposits(grp huma.API, svc *reservations.Service, need guardFn) {
	huma.Register(grp, huma.Operation{
		OperationID: "get-deposit", Method: http.MethodGet, Path: "/reservations/{reservation_id}/deposit", Summary: "Deposito",
		Middlewares: need(authz.DepositsRead), Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *reservationPath) (*depositOutput, error) {
		out, err := svc.Deposit(ctx, p.Actor(), in.ReservationID)
		return &depositOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "add-deposit-transaction", Method: http.MethodPost, Path: "/reservations/{reservation_id}/deposit/transactions",
		Summary:       "Registrar un movimiento del deposito",
		Description:   "received hasta el monto; returned y retained necesitan la inspeccion de salida (retained, un hallazgo con cargo). Avisa al huesped por correo.",
		DefaultStatus: http.StatusCreated, Middlewares: need(authz.DepositsManage),
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		ReservationParam
		Body reservations.DepositTxCreate
	},
	) (*depositOutput, error) {
		out, err := svc.AddDepositTx(ctx, p.Actor(), in.ReservationID, in.Body)
		return &depositOutput{Body: out}, err
	}))
}

func registerInspections(grp huma.API, svc *reservations.Service, need guardFn) {
	validate := need(authz.TasksValidate)
	errs := []int{http.StatusNotFound, http.StatusUnprocessableEntity}
	huma.Register(grp, huma.Operation{
		OperationID: "create-inspection", Method: http.MethodPost, Path: "/reservations/{reservation_id}/inspections",
		Summary: "Registrar una inspeccion", Description: "pre_arrival antes del check-in; post_departure despues del check-out. Una de cada tipo.",
		DefaultStatus: http.StatusCreated, Middlewares: validate, Errors: errs,
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		ReservationParam
		Body reservations.InspectionCreate
	},
	) (*reservationDetailOutput, error) {
		out, err := svc.CreateInspection(ctx, p.Actor(), in.ReservationID, in.Body)
		return &reservationDetailOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "add-inspection-finding", Method: http.MethodPost, Path: "/inspections/{inspection_id}/findings",
		Summary: "Agregar un hallazgo", Description: "create_ticket abre la incidencia en mantenimiento (publica ticket.created).",
		DefaultStatus: http.StatusCreated, Middlewares: validate, Errors: errs,
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		InspectionParam
		Body reservations.FindingCreate
	},
	) (*reservationDetailOutput, error) {
		out, err := svc.AddFinding(ctx, p.Actor(), in.InspectionID, in.Body)
		return &reservationDetailOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "remove-inspection-finding", Method: http.MethodDelete, Path: "/inspections/{inspection_id}/findings/{finding_id}",
		Summary: "Quitar un hallazgo", Description: "Mientras el deposito no se liquide.", Middlewares: validate, Errors: errs,
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		InspectionParam
		FindingID uuid.UUID `path:"finding_id" format:"uuid"`
	},
	) (*reservationDetailOutput, error) {
		out, err := svc.RemoveFinding(ctx, p.Actor(), in.InspectionID, in.FindingID)
		return &reservationDetailOutput{Body: out}, err
	}))
}
