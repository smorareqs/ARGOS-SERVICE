package v1

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/api"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
)

type tenantPath struct {
	TenantID uuid.UUID `path:"tenant_id" format:"uuid"`
}

func (t tenantPath) tenant() uuid.UUID { return t.TenantID }

type memberPath struct {
	TenantID uuid.UUID `path:"tenant_id" format:"uuid"`
	MemberID uuid.UUID `path:"member_id" format:"uuid"`
}

func (m memberPath) tenant() uuid.UUID { return m.TenantID }

type membersOutput struct {
	Body page[authz.MemberRead]
}

type memberOutput struct {
	Body authz.MemberRead
}

type memberRoleInput struct {
	TenantID uuid.UUID `path:"tenant_id" format:"uuid"`
	MemberID uuid.UUID `path:"member_id" format:"uuid"`
	Body     authz.RoleChange
}

func (m memberRoleInput) tenant() uuid.UUID { return m.TenantID }

type invitationsInput struct {
	TenantID uuid.UUID `path:"tenant_id" format:"uuid"`
	Status   string    `query:"status" maxLength:"16" doc:"pending, accepted, expired o revoked; vacio = todas."`
}

func (i invitationsInput) tenant() uuid.UUID { return i.TenantID }

type invitationsOutput struct {
	Body page[auth.InvitationRead]
}

type invitationOutput struct {
	Body auth.InvitationRead
}

type inviteInput struct {
	TenantID uuid.UUID `path:"tenant_id" format:"uuid"`
	Body     auth.InvitationCreate
}

func (i inviteInput) tenant() uuid.UUID { return i.TenantID }

type invitationPath struct {
	TenantID     uuid.UUID `path:"tenant_id" format:"uuid"`
	InvitationID uuid.UUID `path:"invitation_id" format:"uuid"`
}

func (i invitationPath) tenant() uuid.UUID { return i.TenantID }

type rolesOutput struct {
	Body page[authz.RoleRead]
}

type roleOutput struct {
	Body authz.RoleRead
}

type roleCreateInput struct {
	TenantID uuid.UUID `path:"tenant_id" format:"uuid"`
	Body     authz.RoleCreate
}

func (r roleCreateInput) tenant() uuid.UUID { return r.TenantID }

type rolePath struct {
	TenantID uuid.UUID `path:"tenant_id" format:"uuid"`
	RoleID   uuid.UUID `path:"role_id" format:"uuid"`
}

func (r rolePath) tenant() uuid.UUID { return r.TenantID }

type roleUpdateInput struct {
	TenantID uuid.UUID `path:"tenant_id" format:"uuid"`
	RoleID   uuid.UUID `path:"role_id" format:"uuid"`
	Body     authz.RoleUpdate
}

func (r roleUpdateInput) tenant() uuid.UUID { return r.TenantID }

type settingsOutput struct {
	Body tenants.SettingsRead
}

type settingsInput struct {
	TenantID uuid.UUID `path:"tenant_id" format:"uuid"`
	Body     tenants.SettingsUpdate
}

func (s settingsInput) tenant() uuid.UUID { return s.TenantID }

func actorOf(p auth.Principal) authz.Actor {
	return p.Actor()
}

// Administracion de la empresa activa: miembros, invitaciones, roles y
// configuracion (fase01.md §3.7). Toda ruta /tenants/{tenant_id} fuera de la
// empresa de la sesion responde 404.
func registerTeam(v1 huma.API, authSvc *auth.Service, authzSvc *authz.Service, tenantSvc *tenants.Service, guard *api.Guard) {
	grp := huma.NewGroup(v1, "/tenants/{tenant_id}")
	grp.UseSimpleModifier(func(op *huma.Operation) {
		op.Tags = []string{"empresa"}
		op.Security = api.Secured
		op.Errors = append(op.Errors, http.StatusNotFound)
	})
	registerMembers(grp, authzSvc, guard)
	registerInvitations(grp, authSvc, guard)
	registerRoles(grp, authzSvc, guard)
	registerSettings(grp, tenantSvc, guard)
}

func registerMembers(grp huma.API, svc *authz.Service, guard *api.Guard) {
	huma.Register(grp, huma.Operation{
		OperationID: "list-members", Method: http.MethodGet, Path: "/members", Summary: "Personas de la empresa",
		Middlewares: guard.RequirePermission(authz.UsersRead),
	}, scoped(func(ctx context.Context, _ auth.Principal, in *tenantPath) (*membersOutput, error) {
		list, err := svc.Members(ctx, in.TenantID)
		return &membersOutput{Body: pageOf(list)}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "change-member-role", Method: http.MethodPatch, Path: "/members/{member_id}/role",
		Summary: "Cambiar el rol", Middlewares: guard.RequirePermission(authz.UsersManage),
		Description: "Debe quedar al menos un administrador activo; no se dan roles con permisos que uno no tiene.",
		Errors:      []int{http.StatusUnprocessableEntity},
	}, scoped(func(ctx context.Context, p auth.Principal, in *memberRoleInput) (*memberOutput, error) {
		out, err := svc.ChangeMemberRole(ctx, actorOf(p), in.TenantID, in.MemberID, in.Body.RoleKey)
		return &memberOutput{Body: out}, err
	}))

	for _, action := range []struct {
		id, path, summary string
		suspend           bool
	}{
		{"suspend-member", "/members/{member_id}/suspend", "Suspender (cierra sus sesiones en la empresa)", true},
		{"reactivate-member", "/members/{member_id}/reactivate", "Reactivar", false},
	} {
		huma.Register(grp, huma.Operation{
			OperationID: action.id, Method: http.MethodPost, Path: action.path, Summary: action.summary,
			Middlewares: guard.RequirePermission(authz.UsersManage), Errors: []int{http.StatusUnprocessableEntity},
		}, scoped(func(ctx context.Context, p auth.Principal, in *memberPath) (*memberOutput, error) {
			out, err := svc.SetMemberSuspended(ctx, actorOf(p), in.TenantID, in.MemberID, action.suspend)
			return &memberOutput{Body: out}, err
		}))
	}

	huma.Register(grp, huma.Operation{
		OperationID: "revoke-member-sessions", Method: http.MethodDelete, Path: "/members/{member_id}/sessions",
		DefaultStatus: http.StatusNoContent, Summary: "Cerrar sus sesiones en la empresa",
		Middlewares: guard.RequirePermission(authz.UsersManage),
	}, scoped(func(ctx context.Context, p auth.Principal, in *memberPath) (*struct{}, error) {
		return noContent(svc.RevokeMemberSessions(ctx, actorOf(p), in.TenantID, in.MemberID))
	}))
}

func registerInvitations(grp huma.API, svc *auth.Service, guard *api.Guard) {
	huma.Register(grp, huma.Operation{
		OperationID: "list-invitations", Method: http.MethodGet, Path: "/invitations", Summary: "Invitaciones",
		Description: "`status=pending` excluye las vencidas.", Middlewares: guard.RequirePermission(authz.UsersRead),
	}, scoped(func(ctx context.Context, _ auth.Principal, in *invitationsInput) (*invitationsOutput, error) {
		list, err := svc.Invitations(ctx, in.TenantID, in.Status)
		return &invitationsOutput{Body: pageOf(list)}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "create-invitation", Method: http.MethodPost, Path: "/invitations",
		DefaultStatus: http.StatusCreated, Summary: "Invitar por correo",
		Description: "422 en body.email si ya es miembro o tiene una invitacion pendiente.",
		Middlewares: guard.RequirePermission(authz.UsersManage), Errors: []int{http.StatusUnprocessableEntity},
	}, scoped(func(ctx context.Context, p auth.Principal, in *inviteInput) (*invitationOutput, error) {
		out, err := svc.Invite(ctx, p, in.TenantID, in.Body)
		return &invitationOutput{Body: out}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "resend-invitation", Method: http.MethodPost, Path: "/invitations/{invitation_id}/resend",
		Summary: "Reenviar (el enlace anterior deja de servir)", Middlewares: guard.RequirePermission(authz.UsersManage),
		Errors: []int{http.StatusUnprocessableEntity},
	}, scoped(func(ctx context.Context, p auth.Principal, in *invitationPath) (*invitationOutput, error) {
		out, err := svc.ResendInvitation(ctx, p, in.TenantID, in.InvitationID)
		return &invitationOutput{Body: out}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "revoke-invitation", Method: http.MethodDelete, Path: "/invitations/{invitation_id}",
		DefaultStatus: http.StatusNoContent, Summary: "Cancelar", Middlewares: guard.RequirePermission(authz.UsersManage),
	}, scoped(func(ctx context.Context, p auth.Principal, in *invitationPath) (*struct{}, error) {
		return noContent(svc.RevokeInvitation(ctx, p, in.TenantID, in.InvitationID))
	}))
}

func registerRoles(grp huma.API, svc *authz.Service, guard *api.Guard) {
	huma.Register(grp, huma.Operation{
		OperationID: "list-roles", Method: http.MethodGet, Path: "/roles", Summary: "Roles con sus permisos",
		Middlewares: guard.RequireAnyPermission(authz.UsersRead, authz.RolesManage),
	}, scoped(func(ctx context.Context, _ auth.Principal, in *tenantPath) (*rolesOutput, error) {
		list, err := svc.Roles(ctx, in.TenantID)
		return &rolesOutput{Body: pageOf(list)}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "create-role", Method: http.MethodPost, Path: "/roles", DefaultStatus: http.StatusCreated,
		Summary: "Crear rol personalizado", Middlewares: guard.RequirePermission(authz.RolesManage),
		Errors: []int{http.StatusUnprocessableEntity},
	}, scoped(func(ctx context.Context, p auth.Principal, in *roleCreateInput) (*roleOutput, error) {
		out, err := svc.CreateRole(ctx, actorOf(p), in.TenantID, in.Body)
		return &roleOutput{Body: out}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "update-role", Method: http.MethodPatch, Path: "/roles/{role_id}",
		Summary: "Editar rol personalizado", Description: "Los roles de sistema son de solo lectura (422).",
		Middlewares: guard.RequirePermission(authz.RolesManage), Errors: []int{http.StatusUnprocessableEntity},
	}, scoped(func(ctx context.Context, p auth.Principal, in *roleUpdateInput) (*roleOutput, error) {
		out, err := svc.UpdateRole(ctx, actorOf(p), in.TenantID, in.RoleID, in.Body)
		return &roleOutput{Body: out}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "delete-role", Method: http.MethodDelete, Path: "/roles/{role_id}",
		DefaultStatus: http.StatusNoContent, Summary: "Borrar rol personalizado sin personas",
		Middlewares: guard.RequirePermission(authz.RolesManage), Errors: []int{http.StatusUnprocessableEntity},
	}, scoped(func(ctx context.Context, p auth.Principal, in *rolePath) (*struct{}, error) {
		return noContent(svc.DeleteRole(ctx, actorOf(p), in.TenantID, in.RoleID))
	}))
}

func registerSettings(grp huma.API, svc *tenants.Service, guard *api.Guard) {
	huma.Register(grp, huma.Operation{
		OperationID: "get-tenant-settings", Method: http.MethodGet, Path: "/settings", Summary: "Configuracion de la empresa",
		Middlewares: guard.RequirePermission(authz.TenantSettings),
	}, scoped(func(ctx context.Context, _ auth.Principal, in *tenantPath) (*settingsOutput, error) {
		out, err := svc.Settings(ctx, in.TenantID)
		return &settingsOutput{Body: out}, err
	}))

	huma.Register(grp, huma.Operation{
		OperationID: "update-tenant-settings", Method: http.MethodPatch, Path: "/settings",
		Summary: "Cambiar la configuracion", Description: "Las banderas son de solo lectura (las define el plan).",
		Middlewares: guard.RequirePermission(authz.TenantSettings), Errors: []int{http.StatusUnprocessableEntity},
	}, scoped(func(ctx context.Context, p auth.Principal, in *settingsInput) (*settingsOutput, error) {
		out, err := svc.UpdateSettings(ctx, p.User.ID, in.TenantID, in.Body)
		return &settingsOutput{Body: out}, err
	}))
}
