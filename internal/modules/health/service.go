// Package health verifica el estado de la aplicacion y de la base de datos.
package health

import (
	"context"
	"time"
)

const pingTimeout = 3 * time.Second

// Pinger verifica la conectividad con la base de datos.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Service calcula el estado del servicio.
type Service struct {
	db          Pinger
	environment string
}

// NewService crea el servicio.
func NewService(db Pinger, environment string) *Service {
	return &Service{db: db, environment: environment}
}

// Check devuelve el estado del servicio y la conectividad con PostgreSQL.
func (s *Service) Check(ctx context.Context) Status {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	dbStatus := "up"
	if err := s.db.Ping(ctx); err != nil {
		dbStatus = "down"
	}
	status := "ok"
	if dbStatus != "up" {
		status = "degraded"
	}
	return Status{Status: status, Environment: s.environment, Database: dbStatus}
}
