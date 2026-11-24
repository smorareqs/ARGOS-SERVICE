package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/billing"
)

// Facturacion de empresas (fase06.md §3.3). La empresa paga con tenant.billing
// (sigue funcionando en solo lectura); la plataforma administra planes y
// suscripciones.

type (
	// InvoiceParam es el id de la factura en la ruta.
	InvoiceParam struct {
		InvoiceID uuid.UUID `path:"invoice_id" format:"uuid"`
	}
	// PlanParam es el id del plan en la ruta.
	PlanParam struct {
		PlanID uuid.UUID `path:"plan_id" format:"uuid"`
	}
	// SubscriptionParam es el id de la suscripcion en la ruta.
	SubscriptionParam struct {
		SubscriptionID uuid.UUID `path:"subscription_id" format:"uuid"`
	}
)

type (
	billingOverviewOutput struct{ Body billing.Overview }
	billingInvoicesOutput struct {
		Body page[billing.InvoiceRead]
	}
	billingCheckoutInput  struct{ Body billing.CheckoutInput }
	billingCheckoutOutput struct{ Body billing.CheckoutResult }
	billingPlansOutput    struct {
		Body page[billing.Plan]
	}
	billingPlanOutput      struct{ Body billing.Plan }
	billingPlanCreateInput struct{ Body billing.NewPlan }
	subscriptionsOutput    struct {
		Body page[billing.SubscriptionRead]
	}
	subscriptionOutput struct{ Body billing.SubscriptionRead }
)

func billingFile(f billing.File) *downloadOutput {
	return &downloadOutput{
		ContentType: f.ContentType, ContentDisposition: `attachment; filename="` + f.Name + `"`,
		CacheControl: "no-store", Body: f.Body,
	}
}

func registerBilling(v1 huma.API, svc *billing.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1, "/billing")
	grp.UseMiddleware(guard.RequirePermission(authz.TenantBilling)...)
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"facturacion"}
		op.Security = api.Secured
	})
	registerBillingTenant(grp, svc)

	platform := huma.NewGroup(v1, "/platform")
	platform.UseMiddleware(guard.RequirePlatformAdmin()...)
	platform.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"plataforma"}
		op.Security = api.Secured
	})
	registerBillingPlans(platform, svc)
	registerBillingPlatform(platform, svc)
}

func registerBillingTenant(grp huma.API, svc *billing.Service) {
	huma.Register(grp, huma.Operation{
		OperationID: "billing-subscription", Method: http.MethodGet, Path: "/subscription", Summary: "Plan, uso y suscripcion",
		Description: "Plan actual con sus limites, uso, estado, factura pendiente y planes disponibles (W-O4).",
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*billingOverviewOutput, error) {
		out, err := svc.Overview(ctx, p.Membership.TenantID)
		return &billingOverviewOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "billing-invoices", Method: http.MethodGet, Path: "/invoices", Summary: "Facturas",
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*billingInvoicesOutput, error) {
		list, err := svc.Invoices(ctx, p.Membership.TenantID)
		return &billingInvoicesOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "billing-invoice-pdf", Method: http.MethodGet, Path: "/invoices/{invoice_id}/pdf", Summary: "Factura en PDF",
		Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *InvoiceParam) (*downloadOutput, error) {
		f, err := svc.InvoicePDF(ctx, p.Membership.TenantID, in.InvoiceID)
		if err != nil {
			return nil, err
		}
		return billingFile(f), nil
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "billing-checkout", Method: http.MethodPost, Path: "/checkout", Summary: "Pagar o cambiar de plan",
		Description: "Con plan_key o cycle cambia de plan desde hoy (anula lo pendiente y emite la factura nueva); " +
			"luego cobra la factura pendiente con la pasarela. Una empresa suspendida vuelve a operar al pagar. " +
			"422 si el uso no cabe en el plan nuevo o si no hay nada que pagar.",
		Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *billingCheckoutInput) (*billingCheckoutOutput, error) {
		out, err := svc.Checkout(ctx, p.Actor(), in.Body)
		return &billingCheckoutOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "billing-cancel", Method: http.MethodPost, Path: "/cancel", Summary: "Cancelar al terminar el periodo",
		Description: "La empresa sigue operando hasta current_period_end; despues queda en solo lectura.",
		Errors:      []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*billingOverviewOutput, error) {
		out, err := svc.Cancel(ctx, p.Actor())
		return &billingOverviewOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "billing-resume", Method: http.MethodPost, Path: "/resume", Summary: "Deshacer la cancelacion",
		Errors: []int{http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, _ *struct{}) (*billingOverviewOutput, error) {
		out, err := svc.Resume(ctx, p.Actor())
		return &billingOverviewOutput{Body: out}, err
	}))
}

func registerBillingPlans(grp huma.API, svc *billing.Service) {
	huma.Register(grp, huma.Operation{
		OperationID: "platform-plans", Method: http.MethodGet, Path: "/plans", Summary: "Planes (W-P2)",
	}, authed(func(ctx context.Context, _ auth.Principal, _ *struct{}) (*billingPlansOutput, error) {
		list, err := svc.Plans(ctx, false)
		return &billingPlansOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "platform-create-plan", Method: http.MethodPost, Path: "/plans", Summary: "Nuevo plan",
		DefaultStatus: http.StatusCreated, Errors: []int{http.StatusConflict, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *billingPlanCreateInput) (*billingPlanOutput, error) {
		out, err := svc.CreatePlan(ctx, p.User.ID, in.Body)
		return &billingPlanOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "platform-update-plan", Method: http.MethodPatch, Path: "/plans/{plan_id}", Summary: "Cambiar un plan",
		Description: "Los limites nuevos aplican a la siguiente alta o invitacion; no quitan nada a quien ya lo tiene.",
		Errors:      []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		PlanParam
		Body billing.PlanChanges
	},
	) (*billingPlanOutput, error) {
		out, err := svc.UpdatePlan(ctx, p.User.ID, in.PlanID, in.Body)
		return &billingPlanOutput{Body: out}, err
	}))
}

func registerBillingPlatform(grp huma.API, svc *billing.Service) {
	huma.Register(grp, huma.Operation{
		OperationID: "platform-subscriptions", Method: http.MethodGet, Path: "/subscriptions", Summary: "Suscripciones (W-P3)",
		Description: "Suspendidas y vencidas primero, con la factura pendiente y los cobros fallidos de 90 dias.",
	}, authed(func(ctx context.Context, _ auth.Principal, in *struct {
		Status string `query:"status" maxLength:"16" doc:"trialing, active, past_due, suspended o cancelled; vacío = todas."`
	},
	) (*subscriptionsOutput, error) {
		list, err := svc.Subscriptions(ctx, billing.SubscriptionFilter{Status: in.Status})
		return &subscriptionsOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "platform-reactivate-subscription", Method: http.MethodPost, Path: "/subscriptions/{subscription_id}/reactivate",
		Summary: "Reactivar", Description: "Vuelve a activa una suscripcion suspendida o cancelada; lo pendiente vence en `days` dias.",
		Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		SubscriptionParam
		Body billing.ReactivateInput
	},
	) (*subscriptionOutput, error) {
		out, err := svc.Reactivate(ctx, p.User.ID, in.SubscriptionID, in.Body)
		return &subscriptionOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "platform-tenant-trial", Method: http.MethodPost, Path: "/tenants/{tenant_id}/trial", Summary: "Dar prueba",
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, authed(func(ctx context.Context, p auth.Principal, in *struct {
		TenantID uuid.UUID `path:"tenant_id" format:"uuid"`
		Body     billing.TrialInput
	},
	) (*subscriptionOutput, error) {
		out, err := svc.StartTrial(ctx, p.User.ID, in.TenantID, in.Body)
		return &subscriptionOutput{Body: out}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "platform-tenant-invoices", Method: http.MethodGet, Path: "/tenants/{tenant_id}/invoices", Summary: "Facturas de una empresa",
	}, authed(func(ctx context.Context, _ auth.Principal, in *struct {
		TenantID uuid.UUID `path:"tenant_id" format:"uuid"`
	},
	) (*billingInvoicesOutput, error) {
		list, err := svc.TenantInvoices(ctx, in.TenantID)
		return &billingInvoicesOutput{Body: pageOf(list)}, err
	}))
	huma.Register(grp, huma.Operation{
		OperationID: "platform-invoice-pdf", Method: http.MethodGet, Path: "/invoices/{invoice_id}/pdf", Summary: "Factura en PDF",
		Errors: []int{http.StatusNotFound},
	}, authed(func(ctx context.Context, _ auth.Principal, in *InvoiceParam) (*downloadOutput, error) {
		f, err := svc.InvoicePDF(ctx, uuid.Nil, in.InvoiceID)
		if err != nil {
			return nil, err
		}
		return billingFile(f), nil
	}))
}
