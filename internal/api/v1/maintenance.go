package v1

import (
	"context"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/maintenance"
)

// Mantenimiento: proveedores, activos, preventivos, incidencias e indicadores
// (fase03.md §5).

type vendorsInput struct {
	Active string `query:"active" enum:"true,false" doc:"Vacio = todos."`
}

type vendorsOutput struct {
	Body page[maintenance.VendorRead]
}

type vendorOutput struct {
	Body maintenance.VendorRead
}

type vendorPath struct {
	VendorID uuid.UUID `path:"vendor_id" format:"uuid"`
}

type vendorCreateInput struct {
	Body maintenance.VendorCreate
}

type vendorUpdateInput struct {
	VendorID uuid.UUID `path:"vendor_id" format:"uuid"`
	Body     maintenance.VendorUpdate
}

type assetsInput struct {
	Query      string    `query:"q" maxLength:"64" doc:"Busca en nombre, marca, modelo, serie, ubicacion y codigo de propiedad."`
	PropertyID uuid.UUID `query:"property_id" format:"uuid" doc:"Los de la propiedad y los de su edificio."`
	BuildingID uuid.UUID `query:"building_id" format:"uuid"`
	VendorID   uuid.UUID `query:"vendor_id" format:"uuid"`
	Category   string    `query:"category" enum:"elevator,camera,water,paint,appliance,plumbing,electrical,hvac,hospitality,furniture,other"`
	Status     string    `query:"status" enum:"operational,needs_attention,in_repair,out_of_service"`
	Active     string    `query:"active" enum:"true,false"`
	Cursor     string    `query:"cursor" maxLength:"512"`
	Limit      int       `query:"limit" minimum:"0" maximum:"500" doc:"Por defecto 50."`
}

type assetsOutput struct {
	Body page[maintenance.AssetRead]
}

type assetOutput struct {
	Body maintenance.AssetRead
}

type assetDetailOutput struct {
	Body maintenance.AssetDetail
}

type assetPath struct {
	AssetID uuid.UUID `path:"asset_id" format:"uuid"`
}

type assetCreateInput struct {
	Body maintenance.AssetCreate
}

type assetUpdateInput struct {
	AssetID uuid.UUID `path:"asset_id" format:"uuid"`
	Body    maintenance.AssetUpdate
}

type documentInput struct {
	AssetID uuid.UUID `path:"asset_id" format:"uuid"`
	Body    maintenance.DocumentAttach
}

type documentOutput struct {
	Body maintenance.DocumentRead
}

type documentPath struct {
	AssetID uuid.UUID `path:"asset_id" format:"uuid"`
	FileID  uuid.UUID `path:"file_id" format:"uuid"`
}

type plansInput struct {
	From    string    `query:"from" format:"date" doc:"Vencen desde (incluido)."`
	To      string    `query:"to" format:"date" doc:"Vencen hasta (incluido)."`
	AssetID uuid.UUID `query:"asset_id" format:"uuid"`
	Active  string    `query:"active" enum:"true,false"`
}

type plansOutput struct {
	Body page[maintenance.PlanRead]
}

type planOutput struct {
	Body maintenance.PlanRead
}

type planCreateInput struct {
	AssetID uuid.UUID `path:"asset_id" format:"uuid"`
	Body    maintenance.PlanCreate
}

type planUpdateInput struct {
	AssetID uuid.UUID `path:"asset_id" format:"uuid"`
	PlanID  uuid.UUID `path:"plan_id" format:"uuid"`
	Body    maintenance.PlanUpdate
}

func registerMaintenance(v1 huma.API, svc *maintenance.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"mantenimiento"}
		op.Security = api.Secured
	})
	registerVendors(grp, svc, guard)
	registerAssets(grp, svc, guard)
	registerPlans(grp, svc, guard)
	registerTicketReads(grp, svc, guard)
	registerTicketFlow(grp, svc, guard)
}

func registerVendors(grp huma.API, svc *maintenance.Service, guard *api.Guard) {
	read, manage := guard.RequirePermission(authz.MaintenanceRead), guard.RequirePermission(authz.MaintenanceManage)
	huma.Register(grp, huma.Operation{
		OperationID: "list-vendors", Method: http.MethodGet, Path: "/vendors", Summary: "Proveedores",
		Description: "Activos primero, por nombre, con activos a cargo, incidencias y gasto del año.", Middlewares: read,
	}, authed(func(ctx context.Context, _ auth.Principal, in *vendorsInput) (*vendorsOutput, error) {
		list, err := svc.Vendors(ctx, optionalBool(in.Active))
		return &vendorsOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "get-vendor", Method: http.MethodGet, Path: "/vendors/{vendor_id}", Summary: "Proveedor",
		Middlewares: read, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, _ auth.Principal, in *vendorPath) (*vendorOutput, error) {
		out, err := svc.Vendor(ctx, in.VendorID)
		return &vendorOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-vendor", Method: http.MethodPost, Path: "/vendors", Summary: "Dar de alta un proveedor",
		DefaultStatus: http.StatusCreated, Middlewares: manage, Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *vendorCreateInput) (*vendorOutput, error) {
		out, err := svc.CreateVendor(ctx, p.Actor(), in.Body)
		return &vendorOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-vendor", Method: http.MethodPatch, Path: "/vendors/{vendor_id}", Summary: "Editar el proveedor",
		Description: "is_active = false lo saca de los selectores; conserva su historial.",
		Middlewares: manage, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *vendorUpdateInput) (*vendorOutput, error) {
		out, err := svc.UpdateVendor(ctx, p.Actor(), in.VendorID, in.Body)
		return &vendorOutput{Body: out}, err
	}))
}

func registerAssets(grp huma.API, svc *maintenance.Service, guard *api.Guard) {
	read, manage := guard.RequirePermission(authz.MaintenanceRead), guard.RequirePermission(authz.MaintenanceManage)
	huma.Register(grp, huma.Operation{
		OperationID: "list-assets", Method: http.MethodGet, Path: "/assets", Summary: "Activos",
		Description: "Del alcance (los de sus propiedades y de los edificios de esas propiedades), por nombre.",
		Middlewares: read,
	}, authed(func(ctx context.Context, p auth.Principal, in *assetsInput) (*assetsOutput, error) {
		items, next, err := svc.Assets(ctx, p.Actor(), maintenance.AssetFilter{
			Query: in.Query, PropertyID: optionalID(in.PropertyID), BuildingID: optionalID(in.BuildingID),
			VendorID: optionalID(in.VendorID), Category: in.Category, Status: in.Status, Active: optionalBool(in.Active),
			Cursor: in.Cursor, Limit: in.Limit,
		})
		if err != nil {
			return nil, err
		}
		return &assetsOutput{Body: page[maintenance.AssetRead]{Items: items, NextCursor: next}}, nil
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "get-asset", Method: http.MethodGet, Path: "/assets/{asset_id}", Summary: "Ficha del activo",
		Description: "Con preventivos, documentos (URL firmada de 1 h) y sus 20 incidencias mas recientes.",
		Middlewares: read, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *assetPath) (*assetDetailOutput, error) {
		out, err := svc.Asset(ctx, p.Actor(), in.AssetID)
		return &assetDetailOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-asset", Method: http.MethodPost, Path: "/assets", Summary: "Dar de alta un activo",
		Description:   "De una propiedad o de un edificio (uno de los dos). Un estado distinto de operational levanta la alerta asset_failure.",
		DefaultStatus: http.StatusCreated, Middlewares: manage, Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *assetCreateInput) (*assetOutput, error) {
		out, err := svc.CreateAsset(ctx, p.Actor(), in.Body)
		return &assetOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-asset", Method: http.MethodPatch, Path: "/assets/{asset_id}", Summary: "Editar el activo",
		Description: "needs_attention, in_repair u out_of_service levantan asset_failure; operational o desactivarlo la resuelve.",
		Middlewares: manage, Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *assetUpdateInput) (*assetOutput, error) {
		out, err := svc.UpdateAsset(ctx, p.Actor(), in.AssetID, in.Body)
		return &assetOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "attach-asset-document", Method: http.MethodPost, Path: "/assets/{asset_id}/documents",
		Summary: "Adjuntar un documento", Description: "Archivo maintenance/asset_document ya confirmado (PDF o imagen, 15 MB).",
		DefaultStatus: http.StatusCreated, Middlewares: manage,
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *documentInput) (*documentOutput, error) {
		out, err := svc.AttachDocument(ctx, p.Actor(), in.AssetID, in.Body)
		return &documentOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "remove-asset-document", Method: http.MethodDelete, Path: "/assets/{asset_id}/documents/{file_id}",
		Summary: "Quitar un documento", Middlewares: manage, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *documentPath) (*struct{}, error) {
		return noContent(svc.RemoveDocument(ctx, p.Actor(), in.AssetID, in.FileID))
	}))
}

func registerPlans(grp huma.API, svc *maintenance.Service, guard *api.Guard) {
	read, manage := guard.RequirePermission(authz.MaintenanceRead), guard.RequirePermission(authz.MaintenanceManage)
	huma.Register(grp, huma.Operation{
		OperationID: "list-maintenance-plans", Method: http.MethodGet, Path: "/maintenance/plans", Summary: "Preventivos",
		Description: "Del alcance, por vencimiento (calendario W-G3). due_state: ok, upcoming (en su anticipacion), overdue, inactive.",
		Middlewares: read, Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *plansInput) (*plansOutput, error) {
		list, err := svc.Plans(ctx, p.Actor(), maintenance.PlanFilter{
			From: in.From, To: in.To, AssetID: optionalID(in.AssetID), Active: optionalBool(in.Active),
		})
		return &plansOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "list-asset-plans", Method: http.MethodGet, Path: "/assets/{asset_id}/plans", Summary: "Preventivos del activo",
		Middlewares: read, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *assetPath) (*plansOutput, error) {
		out, err := svc.Asset(ctx, p.Actor(), in.AssetID)
		return &plansOutput{Body: pageOf(out.Plans)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-asset-plan", Method: http.MethodPost, Path: "/assets/{asset_id}/plans", Summary: "Nuevo preventivo",
		Description:   "Sin next_due_at vence en last_done_at + frecuencia (u hoy + frecuencia). Sin proveedor usa el del activo.",
		DefaultStatus: http.StatusCreated, Middlewares: manage,
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *planCreateInput) (*planOutput, error) {
		out, err := svc.CreatePlan(ctx, p.Actor(), in.AssetID, in.Body)
		return &planOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "update-asset-plan", Method: http.MethodPatch, Path: "/assets/{asset_id}/plans/{plan_id}",
		Summary: "Editar el preventivo", Middlewares: manage,
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *planUpdateInput) (*planOutput, error) {
		out, err := svc.UpdatePlan(ctx, p.Actor(), in.AssetID, in.PlanID, in.Body)
		return &planOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "maintenance-metrics", Method: http.MethodGet, Path: "/maintenance/metrics", Summary: "Indicadores",
		Description: "Del alcance: abiertas, urgentes, preventivos, tiempo de resolucion por mes y costo por categoria (W-G5).",
		Middlewares: read,
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*metricsOutput, error) {
		out, err := svc.Metrics(ctx, p.Actor())
		return &metricsOutput{Body: out}, err
	}))
}

type metricsOutput struct {
	Body maintenance.Metrics
}

type ticketsInput struct {
	Query      string    `query:"q" maxLength:"64" doc:"Busca en titulo, propiedad, activo o folio (#42)."`
	Status     string    `query:"status" maxLength:"128" doc:"Lista separada por comas; active = open, assigned e in_progress. Vacio = todas."`
	Urgency    string    `query:"urgency" enum:"low,medium,high,critical"`
	PropertyID uuid.UUID `query:"property_id" format:"uuid"`
	AssetID    uuid.UUID `query:"asset_id" format:"uuid"`
	VendorID   uuid.UUID `query:"vendor_id" format:"uuid"`
	AssigneeID uuid.UUID `query:"assignee" format:"uuid"`
	Mine       bool      `query:"mine" doc:"Solo las asignadas a quien consulta (app de mantenimiento)."`
	Cursor     string    `query:"cursor" maxLength:"512"`
	Limit      int       `query:"limit" minimum:"0" maximum:"500" doc:"Por defecto 50."`
}

type ticketsOutput struct {
	Body page[maintenance.TicketRead]
}

type ticketOutput struct {
	Body maintenance.TicketDetail
}

type ticketPath struct {
	TicketID uuid.UUID `path:"ticket_id" format:"uuid"`
}

type ticketCreateInput struct {
	Body maintenance.TicketCreate
}

type ticketUpdateInput struct {
	TicketID uuid.UUID `path:"ticket_id" format:"uuid"`
	Body     maintenance.TicketUpdate
}

type ticketAssignInput struct {
	TicketID uuid.UUID `path:"ticket_id" format:"uuid"`
	Body     maintenance.TicketAssign
}

type ticketResolveInput struct {
	TicketID uuid.UUID `path:"ticket_id" format:"uuid"`
	Body     maintenance.TicketResolve
}

type ticketReasonInput struct {
	TicketID uuid.UUID `path:"ticket_id" format:"uuid"`
	Body     maintenance.TicketReason
}

type assignablesOutput struct {
	Body page[maintenance.Assignable]
}

func registerTicketReads(grp huma.API, svc *maintenance.Service, guard *api.Guard) {
	see := guard.RequireAnyPermission(authz.MaintenanceRead, authz.MaintenanceReport)
	huma.Register(grp, huma.Operation{
		OperationID: "list-tickets", Method: http.MethodGet, Path: "/tickets", Summary: "Incidencias",
		Description: "Del alcance (sus propiedades, o las que reporto o tiene asignadas), las mas nuevas primero.",
		Middlewares: see,
	}, authed(func(ctx context.Context, p auth.Principal, in *ticketsInput) (*ticketsOutput, error) {
		items, next, err := svc.Tickets(ctx, p.Actor(), maintenance.TicketFilter{
			Query: in.Query, Statuses: splitList(in.Status), Urgency: in.Urgency, PropertyID: optionalID(in.PropertyID),
			AssetID: optionalID(in.AssetID), VendorID: optionalID(in.VendorID), AssigneeID: optionalID(in.AssigneeID),
			Mine: in.Mine, Cursor: in.Cursor, Limit: in.Limit,
		})
		if err != nil {
			return nil, err
		}
		return &ticketsOutput{Body: page[maintenance.TicketRead]{Items: items, NextCursor: next}}, nil
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "get-ticket", Method: http.MethodGet, Path: "/tickets/{ticket_id}", Summary: "Incidencia",
		Description: "Con linea de tiempo y fotos (URL firmada de 1 h).", Middlewares: see, Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *ticketPath) (*ticketOutput, error) {
		out, err := svc.Ticket(ctx, p.Actor(), in.TicketID)
		return &ticketOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "create-ticket", Method: http.MethodPost, Path: "/tickets", Summary: "Reportar una incidencia",
		Description:   "Sobre una propiedad del alcance (fuera: 404). Publica ticket.created.",
		DefaultStatus: http.StatusCreated, Middlewares: guard.RequirePermission(authz.MaintenanceReport),
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *ticketCreateInput) (*ticketOutput, error) {
		out, err := svc.CreateTicket(ctx, p.Actor(), in.Body)
		return &ticketOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "maintenance-assignables", Method: http.MethodGet, Path: "/maintenance/assignables",
		Summary: "Quienes atienden incidencias", Description: "Miembros activos con maintenance.execute, los menos cargados primero.",
		Middlewares: guard.RequireAnyPermission(authz.MaintenanceAssign, authz.MaintenanceManage),
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*assignablesOutput, error) {
		list, err := svc.Assignables(ctx, p.Actor())
		return &assignablesOutput{Body: pageOf(list)}, err
	}))
}

func registerTicketFlow(grp huma.API, svc *maintenance.Service, guard *api.Guard) {
	manage := guard.RequirePermission(authz.MaintenanceManage)
	execute := guard.RequirePermission(authz.MaintenanceExecute)
	errs := []int{http.StatusNotFound, http.StatusUnprocessableEntity}
	huma.Register(grp, huma.Operation{
		OperationID: "update-ticket", Method: http.MethodPatch, Path: "/tickets/{ticket_id}", Summary: "Corregir la incidencia",
		Description: "Titulo, descripcion, urgencia, activo o costo.", Middlewares: manage, Errors: errs,
	}, authed(func(ctx context.Context, p auth.Principal, in *ticketUpdateInput) (*ticketOutput, error) {
		out, err := svc.UpdateTicket(ctx, p.Actor(), in.TicketID, in.Body)
		return &ticketOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "assign-ticket", Method: http.MethodPost, Path: "/tickets/{ticket_id}/assign", Summary: "Asignar",
		Description: "El cuerpo es la asignacion completa (colaborador, proveedor o ambos). A un colaborador crea su tarea de mantenimiento cuando el modulo de tareas esta conectado. Publica ticket.assigned.",
		Middlewares: guard.RequireAnyPermission(authz.MaintenanceAssign, authz.MaintenanceManage), Errors: errs,
	}, authed(func(ctx context.Context, p auth.Principal, in *ticketAssignInput) (*ticketOutput, error) {
		out, err := svc.Assign(ctx, p.Actor(), in.TicketID, in.Body)
		return &ticketOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "start-ticket", Method: http.MethodPost, Path: "/tickets/{ticket_id}/start", Summary: "Iniciar",
		Description: "Desde open o assigned. Sin asignado la toma quien la inicia; si es de otra persona, 403.",
		Middlewares: execute, Errors: append(errs, http.StatusForbidden),
	}, authed(func(ctx context.Context, p auth.Principal, in *ticketPath) (*ticketOutput, error) {
		out, err := svc.Start(ctx, p.Actor(), in.TicketID)
		return &ticketOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "resolve-ticket", Method: http.MethodPost, Path: "/tickets/{ticket_id}/resolve", Summary: "Resolver",
		Description: "Con lo que se hizo, costo y fotos. Si nacio de un preventivo, el plan avanza. Publica ticket.resolved.",
		Middlewares: execute, Errors: append(errs, http.StatusForbidden),
	}, authed(func(ctx context.Context, p auth.Principal, in *ticketResolveInput) (*ticketOutput, error) {
		out, err := svc.Resolve(ctx, p.Actor(), in.TicketID, in.Body)
		return &ticketOutput{Body: out}, err
	}))
	for _, op := range []struct {
		id, path, summary, doc string
		fn                     func(context.Context, authz.Actor, uuid.UUID, maintenance.TicketReason) (maintenance.TicketDetail, error)
	}{
		{"close-ticket", "close", "Cerrar", "Da por verificada una resuelta. reason es una nota opcional.", svc.Close},
		{"reject-ticket", "reject", "Rechazar", "Desde open o assigned (duplicada, no es falla). reason obligatorio.", svc.Reject},
		{"reopen-ticket", "reopen", "Reabrir", "Una resuelta o rechazada vuelve a open o assigned. reason obligatorio.", svc.Reopen},
	} {
		fn := op.fn
		huma.Register(grp, huma.Operation{
			OperationID: op.id, Method: http.MethodPost, Path: "/tickets/{ticket_id}/" + op.path, Summary: op.summary,
			Description: op.doc, Middlewares: manage, Errors: errs,
		}, authed(func(ctx context.Context, p auth.Principal, in *ticketReasonInput) (*ticketOutput, error) {
			out, err := fn(ctx, p.Actor(), in.TicketID, in.Body)
			return &ticketOutput{Body: out}, err
		}))
	}
}

// splitList separa una lista "a,b,c" de un query param.
func splitList(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}
