package authz

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
)

// Service contiene los casos de uso de autorizacion.
type Service struct {
	db  *database.DB
	log *slog.Logger
}

// NewService crea el servicio.
func NewService(db *database.DB, log *slog.Logger) *Service {
	return &Service{db: db, log: log}
}

// SeedSystemRoles deja los siete roles de sistema del tenant con los permisos
// de SystemRoles. Opera sobre la transaccion del llamador (el alta de tenant la
// incluye en su transaccion atomica) y es idempotente: solo reescribe, y sube
// la version de permisos, de los roles cuyos permisos cambiaron.
func SeedSystemRoles(ctx context.Context, q database.Querier, tenantID uuid.UUID) (changed int, err error) {
	repo := NewRepository(q)
	for _, def := range SystemRoles {
		role, err := repo.UpsertSystemRole(ctx, tenantID, def)
		if err != nil {
			return changed, err
		}
		current, err := repo.Permissions(ctx, role.ID)
		if err != nil {
			return changed, err
		}
		if samePermissions(current, def.Permissions) {
			continue
		}
		if err := repo.ReplacePermissions(ctx, role.ID, def.Permissions); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}

// SyncAllTenants aplica SeedSystemRoles a todos los tenants (cada uno en su
// transaccion). Lo ejecuta `argos migrate up` despues de las migraciones: asi
// un cambio en la matriz llega a todos los tenants existentes.
func (s *Service) SyncAllTenants(ctx context.Context) error {
	ids, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]uuid.UUID, error) {
		return NewRepository(q).TenantIDs(ctx)
	})
	if err != nil {
		return err
	}
	for _, id := range ids {
		changed, err := database.InTxValue(ctx, s.db, func(q database.Querier) (int, error) {
			return SeedSystemRoles(ctx, q, id)
		})
		if err != nil {
			return err
		}
		if changed > 0 {
			s.log.InfoContext(ctx, "Roles de sistema actualizados", "tenant_id", id, "roles", changed)
		}
	}
	return nil
}

// AddMember crea (o actualiza) la membresia activa del usuario en el tenant con
// el rol de esa clave.
func AddMember(ctx context.Context, q database.Querier, userID, tenantID uuid.UUID, key RoleKey) error {
	repo := NewRepository(q)
	role, err := repo.RoleByKey(ctx, tenantID, key)
	if err != nil {
		return err
	}
	if role == nil {
		return ErrRoleNotFound
	}
	now := time.Now()
	_, err = repo.UpsertMembership(ctx, Membership{
		UserID: userID, TenantID: tenantID, RoleID: role.ID, Status: MembershipActive, JoinedAt: &now,
	})
	return err
}

func samePermissions(a, b []Permission) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}
