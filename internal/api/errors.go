// Package api es la capa de interfaz HTTP compartida por todas las versiones:
// traduccion de errores de dominio a HTTP y dependencias de autenticacion.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/smorareqs/argos-service/internal/core/apperr"
)

const internalErrorMessage = "Error interno del servidor."

// Codigo HTTP de cada tipo de error de dominio.
var kindStatus = map[apperr.Kind]int{
	apperr.KindNotFound:         http.StatusNotFound,
	apperr.KindAlreadyExists:    http.StatusConflict,
	apperr.KindValidation:       http.StatusUnprocessableEntity,
	apperr.KindAuthentication:   http.StatusUnauthorized,
	apperr.KindPermissionDenied: http.StatusForbidden,
}

// HTTPError traduce un error devuelto por un servicio a un error HTTP.
//
// Todo handler debe devolver sus errores a traves de esta funcion: huma
// incluye el texto de cualquier otro error en la respuesta, lo que filtraria
// detalles internos (SQL, rutas, etc.).
func HTTPError(ctx context.Context, err error) huma.StatusError {
	status, message := classify(ctx, err)
	return huma.NewError(status, message)
}

func classify(ctx context.Context, err error) (int, string) {
	var domainErr *apperr.Error
	if !errors.As(err, &domainErr) {
		slog.ErrorContext(ctx, "Error no controlado", "error", err)
		return http.StatusInternalServerError, internalErrorMessage
	}
	if status, ok := kindStatus[domainErr.Kind]; ok {
		return status, domainErr.Message
	}
	// Error de dominio sin codigo asignado: error interno controlado.
	slog.ErrorContext(ctx, "Error de dominio no mapeado", "error", err)
	return http.StatusInternalServerError, domainErr.Message
}
