package auth

import "github.com/smorareqs/argos-service/internal/core/apperr"

// Errores del modulo de autenticacion.
var (
	ErrUserNotFound       = apperr.New(apperr.KindNotFound, "El usuario no existe.")
	ErrEmailAlreadyExists = apperr.New(apperr.KindAlreadyExists, "Ya existe un usuario con ese correo.")
	ErrInvalidCredentials = apperr.New(apperr.KindAuthentication, "Correo o contrasena incorrectos.")
	ErrInactiveUser       = apperr.New(apperr.KindAuthentication, "La cuenta de usuario esta inactiva.")
	ErrInvalidToken       = apperr.New(apperr.KindAuthentication, "Token invalido o expirado.")
	ErrUnknownTenant      = apperr.New(apperr.KindValidation, "El tenant indicado no existe.")
	ErrInvalidEmail       = apperr.New(apperr.KindValidation, "El correo electronico no es valido.")
	ErrInvalidFullName    = apperr.New(apperr.KindValidation, "El nombre debe tener entre 1 y 255 caracteres.")
	ErrInvalidPassword    = apperr.New(apperr.KindValidation, "La contrasena debe tener entre 8 y 128 caracteres.")
	ErrInvalidRole        = apperr.New(apperr.KindValidation, "El rol no es valido.")
)
