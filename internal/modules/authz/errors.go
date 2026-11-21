package authz

import "github.com/smorareqs/argos-service/internal/core/apperr"

// Errores del modulo de autorizacion.
var (
	ErrRoleNotFound = apperr.New(apperr.KindNotFound, "El rol indicado no existe en esta empresa.")
)
