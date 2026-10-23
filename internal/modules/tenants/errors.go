package tenants

import (
	"fmt"

	"github.com/smorareqs/argos-service/internal/core/apperr"
)

// Errores del modulo de tenants.
var (
	ErrTenantNotFound          = apperr.New(apperr.KindNotFound, "El tenant solicitado no existe.")
	ErrTenantSlugAlreadyExists = apperr.New(apperr.KindAlreadyExists, "Ya existe un tenant con ese slug.")
	ErrInvalidSlug             = apperr.New(apperr.KindValidation, fmt.Sprintf(
		"El slug debe contener solo minusculas, numeros y guiones bajos, "+
			"empezar y terminar en letra o numero y tener entre %d y %d caracteres.",
		slugMinLength, slugMaxLength))
)
