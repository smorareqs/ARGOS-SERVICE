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

// Errores de configuracion de la empresa.
var (
	ErrInvalidName      = apperr.New(apperr.KindValidation, "El nombre debe tener entre 1 y 255 caracteres.")
	ErrInvalidTimezone  = apperr.New(apperr.KindValidation, "La zona horaria no es válida.")
	ErrInvalidCurrency  = apperr.New(apperr.KindValidation, "La moneda debe ser GTQ o USD.")
	ErrInvalidTime      = apperr.New(apperr.KindValidation, "La hora debe tener el formato HH:MM.")
	ErrInvalidAlarm     = apperr.New(apperr.KindValidation, "La alarma debe estar entre 0 y 240 minutos antes de la salida.")
	ErrInvalidLogoFile  = apperr.New(apperr.KindValidation, "Sube el logo de nuevo: el archivo no está listo o no es un logo.")
	ErrInvalidText      = apperr.New(apperr.KindValidation, "El texto es demasiado largo.")
	ErrInvalidAdminMail = apperr.New(apperr.KindValidation, "El correo del administrador no es válido.")
)
