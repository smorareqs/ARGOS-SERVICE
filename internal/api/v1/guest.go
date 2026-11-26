package v1

import (
	"context"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/chat"
	"github.com/smorareqs/argos-service/internal/modules/guestaccess"
	"github.com/smorareqs/argos-service/internal/modules/locks"
	"github.com/smorareqs/argos-service/internal/modules/parking"
	"github.com/smorareqs/argos-service/internal/modules/sharedareas"
)

// Modo huesped de la app (fase04.md §3-5, Persona B). POST /guest/auth es
// publico; el resto exige el JWT typ=guest que devuelve, y solo ve la reserva
// de ese acceso (otra reserva: 404; acceso revocado o vencido: 401).

type guestKey struct{}

func guestOf(ctx context.Context) (guestaccess.Principal, error) {
	p, ok := ctx.Value(guestKey{}).(guestaccess.Principal)
	if !ok {
		return guestaccess.Principal{}, api.HTTPError(ctx, guestaccess.ErrInvalidAccess)
	}
	return p, nil
}

// asGuest es un handler del huesped autenticado.
func asGuest[I, O any](fn func(context.Context, guestaccess.Principal, *I) (*O, error)) func(context.Context, *I) (*O, error) {
	return func(ctx context.Context, in *I) (*O, error) {
		p, err := guestOf(ctx)
		if err != nil {
			return nil, err
		}
		out, err := fn(ctx, p, in)
		if err != nil {
			return nil, httpError(ctx, err)
		}
		return out, nil
	}
}

func guestGuard(humaAPI huma.API, svc *guestaccess.Service) huma.Middlewares {
	return huma.Middlewares{func(hctx huma.Context, next func(huma.Context)) {
		ctx := hctx.Context()
		scheme, token, _ := strings.Cut(hctx.Header("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
			hctx.SetHeader("WWW-Authenticate", "Bearer")
			_ = huma.WriteErr(humaAPI, hctx, http.StatusUnauthorized, guestaccess.ErrInvalidAccess.Message)
			return
		}
		p, err := svc.Resolve(ctx, strings.TrimSpace(token))
		if err != nil {
			se := api.HTTPError(ctx, err)
			if se.GetStatus() == http.StatusUnauthorized {
				hctx.SetHeader("WWW-Authenticate", "Bearer")
			}
			_ = huma.WriteErr(humaAPI, hctx, se.GetStatus(), se.Error())
			return
		}
		ctx = context.WithValue(ctx, guestKey{}, p)
		ctx = database.WithTenantSchema(ctx, p.TenantSchema)
		next(huma.WithContext(hctx, ctx))
	}}
}

type guestAuthInput struct{ Body guestaccess.GuestAuthInput }
type guestSessionOutput struct{ Body guestaccess.GuestSession }
type guestMeOutput struct{ Body guestaccess.GuestMe }
type guestReservationOutput struct{ Body guestaccess.GuestReservation }
type guestParkingOutput struct{ Body []parking.AssignmentRead }
type guestAreasOutput struct{ Body guestaccess.GuestAreas }

type guestAreaDateInput struct {
	ID   uuid.UUID `path:"area_id" format:"uuid"`
	Date string    `query:"date" format:"date" required:"true"`
}

type guestBookInput struct {
	ID   uuid.UUID `path:"area_id" format:"uuid"`
	Body sharedareas.GuestBookingCreate
}

type guestTicketInput struct{ Body guestaccess.GuestTicketInput }
type guestTicketOutput struct {
	Body guestaccess.GuestTicketCreated
}
type guestDeviceInput struct{ Body guestaccess.GuestDeviceInput }

func registerGuest(v1 huma.API, svc *guestaccess.Service, chatSvc *chat.Service, lockSvc *locks.Service) {
	grp := huma.NewGroup(v1)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"huesped"}
	})
	huma.Register(grp, huma.Operation{
		OperationID: "guest-auth", Method: http.MethodPost, Path: "/guest/auth", Summary: "Entrar como huesped",
		Description: "Con el token del enlace argos://guest?token=... o con el codigo del correo y el correo. Devuelve un JWT que dura hasta 24 h despues de la salida. 10 intentos por minuto por IP.",
		Errors:      []int{http.StatusUnauthorized, http.StatusUnprocessableEntity, http.StatusTooManyRequests},
	}, func(ctx context.Context, in *guestAuthInput) (*guestSessionOutput, error) {
		out, err := svc.Auth(ctx, api.ClientIP(ctx), in.Body)
		if err != nil {
			return nil, httpError(ctx, err)
		}
		return &guestSessionOutput{Body: out}, nil
	})

	secured := huma.NewGroup(grp)
	secured.UseSimpleModifier(func(op *huma.Operation) {
		op.Security = api.Secured
		op.Middlewares = append(guestGuard(v1, svc), op.Middlewares...)
		op.Errors = append(op.Errors, http.StatusUnauthorized)
	})
	huma.Register(secured, huma.Operation{
		OperationID: "guest-me", Method: http.MethodGet, Path: "/guest/me", Summary: "Quien soy (huesped)",
	}, asGuest(func(ctx context.Context, p guestaccess.Principal, _ *struct{}) (*guestMeOutput, error) {
		out, err := svc.Me(ctx, p)
		return &guestMeOutput{Body: out}, err
	}))
	huma.Register(secured, huma.Operation{
		OperationID: "guest-reservation", Method: http.MethodGet, Path: "/guest/reservation", Summary: "Mi reserva",
		Description: "Fechas, propiedad, instrucciones de la empresa y contacto.",
	}, asGuest(func(ctx context.Context, p guestaccess.Principal, _ *struct{}) (*guestReservationOutput, error) {
		out, err := svc.Reservation(ctx, p)
		return &guestReservationOutput{Body: out}, err
	}))
	huma.Register(secured, huma.Operation{
		OperationID: "guest-parking", Method: http.MethodGet, Path: "/guest/parking", Summary: "Mi parqueo",
		Errors: []int{http.StatusForbidden},
	}, asGuest(func(ctx context.Context, p guestaccess.Principal, _ *struct{}) (*guestParkingOutput, error) {
		out, err := svc.Parking(ctx, p)
		return &guestParkingOutput{Body: out}, err
	}))
	huma.Register(secured, huma.Operation{
		OperationID: "guest-shared-areas", Method: http.MethodGet, Path: "/guest/shared-areas", Summary: "Areas de mi edificio y mis bloques",
		Errors: []int{http.StatusForbidden},
	}, asGuest(func(ctx context.Context, p guestaccess.Principal, _ *struct{}) (*guestAreasOutput, error) {
		out, err := svc.Areas(ctx, p)
		return &guestAreasOutput{Body: out}, err
	}))
	huma.Register(secured, huma.Operation{
		OperationID: "guest-area-availability", Method: http.MethodGet, Path: "/guest/shared-areas/{area_id}/availability",
		Summary: "Bloques libres del dia", Errors: []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity},
	}, asGuest(func(ctx context.Context, p guestaccess.Principal, in *guestAreaDateInput) (*availabilityOutput, error) {
		out, err := svc.Availability(ctx, p, in.ID, in.Date)
		return &availabilityOutput{Body: out}, err
	}))
	huma.Register(secured, huma.Operation{
		OperationID: "guest-book-area", Method: http.MethodPost, Path: "/guest/shared-areas/{area_id}/bookings",
		Summary:       "Reservar un bloque",
		Description:   "Dentro de la estancia, con la anticipacion del area y hasta advance_days adelante. Confirmacion automatica y push.",
		DefaultStatus: http.StatusCreated, Errors: []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity},
	}, asGuest(func(ctx context.Context, p guestaccess.Principal, in *guestBookInput) (*areaBookingOutput, error) {
		out, err := svc.Book(ctx, p, in.ID, in.Body)
		return &areaBookingOutput{Body: out}, err
	}))
	huma.Register(secured, huma.Operation{
		OperationID: "guest-bookings", Method: http.MethodGet, Path: "/guest/bookings", Summary: "Mis bloques",
	}, asGuest(func(ctx context.Context, p guestaccess.Principal, _ *struct{}) (*areaBookingsOutput, error) {
		out, err := svc.Bookings(ctx, p)
		return &areaBookingsOutput{Body: out}, err
	}))
	huma.Register(secured, huma.Operation{
		OperationID: "guest-cancel-booking", Method: http.MethodDelete, Path: "/guest/bookings/{booking_id}",
		Summary: "Cancelar un bloque (hasta 1 h antes)", DefaultStatus: http.StatusNoContent,
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, asGuest(func(ctx context.Context, p guestaccess.Principal, in *bookingPath) (*struct{}, error) {
		return noContent(svc.CancelBooking(ctx, p, in.ID))
	}))
	huma.Register(secured, huma.Operation{
		OperationID: "guest-report-problem", Method: http.MethodPost, Path: "/guest/tickets", Summary: "Reportar un problema",
		Description: "Abre la incidencia en Mantenimiento sobre la propiedad de la reserva.", DefaultStatus: http.StatusCreated,
		Errors: []int{http.StatusUnprocessableEntity},
	}, asGuest(func(ctx context.Context, p guestaccess.Principal, in *guestTicketInput) (*guestTicketOutput, error) {
		out, err := svc.Ticket(ctx, p, in.Body)
		return &guestTicketOutput{Body: out}, err
	}))
	huma.Register(secured, huma.Operation{
		OperationID: "guest-device", Method: http.MethodPost, Path: "/guest/device", Summary: "Registrar el telefono para push",
		DefaultStatus: http.StatusNoContent,
	}, asGuest(func(ctx context.Context, p guestaccess.Principal, in *guestDeviceInput) (*struct{}, error) {
		return noContent(svc.Device(ctx, p, in.Body))
	}))
	registerGuestChat(secured, chatSvc, lockSvc)
}
