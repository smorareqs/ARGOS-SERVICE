// Package app construye los servicios de dominio y sus dependencias
// (inyeccion de dependencias). Lo usan el servidor HTTP y los comandos CLI.
//
// Archivo compartido: cada modulo nuevo agrega su campo y su linea de
// construccion; nunca se reordena.
package app

import (
	"log/slog"
	"time"

	"github.com/smorareqs/argos-service/internal/core/config"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/migrate"
	"github.com/smorareqs/argos-service/internal/core/security"
	"github.com/smorareqs/argos-service/internal/modules/auth"
	"github.com/smorareqs/argos-service/internal/modules/health"
	"github.com/smorareqs/argos-service/internal/modules/tenants"
)

// Services agrupa los servicios de dominio.
type Services struct {
	Health  *health.Service
	Auth    *auth.Service
	Tenants *tenants.Service
}

// NewServices construye todos los servicios.
func NewServices(cfg config.Config, db *database.DB, log *slog.Logger) (Services, error) {
	tokens, err := security.NewTokenManager(
		cfg.SecretKey,
		cfg.JWTAlgorithm,
		time.Duration(cfg.AccessTokenExpireMinutes)*time.Minute,
		time.Duration(cfg.RefreshTokenExpireDays)*24*time.Hour,
	)
	if err != nil {
		return Services{}, err
	}
	tenantMigrations, err := migrate.Tenant()
	if err != nil {
		return Services{}, err
	}

	return Services{
		Health:  health.NewService(db, string(cfg.Environment)),
		Auth:    auth.NewService(db, tokens, log.With("module", "auth")),
		Tenants: tenants.NewService(db, tenantMigrations, log.With("module", "tenants")),
	}, nil
}
