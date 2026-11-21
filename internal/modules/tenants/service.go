package tenants

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/migrate"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Service contiene los casos de uso de tenants. Orquesta el alta: valida
// unicidad, persiste el registro en el schema compartido y aprovisiona su
// schema dedicado.
type Service struct {
	db         *database.DB
	migrations migrate.Set
	log        *slog.Logger
}

// NewService crea el servicio. migrations es el conjunto de migraciones de
// tenant que se aplica a cada schema nuevo.
func NewService(db *database.DB, migrations migrate.Set, log *slog.Logger) *Service {
	return &Service{db: db, migrations: migrations, log: log}
}

// CreateTenant crea un tenant y aprovisiona su schema en una sola
// transaccion: si cualquier paso falla no queda ni el registro ni el schema.
func (s *Service) CreateTenant(ctx context.Context, in TenantCreate) (Tenant, error) {
	slug, err := NormalizeSlug(in.Slug)
	if err != nil {
		return Tenant{}, err
	}

	tenant, err := database.InTxValue(ctx, s.db, func(q database.Querier) (Tenant, error) {
		repo := NewRepository(q)
		existing, err := repo.GetBySlug(ctx, slug)
		if err != nil {
			return Tenant{}, err
		}
		if existing != nil {
			return Tenant{}, ErrTenantSlugAlreadyExists
		}

		schemaName := BuildSchemaName(slug)
		tenant, err := repo.Add(ctx, Tenant{
			Name: in.Name, Slug: slug, SchemaName: schemaName, IsActive: true,
			Features: normalizedFeatures(in.Features),
		})
		if err != nil {
			return Tenant{}, err
		}
		// Aprovisionamiento del schema-per-tenant: schema + tablas de tenant.
		if err := repo.CreateSchema(ctx, schemaName); err != nil {
			return Tenant{}, err
		}
		if _, err := s.migrations.Up(ctx, q, schemaName); err != nil {
			return Tenant{}, err
		}
		// Los siete roles de sistema con los permisos de la matriz del PRD.
		if _, err := authz.SeedSystemRoles(ctx, q, tenant.ID); err != nil {
			return Tenant{}, err
		}
		return tenant, nil
	})
	if err != nil {
		return Tenant{}, err
	}
	s.log.InfoContext(ctx, "Tenant creado", "slug", tenant.Slug, "schema", tenant.SchemaName)
	return tenant, nil
}

// GetTenant devuelve un tenant por id.
func (s *Service) GetTenant(ctx context.Context, id uuid.UUID) (Tenant, error) {
	tenant, err := database.InTxValue(ctx, s.db, func(q database.Querier) (*Tenant, error) {
		return NewRepository(q).GetByID(ctx, id)
	})
	if err != nil {
		return Tenant{}, err
	}
	if tenant == nil {
		return Tenant{}, ErrTenantNotFound
	}
	return *tenant, nil
}

// GetTenantBySlug devuelve un tenant por slug.
func (s *Service) GetTenantBySlug(ctx context.Context, slug string) (Tenant, error) {
	tenant, err := database.InTxValue(ctx, s.db, func(q database.Querier) (*Tenant, error) {
		return NewRepository(q).GetBySlug(ctx, slug)
	})
	if err != nil {
		return Tenant{}, err
	}
	if tenant == nil {
		return Tenant{}, ErrTenantNotFound
	}
	return *tenant, nil
}

// SetFeatures reemplaza las banderas de funcionalidad del tenant.
func (s *Service) SetFeatures(ctx context.Context, id uuid.UUID, features map[string]bool) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		return NewRepository(q).SetFeatures(ctx, id, normalizedFeatures(features))
	})
}

// ListTenants devuelve una pagina de tenants.
func (s *Service) ListTenants(ctx context.Context, limit, offset int) ([]Tenant, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]Tenant, error) {
		return NewRepository(q).ListAll(ctx, limit, offset)
	})
}
