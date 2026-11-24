package authz

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/audit"
)

// Administracion de roles y miembros de una empresa (fase01.md §3.7:
// /tenants/{id}/roles y /tenants/{id}/members).
//
// Decision (2026-11-21): los siete roles de sistema son de solo lectura en
// cada empresa; su matriz la mantiene ARGOS y `migrate up` la sincroniza. Las
// empresas ajustan permisos creando roles personalizados (p. ej. duplicando
// uno de sistema).

// RoleRead es un rol en la administracion de la empresa.
type RoleRead struct {
	ID          uuid.UUID    `json:"id" format:"uuid"`
	Key         RoleKey      `json:"key"`
	Name        string       `json:"name"`
	Description *string      `json:"description" nullable:"true"`
	IsSystem    bool         `json:"is_system"`
	Scoped      bool         `json:"scoped" doc:"true si solo ve sus propiedades asignadas o propias."`
	Members     int          `json:"members"`
	Permissions []Permission `json:"permissions"`
}

// RoleCreate es el cuerpo de POST /tenants/{id}/roles.
type RoleCreate struct {
	_           struct{}     `additionalProperties:"true"`
	Name        string       `json:"name" minLength:"1" maxLength:"128"`
	Description *string      `json:"description,omitempty" maxLength:"500" nullable:"true"`
	Permissions []Permission `json:"permissions" maxItems:"100"`
	Scoped      bool         `json:"scoped,omitempty" doc:"true = solo ve sus propiedades asignadas o propias."`
}

// RoleUpdate es el cuerpo de PATCH /tenants/{id}/roles/{role_id}.
type RoleUpdate struct {
	_           struct{}      `additionalProperties:"true"`
	Name        *string       `json:"name,omitempty" maxLength:"128"`
	Description *string       `json:"description,omitempty" maxLength:"500" nullable:"true"`
	Permissions *[]Permission `json:"permissions,omitempty" maxItems:"100"`
	Scoped      *bool         `json:"scoped,omitempty"`
}

// MemberRead es una persona de la empresa (W-D1).
type MemberRead struct {
	ID              uuid.UUID        `json:"id" format:"uuid" doc:"Id de la membresia (no del usuario)."`
	UserID          uuid.UUID        `json:"user_id" format:"uuid"`
	FullName        string           `json:"full_name"`
	Email           string           `json:"email"`
	Phone           *string          `json:"phone" nullable:"true"`
	RoleID          uuid.UUID        `json:"role_id" format:"uuid"`
	RoleKey         RoleKey          `json:"role_key"`
	RoleName        string           `json:"role_name"`
	Status          MembershipStatus `json:"status" enum:"active,suspended,invited"`
	TOTPEnabled     bool             `json:"totp_enabled"`
	PropertiesCount int              `json:"properties_count" doc:"Propiedades asignadas o propias."`
	LastSeenAt      *time.Time       `json:"last_seen_at" nullable:"true"`
	JoinedAt        *time.Time       `json:"joined_at" nullable:"true"`
}

// RoleChange es el cuerpo de PATCH /tenants/{id}/members/{member_id}/role.
type RoleChange struct {
	_       struct{} `additionalProperties:"true"`
	RoleKey RoleKey  `json:"role_key" minLength:"1" maxLength:"64"`
}

// Actor es quien hace un cambio: su usuario, sus permisos en la empresa y si
// su rol solo ve sus propiedades.
type Actor struct {
	UserID      uuid.UUID
	TenantID    uuid.UUID // empresa activa de la sesion
	Permissions map[Permission]bool
	Scoped      bool
}

// CanGrant indica si el actor puede dar estos permisos: solo los que tiene,
// salvo los de alcance propio (*.own), que no dan acceso a datos de otros.
func (a Actor) CanGrant(perms []Permission) bool {
	for _, p := range perms {
		if !a.Permissions[p] && !strings.HasSuffix(string(p), ".own") {
			return false
		}
	}
	return true
}

// CanGrantRole indica si el actor puede dar un rol: sus permisos (CanGrant) y,
// si el actor solo ve sus propiedades, que el rol tambien.
func (a Actor) CanGrantRole(perms []Permission, scoped bool) bool {
	return a.CanGrant(perms) && (!a.Scoped || scoped)
}

// --- Roles -----------------------------------------------------------------

// Roles lista los roles de la empresa con sus permisos.
func (s *Service) Roles(ctx context.Context, tenantID uuid.UUID) ([]RoleRead, error) {
	list, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]RoleSummary, error) {
		return NewRepository(q).Roles(ctx, tenantID)
	})
	if err != nil {
		return nil, err
	}
	out := make([]RoleRead, 0, len(list))
	for _, r := range list {
		out = append(out, newRoleRead(r))
	}
	return out, nil
}

// CreateRole crea un rol personalizado.
func (s *Service) CreateRole(ctx context.Context, actor Actor, tenantID uuid.UUID, in RoleCreate) (RoleRead, error) {
	name, err := roleName(in.Name)
	if err != nil {
		return RoleRead{}, err
	}
	perms, err := validPermissions(actor, in.Permissions)
	if err != nil {
		return RoleRead{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (RoleRead, error) {
		repo := NewRepository(q)
		taken, err := repo.RoleNameExists(ctx, tenantID, name, nil)
		if err != nil {
			return RoleRead{}, err
		}
		if taken {
			return RoleRead{}, ErrRoleNameTaken.WithField("body.name")
		}
		key, err := uniqueRoleKey(ctx, repo, tenantID, name)
		if err != nil {
			return RoleRead{}, err
		}
		if actor.Scoped && !in.Scoped {
			return RoleRead{}, ErrCannotGrant.WithField("body.scoped")
		}
		id, err := repo.AddRole(ctx, tenantID, key, name, cleanDescription(in.Description), in.Scoped)
		if err != nil {
			return RoleRead{}, err
		}
		if err := repo.ReplacePermissions(ctx, id, perms); err != nil {
			return RoleRead{}, err
		}
		role, err := repo.RoleSummaryByID(ctx, tenantID, id)
		if err != nil {
			return RoleRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "role.created", Entity: "role", EntityID: &id,
			After: map[string]any{"name": name, "permissions": perms, "scoped": in.Scoped},
		}); err != nil {
			return RoleRead{}, err
		}
		return newRoleRead(*role), nil
	})
}

// UpdateRole edita un rol personalizado. Cambiar permisos sube su version: los
// access tokens de quienes lo tienen dejan de valer en la siguiente peticion.
func (s *Service) UpdateRole(ctx context.Context, actor Actor, tenantID, roleID uuid.UUID, in RoleUpdate) (RoleRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (RoleRead, error) {
		repo := NewRepository(q)
		role, err := repo.RoleSummaryByID(ctx, tenantID, roleID)
		if err != nil {
			return RoleRead{}, err
		}
		if role == nil || role.Key == RoleGuest {
			return RoleRead{}, ErrRoleNotFound
		}
		if role.IsSystem {
			return RoleRead{}, ErrSystemRoleLocked
		}
		before := map[string]any{"name": role.Name, "permissions": role.Permissions, "scoped": role.Scoped}
		if err := applyRoleUpdate(ctx, repo, actor, *role, in); err != nil {
			return RoleRead{}, err
		}
		updated, err := repo.RoleSummaryByID(ctx, tenantID, roleID)
		if err != nil {
			return RoleRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "role.updated", Entity: "role", EntityID: &roleID,
			Before: before, After: map[string]any{"name": updated.Name, "permissions": updated.Permissions, "scoped": updated.Scoped},
		}); err != nil {
			return RoleRead{}, err
		}
		return newRoleRead(*updated), nil
	})
}

// DeleteRole borra un rol personalizado sin personas ni invitaciones.
func (s *Service) DeleteRole(ctx context.Context, actor Actor, tenantID, roleID uuid.UUID) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		repo := NewRepository(q)
		role, err := repo.RoleSummaryByID(ctx, tenantID, roleID)
		if err != nil {
			return err
		}
		if role == nil || role.Key == RoleGuest {
			return ErrRoleNotFound
		}
		if role.IsSystem {
			return ErrSystemRoleLocked
		}
		pending, err := repo.PendingInvitationsWithRole(ctx, roleID)
		if err != nil {
			return err
		}
		if role.Members > 0 || pending > 0 {
			return ErrRoleInUse
		}
		if err := repo.DeleteRole(ctx, roleID); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "role.deleted", Entity: "role", EntityID: &roleID,
			Before: map[string]any{"name": role.Name, "permissions": role.Permissions},
		})
	})
}

// CanGrantKey indica si el actor puede dar el rol de esa clave (lo usa la
// invitacion). ErrRoleNotFound si no existe.
func CanGrantKey(ctx context.Context, q database.Querier, actor Actor, tenantID uuid.UUID, key RoleKey) (bool, error) {
	repo := NewRepository(q)
	role, err := repo.RoleByKey(ctx, tenantID, key)
	if err != nil {
		return false, err
	}
	if role == nil {
		return false, ErrRoleNotFound
	}
	perms, err := repo.Permissions(ctx, role.ID)
	if err != nil {
		return false, err
	}
	return actor.CanGrantRole(perms, role.Scoped), nil
}

// --- Miembros --------------------------------------------------------------

// Members lista las personas de la empresa.
func (s *Service) Members(ctx context.Context, tenantID uuid.UUID) ([]MemberRead, error) {
	list, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]MemberRow, error) {
		return NewRepository(q).Members(ctx, tenantID)
	})
	if err != nil {
		return nil, err
	}
	out := make([]MemberRead, 0, len(list))
	for _, m := range list {
		out = append(out, newMemberRead(m))
	}
	return out, nil
}

// ChangeMemberRole cambia el rol de una persona. Debe quedar al menos un
// administrador activo y nadie da un rol con permisos que no tiene.
func (s *Service) ChangeMemberRole(ctx context.Context, actor Actor, tenantID, memberID uuid.UUID, key RoleKey) (MemberRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (MemberRead, error) {
		repo := NewRepository(q)
		member, err := repo.MemberByID(ctx, tenantID, memberID)
		if err != nil {
			return MemberRead{}, err
		}
		if member == nil {
			return MemberRead{}, ErrMemberNotFound
		}
		role, err := grantableRole(ctx, repo, actor, tenantID, key)
		if err != nil {
			return MemberRead{}, err
		}
		if role.ID == member.RoleID {
			return newMemberRead(*member), nil
		}
		if member.RoleKey == RoleAdmin && member.Status == MembershipActive {
			if err := s.keepOneAdmin(ctx, repo, tenantID); err != nil {
				return MemberRead{}, err
			}
		}
		if err := repo.SetMemberRole(ctx, memberID, role.ID); err != nil {
			return MemberRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "member.role_changed", Entity: "membership", EntityID: &memberID,
			Before: map[string]any{"role_key": member.RoleKey}, After: map[string]any{"role_key": role.Key},
		}); err != nil {
			return MemberRead{}, err
		}
		return s.member(ctx, repo, tenantID, memberID)
	})
}

// SetMemberSuspended suspende o reactiva a una persona. Al suspender se
// cierran sus sesiones en la empresa; su acceso termina en la siguiente
// peticion porque cada peticion verifica la membresia.
func (s *Service) SetMemberSuspended(ctx context.Context, actor Actor, tenantID, memberID uuid.UUID, suspended bool) (MemberRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (MemberRead, error) {
		repo := NewRepository(q)
		member, err := repo.MemberByID(ctx, tenantID, memberID)
		if err != nil {
			return MemberRead{}, err
		}
		if member == nil {
			return MemberRead{}, ErrMemberNotFound
		}
		status := MembershipActive
		if suspended {
			status = MembershipSuspended
		}
		if member.Status == status {
			return newMemberRead(*member), nil
		}
		if suspended {
			if err := s.prepareSuspension(ctx, repo, actor, tenantID, *member); err != nil {
				return MemberRead{}, err
			}
		}
		if err := repo.SetMemberStatus(ctx, memberID, status); err != nil {
			return MemberRead{}, err
		}
		action := "member.reactivated"
		if suspended {
			action = "member.suspended"
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: action, Entity: "membership", EntityID: &memberID,
			Before: map[string]any{"status": member.Status}, After: map[string]any{"status": status},
		}); err != nil {
			return MemberRead{}, err
		}
		return s.member(ctx, repo, tenantID, memberID)
	})
}

// RevokeMemberSessions cierra las sesiones de una persona en la empresa (en
// otras empresas sigue conectada).
func (s *Service) RevokeMemberSessions(ctx context.Context, actor Actor, tenantID, memberID uuid.UUID) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		repo := NewRepository(q)
		member, err := repo.MemberByID(ctx, tenantID, memberID)
		if err != nil {
			return err
		}
		if member == nil {
			return ErrMemberNotFound
		}
		if err := repo.RevokeTenantSessions(ctx, member.UserID, tenantID); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "member.sessions_revoked", Entity: "membership", EntityID: &memberID,
		})
	})
}

// applyRoleUpdate guarda nombre, descripcion y permisos de un rol
// personalizado.
func applyRoleUpdate(ctx context.Context, repo *Repository, actor Actor, role RoleSummary, in RoleUpdate) error {
	name, description := role.Name, role.Description
	if in.Name != nil {
		var err error
		if name, err = roleName(*in.Name); err != nil {
			return err
		}
		taken, err := repo.RoleNameExists(ctx, role.TenantID, name, &role.ID)
		if err != nil {
			return err
		}
		if taken {
			return ErrRoleNameTaken.WithField("body.name")
		}
	}
	if in.Description != nil {
		description = cleanDescription(in.Description)
	}
	scoped, err := roleScope(actor, role, in)
	if err != nil {
		return err
	}
	if err := repo.UpdateRole(ctx, role.ID, name, description, scoped); err != nil {
		return err
	}
	if in.Permissions == nil {
		return nil
	}
	perms, err := validPermissions(actor, *in.Permissions)
	if err != nil || samePermissions(perms, role.Permissions) {
		return err
	}
	return repo.ReplacePermissions(ctx, role.ID, perms)
}

// roleScope es el alcance que queda en el rol: quien solo ve sus propiedades
// no crea roles que ven todas.
func roleScope(actor Actor, role RoleSummary, in RoleUpdate) (bool, error) {
	scoped := role.Scoped
	if in.Scoped != nil {
		scoped = *in.Scoped
	}
	if actor.Scoped && !scoped {
		return false, ErrCannotGrant.WithField("body.scoped")
	}
	return scoped, nil
}

// grantableRole devuelve el rol de esa clave si el actor puede darlo.
func grantableRole(ctx context.Context, repo *Repository, actor Actor, tenantID uuid.UUID, key RoleKey) (*Role, error) {
	if key == RoleGuest {
		return nil, ErrGuestRoleNotAllowed.WithField("body.role_key")
	}
	role, err := repo.RoleByKey(ctx, tenantID, key)
	if err != nil {
		return nil, err
	}
	if role == nil {
		return nil, ErrRoleNotFound.WithField("body.role_key")
	}
	perms, err := repo.Permissions(ctx, role.ID)
	if err != nil {
		return nil, err
	}
	if !actor.CanGrantRole(perms, role.Scoped) {
		return nil, ErrCannotGrant.WithField("body.role_key")
	}
	return role, nil
}

// prepareSuspension valida la suspension y cierra las sesiones en la empresa.
func (s *Service) prepareSuspension(ctx context.Context, repo *Repository, actor Actor, tenantID uuid.UUID, member MemberRow) error {
	if member.UserID == actor.UserID {
		return ErrSelfSuspend
	}
	if member.RoleKey == RoleAdmin {
		if err := s.keepOneAdmin(ctx, repo, tenantID); err != nil {
			return err
		}
	}
	return repo.RevokeTenantSessions(ctx, member.UserID, tenantID)
}

// keepOneAdmin falla si el cambio dejaria la empresa sin administrador activo.
func (s *Service) keepOneAdmin(ctx context.Context, repo *Repository, tenantID uuid.UUID) error {
	n, err := repo.ActiveAdmins(ctx, tenantID)
	if err != nil {
		return err
	}
	if n <= 1 {
		return ErrLastAdmin
	}
	return nil
}

func (s *Service) member(ctx context.Context, repo *Repository, tenantID, id uuid.UUID) (MemberRead, error) {
	m, err := repo.MemberByID(ctx, tenantID, id)
	if err != nil {
		return MemberRead{}, err
	}
	if m == nil {
		return MemberRead{}, ErrMemberNotFound
	}
	return newMemberRead(*m), nil
}

// --- Utilidades ------------------------------------------------------------

func newRoleRead(r RoleSummary) RoleRead {
	perms := r.Permissions
	if perms == nil {
		perms = []Permission{}
	}
	return RoleRead{
		ID: r.ID, Key: r.Key, Name: r.Name, Description: r.Description, IsSystem: r.IsSystem,
		Scoped: r.Scoped, Members: r.Members, Permissions: perms,
	}
}

func newMemberRead(m MemberRow) MemberRead {
	status := m.Status
	if !m.UserActive {
		status = MembershipSuspended
	}
	return MemberRead{
		ID: m.ID, UserID: m.UserID, FullName: m.FullName, Email: m.Email, Phone: m.Phone,
		RoleID: m.RoleID, RoleKey: m.RoleKey, RoleName: m.RoleName, Status: status, TOTPEnabled: m.TOTPEnabled,
		PropertiesCount: m.PropertiesCount, LastSeenAt: m.LastSeenAt, JoinedAt: m.JoinedAt,
	}
}

func roleName(raw string) (string, error) {
	name := strings.Join(strings.Fields(raw), " ")
	if n := utf8.RuneCountInString(name); n < 2 || n > 128 {
		return "", ErrInvalidRoleName.WithField("body.name")
	}
	return name, nil
}

func cleanDescription(d *string) *string {
	if d == nil {
		return nil
	}
	v := strings.TrimSpace(*d)
	if v == "" {
		return nil
	}
	return &v
}

func validPermissions(actor Actor, in []Permission) ([]Permission, error) {
	out := make([]Permission, 0, len(in))
	for _, p := range in {
		if !p.Valid() {
			return nil, ErrInvalidPermission.WithField("body.permissions")
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, ErrRoleWithoutPerms.WithField("body.permissions")
	}
	if !actor.CanGrant(out) {
		return nil, ErrCannotGrant.WithField("body.permissions")
	}
	slices.Sort(out)
	return out, nil
}

// uniqueRoleKey deriva la clave del nombre ("Supervisor de noche" ->
// "supervisor_de_noche") sin chocar con los de sistema ni con otros.
func uniqueRoleKey(ctx context.Context, repo *Repository, tenantID uuid.UUID, name string) (RoleKey, error) {
	base := slugify(name)
	if base == "" {
		base = "rol"
	}
	for i := 1; i < 100; i++ {
		key := RoleKey(base)
		if i > 1 {
			key = RoleKey(fmt.Sprintf("%s_%d", base, i))
		}
		if _, system := SystemRoleByKey(key); system {
			continue
		}
		exists, err := repo.RoleKeyExists(ctx, tenantID, key)
		if err != nil {
			return "", err
		}
		if !exists {
			return key, nil
		}
	}
	return RoleKey("rol_" + uuid.NewString()[:8]), nil
}

func slugify(s string) string {
	var b strings.Builder
	underscore := false
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			underscore = false
		case !underscore && b.Len() > 0:
			b.WriteByte('_')
			underscore = true
		}
		if b.Len() >= 48 {
			break
		}
	}
	return strings.TrimRight(b.String(), "_")
}
