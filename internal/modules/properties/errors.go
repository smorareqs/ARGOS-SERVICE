package properties

import "github.com/smorareqs/argos-service/internal/core/apperr"

// Errores del modulo de propiedades y edificios.
var (
	ErrPropertyNotFound = apperr.New(apperr.KindNotFound, "La propiedad no existe.")
	ErrBuildingNotFound = apperr.New(apperr.KindNotFound, "El edificio no existe.")
	ErrNoAccessInfo     = apperr.New(apperr.KindNotFound, "Esta propiedad aún no tiene información de acceso.")

	ErrAccessToday = apperr.New(apperr.KindPermissionDenied,
		"Solo ves el acceso de las propiedades donde tienes una tarea hoy.")

	ErrInvalidCode       = apperr.New(apperr.KindValidation, "El código usa de 2 a 32 letras, números o guiones.")
	ErrCodeTaken         = apperr.New(apperr.KindValidation, "Ya existe una propiedad con ese código. Usa otro.")
	ErrInvalidName       = apperr.New(apperr.KindValidation, "El nombre debe tener entre 2 y 128 caracteres.")
	ErrBuildingNameTaken = apperr.New(apperr.KindValidation, "Ya hay un edificio con ese nombre.")
	ErrUnknownBuilding   = apperr.New(apperr.KindValidation, "Ese edificio no existe.")
	ErrUnknownTemplate   = apperr.New(apperr.KindValidation, "Esa plantilla de checklist no existe.")
	ErrInvalidCover      = apperr.New(apperr.KindValidation, "La foto no es una portada subida a esta empresa.")
	ErrCoordinates       = apperr.New(apperr.KindValidation, "Indica latitud y longitud juntas.")
	ErrTextTooLong       = apperr.New(apperr.KindValidation, "El texto es demasiado largo.")
	ErrNotMembers        = apperr.New(apperr.KindValidation, "Alguna persona no pertenece a la empresa.")
	ErrUnknownProperties = apperr.New(apperr.KindValidation, "Alguna propiedad no existe.")
	ErrBuildingInUse     = apperr.New(apperr.KindValidation,
		"El edificio tiene propiedades. Muévelas a otro edificio o desactívalo.")
)
