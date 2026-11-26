package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/parking"
	"github.com/smorareqs/argos-service/internal/modules/securityops"
	"github.com/smorareqs/argos-service/internal/modules/sharedareas"
)

// Parqueos, areas comunes y seguridad (fase04.md §4, Persona B). Parqueos
// exige la bandera parking; areas comunes, shared_areas; seguridad y los
// huespedes en casa, reservations.

type spacesInput struct {
	All bool `query:"all" doc:"Incluye los desactivados."`
}

type spacesOutput struct{ Body []parking.SpaceRead }
type spaceOutput struct{ Body parking.SpaceRead }
type spaceCreateInput struct{ Body parking.SpaceCreate }

type spaceUpdateInput struct {
	ID   uuid.UUID `path:"space_id" format:"uuid"`
	Body parking.SpaceUpdate
}

type rangeInput struct {
	From string `query:"from" format:"date" required:"true"`
	To   string `query:"to" format:"date" required:"true" doc:"Inclusive."`
}

type assignmentsOutput struct{ Body []parking.AssignmentRead }
type assignmentOutput struct{ Body parking.AssignmentRead }
type assignmentCreateInput struct{ Body parking.AssignmentCreate }
type staysOutput struct{ Body []parking.StayRead }

type assignmentPath struct {
	ID uuid.UUID `path:"assignment_id" format:"uuid"`
}

func registerParking(v1 huma.API, svc *parking.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"parqueos"}
		op.Security = api.Secured
	})
	need := func(p authz.Permission) huma.Middlewares {
		return guard.RequireFeature(authz.FeatureParking, guard.RequirePermission(p))
	}
	huma.Register(grp, huma.Operation{
		OperationID: "list-parking-spaces", Method: http.MethodGet, Path: "/parking/spaces", Summary: "Espacios de parqueo",
		Middlewares: need(authz.ParkingRead),
	}, authed(func(ctx context.Context, _ auth.Principal, in *spacesInput) (*spacesOutput, error) {
		out, err := svc.Spaces(ctx, in.All)
		return &spacesOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-parking-space", Method: http.MethodPost, Path: "/parking/spaces", Summary: "Agregar un espacio",
		DefaultStatus: http.StatusCreated, Middlewares: need(authz.ParkingManage), Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *spaceCreateInput) (*spaceOutput, error) {
		out, err := svc.CreateSpace(ctx, p.Actor(), in.Body)
		return &spaceOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-parking-space", Method: http.MethodPatch, Path: "/parking/spaces/{space_id}",
		Summary: "Corregir, bloquear o desactivar un espacio", Middlewares: need(authz.ParkingManage),
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *spaceUpdateInput) (*spaceOutput, error) {
		out, err := svc.UpdateSpace(ctx, p.Actor(), in.ID, in.Body)
		return &spaceOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "list-parking-assignments", Method: http.MethodGet, Path: "/parking/assignments",
		Summary: "Asignaciones que tocan esos dias", Middlewares: need(authz.ParkingRead),
	}, authed(func(ctx context.Context, _ auth.Principal, in *rangeInput) (*assignmentsOutput, error) {
		out, err := svc.Assignments(ctx, in.From, in.To)
		return &assignmentsOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "list-assignable-stays", Method: http.MethodGet, Path: "/parking/assignable-stays",
		Summary: "Estancias sin parqueo (proximos 14 dias)", Middlewares: need(authz.ParkingManage),
	}, authed(func(ctx context.Context, _ auth.Principal, _ *struct{}) (*staysOutput, error) {
		out, err := svc.Stays(ctx)
		return &staysOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "assign-parking", Method: http.MethodPost, Path: "/parking/assignments", Summary: "Asignar un espacio",
		Description:   "Por las fechas de la estancia. 409 con quien lo ocupa si se cruza; 422 si la placa no es valida.",
		DefaultStatus: http.StatusCreated, Middlewares: need(authz.ParkingManage),
		Errors: []int{http.StatusConflict, http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *assignmentCreateInput) (*assignmentOutput, error) {
		out, err := svc.Assign(ctx, p.Actor(), in.Body)
		return &assignmentOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "release-parking", Method: http.MethodDelete, Path: "/parking/assignments/{assignment_id}",
		Summary: "Liberar un espacio", DefaultStatus: http.StatusNoContent, Middlewares: need(authz.ParkingManage),
		Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *assignmentPath) (*struct{}, error) {
		return noContent(svc.Release(ctx, p.Actor(), in.ID))
	}))
}

// --- Areas comunes -------------------------------------------------------------

type areasInput struct {
	All bool `query:"all" doc:"Incluye las desactivadas."`
}

type areasOutput struct{ Body []sharedareas.AreaRead }
type areaOutput struct{ Body sharedareas.AreaRead }
type areaCreateInput struct{ Body sharedareas.AreaWrite }

type areaSaveInput struct {
	ID   uuid.UUID `path:"area_id" format:"uuid"`
	Body sharedareas.AreaWrite
}

type areaDateInput struct {
	ID   uuid.UUID `path:"area_id" format:"uuid"`
	Date string    `query:"date" format:"date" required:"true"`
}

type availabilityOutput struct{ Body sharedareas.AreaAvailability }
type areaBookingsOutput struct{ Body []sharedareas.BookingRead }
type areaBookingOutput struct{ Body sharedareas.BookingRead }
type areaBookInput struct{ Body sharedareas.BookingCreate }
type inHouseOutput struct{ Body []sharedareas.InHouseGuest }

type bookingPath struct {
	ID uuid.UUID `path:"booking_id" format:"uuid"`
}

func registerSharedAreas(v1 huma.API, svc *sharedareas.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"areas comunes"}
		op.Security = api.Secured
	})
	need := func(p authz.Permission) huma.Middlewares {
		return guard.RequireFeature(authz.FeatureSharedAreas, guard.RequirePermission(p))
	}
	huma.Register(grp, huma.Operation{
		OperationID: "list-shared-areas", Method: http.MethodGet, Path: "/shared-areas", Summary: "Areas comunes",
		Middlewares: need(authz.SharedAreasRead),
	}, authed(func(ctx context.Context, _ auth.Principal, in *areasInput) (*areasOutput, error) {
		out, err := svc.Areas(ctx, in.All)
		return &areasOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-shared-area", Method: http.MethodPost, Path: "/shared-areas", Summary: "Agregar un area",
		DefaultStatus: http.StatusCreated, Middlewares: need(authz.SharedAreasManage), Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *areaCreateInput) (*areaOutput, error) {
		out, err := svc.CreateArea(ctx, p.Actor(), in.Body)
		return &areaOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "save-shared-area", Method: http.MethodPut, Path: "/shared-areas/{area_id}", Summary: "Guardar la configuracion de un area",
		Middlewares: need(authz.SharedAreasManage), Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *areaSaveInput) (*areaOutput, error) {
		out, err := svc.SaveArea(ctx, p.Actor(), in.ID, in.Body)
		return &areaOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "shared-area-availability", Method: http.MethodGet, Path: "/shared-areas/{area_id}/availability",
		Summary: "Bloques del dia con lugares libres", Middlewares: need(authz.SharedAreasRead), Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, _ auth.Principal, in *areaDateInput) (*availabilityOutput, error) {
		out, err := svc.AvailabilityTx(ctx, in.ID, in.Date)
		return &availabilityOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "list-shared-area-bookings", Method: http.MethodGet, Path: "/shared-areas/bookings",
		Summary: "Bloques reservados de esos dias", Middlewares: need(authz.SharedAreasRead),
	}, authed(func(ctx context.Context, _ auth.Principal, in *rangeInput) (*areaBookingsOutput, error) {
		out, err := svc.Bookings(ctx, in.From, in.To)
		return &areaBookingsOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "book-shared-area", Method: http.MethodPost, Path: "/shared-areas/bookings",
		Summary:       "Reservar un bloque para un huesped",
		Description:   "guest es \"Nombre|CODIGO_PROPIEDAD\" de un huesped hospedado ese dia (o reservation_id). Respeta capacidad y maximo por dia.",
		DefaultStatus: http.StatusCreated, Middlewares: need(authz.SharedAreasBook),
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *areaBookInput) (*areaBookingOutput, error) {
		out, err := svc.Book(ctx, p.Actor(), in.Body)
		return &areaBookingOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "cancel-shared-area-booking", Method: http.MethodDelete, Path: "/shared-areas/bookings/{booking_id}",
		Summary: "Cancelar un bloque", DefaultStatus: http.StatusNoContent, Middlewares: need(authz.SharedAreasBook),
		Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *bookingPath) (*struct{}, error) {
		return noContent(svc.Cancel(ctx, p.Actor(), in.ID))
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "list-in-house-guests", Method: http.MethodGet, Path: "/reservations/in-house",
		Summary: "Huespedes hospedados hoy", Tags: []string{"reservas"},
		Middlewares: guard.RequireFeature(authz.FeatureReservations,
			guard.RequireAnyPermission(authz.ReservationsRead, authz.SharedAreasBook, authz.ParkingManage)),
	}, authed(func(ctx context.Context, _ auth.Principal, _ *struct{}) (*inHouseOutput, error) {
		out, err := svc.InHouse(ctx)
		return &inHouseOutput{Body: out}, err
	}))
}

// --- Seguridad -----------------------------------------------------------------

type dashboardInput struct {
	Date string `query:"date" format:"date" doc:"Vacio = hoy en la zona de la empresa."`
}

type securityDashboardOutput struct{ Body securityops.SecurityDashboard }

type alarmsInput struct {
	Status string `query:"status" enum:"pending,today,all" doc:"pending (por defecto): sonando y las que faltan hoy."`
}

type alarmsOutput struct{ Body []securityops.AlarmRead }
type alarmOutput struct{ Body securityops.AlarmRead }

type alarmAttendInput struct {
	ID   uuid.UUID `path:"alarm_id" format:"uuid"`
	Body *securityops.AttendInput
}

func registerSecurity(v1 huma.API, svc *securityops.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"seguridad"}
		op.Security = api.Secured
	})
	need := guard.RequireFeature(authz.FeatureReservations, guard.RequirePermission(authz.SecurityDashboard))
	huma.Register(grp, huma.Operation{
		OperationID: "security-dashboard", Method: http.MethodGet, Path: "/security/dashboard", Summary: "Tablero del dia",
		Description: "Llegadas, salidas, en casa, placas con espacio, areas reservadas y alarmas pendientes.",
		Middlewares: need, Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, _ auth.Principal, in *dashboardInput) (*securityDashboardOutput, error) {
		out, err := svc.Dashboard(ctx, in.Date)
		return &securityDashboardOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "list-security-alarms", Method: http.MethodGet, Path: "/security/alarms", Summary: "Alarmas de salida",
		Middlewares: need,
	}, authed(func(ctx context.Context, _ auth.Principal, in *alarmsInput) (*alarmsOutput, error) {
		out, err := svc.Alarms(ctx, in.Status)
		return &alarmsOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "attend-security-alarm", Method: http.MethodPost, Path: "/security/alarms/{alarm_id}/attend",
		Summary:     "Marcar atendida",
		Description: "Tambien marca la otra alarma de esa salida si sigue pendiente y resuelve la alerta de sin atender.",
		Middlewares: need, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *alarmAttendInput) (*alarmOutput, error) {
		var body securityops.AttendInput
		if in.Body != nil {
			body = *in.Body
		}
		out, err := svc.Attend(ctx, p.Actor(), in.ID, body)
		return &alarmOutput{Body: out}, err
	}))
}
