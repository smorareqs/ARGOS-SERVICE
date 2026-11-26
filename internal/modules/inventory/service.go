// Package inventory gestiona el inventario de cada empresa (fase02.md §2):
// articulos, bodegas (central y una por propiedad), existencias, movimientos
// inmutables, custodia por colaborador, diferencias al cerrar tareas y
// solicitudes de reposicion. Es parte de Persona B.
//
// Las existencias y la custodia nunca quedan negativas (CHECK en la base): un
// movimiento que lo haria falla con ErrNotEnoughStock y no cambia nada. Cada
// movimiento y su efecto en existencias o custodia van en la misma
// transaccion.
//
// El modulo de tareas usa CustodyOf, ReturnFromCustody, ConsumeFromCustody y
// OpenDiscrepancy dentro de la transaccion del cierre de la tarea.
package inventory

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
)

// Eventos que publica el modulo (notificaciones se suscribe).
const (
	// EventDiscrepancyOpened: al cerrar una tarea no cuadro la custodia.
	// Data: DiscrepancyEvent. Destinatarios: quienes concilian.
	EventDiscrepancyOpened = "inventory.discrepancy_opened"
	// EventRestockRequested: un colaborador pidio reposicion. Data:
	// RestockEvent. Destinatarios: quienes gestionan inventario.
	EventRestockRequested = "inventory.restock_requested"
)

// DiscrepancyEvent es el Data de EventDiscrepancyOpened.
type DiscrepancyEvent struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	ItemName   string
	Expected   float64
	Returned   float64
	TaskID     *uuid.UUID
	TaskNumber *int
}

// RestockEvent es el Data de EventRestockRequested.
type RestockEvent struct {
	Request RestockRequest
}

// Errores del modulo.
var (
	ErrItemNotFound        = apperr.New(apperr.KindNotFound, "El artículo no existe.")
	ErrWarehouseNotFound   = apperr.New(apperr.KindNotFound, "La bodega no existe.")
	ErrDiscrepancyNotFound = apperr.New(apperr.KindNotFound, "La diferencia no existe.")
	ErrRequestNotFound     = apperr.New(apperr.KindNotFound, "La solicitud no existe.")
	ErrCustodyNotFound     = apperr.New(apperr.KindNotFound, "Esa persona no tiene ese artículo en custodia.")

	ErrSKUTaken          = apperr.New(apperr.KindValidation, "Ya hay un artículo con ese código.")
	ErrWarehouseTaken    = apperr.New(apperr.KindValidation, "Ya hay una bodega con ese nombre o para esa propiedad.")
	ErrUnknownItem       = apperr.New(apperr.KindValidation, "Ese artículo no existe o está inactivo.")
	ErrUnknownWarehouse  = apperr.New(apperr.KindValidation, "Esa bodega no existe o está inactiva.")
	ErrUnknownProperty   = apperr.New(apperr.KindValidation, "Esa propiedad no existe.")
	ErrSameWarehouse     = apperr.New(apperr.KindValidation, "Elige una bodega de destino distinta.")
	ErrNotEnoughStock    = apperr.New(apperr.KindValidation, "No hay suficiente existencia para ese movimiento.")
	ErrNotEnoughCustody  = apperr.New(apperr.KindValidation, "No tienes esa cantidad en custodia.")
	ErrNoteRequired      = apperr.New(apperr.KindValidation, "Escribe el motivo del ajuste.")
	ErrInvalidQuantity   = apperr.New(apperr.KindValidation, "La cantidad no es válida.")
	ErrNoWarehouse       = apperr.New(apperr.KindValidation, "Primero crea una bodega central.")
	ErrDecisionNotOpen   = apperr.New(apperr.KindValidation, "Esa solicitud ya se resolvió.")
	ErrInvalidTransition = apperr.New(apperr.KindValidation, "Ese cambio de estado no está permitido.")
	ErrForbiddenMovement = apperr.New(apperr.KindPermissionDenied, "No puedes registrar ese movimiento.")
)

// Service contiene los casos de uso de inventario.
type Service struct {
	db  *database.DB
	bus *events.Bus
	log *slog.Logger
	now func() time.Time
}

// NewService crea el servicio.
func NewService(db *database.DB, bus *events.Bus, log *slog.Logger) *Service {
	return &Service{db: db, bus: bus, log: log, now: time.Now}
}

func trim(s string) string { return strings.TrimSpace(s) }

// trimmed devuelve el texto sin espacios o nil si queda vacio.
func trimmed(s *string) *string {
	if s == nil {
		return nil
	}
	v := trim(*s)
	if v == "" {
		return nil
	}
	return &v
}

// returnableCategory: blancos y llaves se devuelven completos al cerrar.
func returnableCategory(c string) bool {
	return c == "linens" || c == "keys"
}

// round3 redondea a la precision de la base (numeric(12,3)).
func round3(v float64) float64 {
	if v < 0 {
		return -round3(-v)
	}
	return float64(int64(v*1000+0.5)) / 1000
}

// DefaultWarehouse es la bodega donde vuelve lo de una propiedad: la de la
// propiedad si existe y esta activa, si no la central mas antigua.
func DefaultWarehouse(ctx context.Context, q database.Querier, propertyID *uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		SELECT id FROM warehouses
		WHERE is_active AND (property_id = $1 OR property_id IS NULL)
		ORDER BY (property_id IS NULL), created_at LIMIT 1`, propertyID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNoWarehouse
	}
	return id, err
}

// isCheckViolation indica que la base rechazo el cambio por un CHECK
// (existencia o custodia negativa).
func isCheckViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23514"
}
