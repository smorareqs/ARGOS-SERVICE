package auth

import "github.com/smorareqs/argos-service/internal/core/apperr"

// Errores del modulo de autenticacion. Los mensajes llegan al usuario final.
var (
	ErrUserNotFound        = apperr.New(apperr.KindNotFound, "El usuario no existe.")
	ErrEmailAlreadyExists  = apperr.New(apperr.KindAlreadyExists, "Ya existe un usuario con ese correo.")
	ErrInvalidCredentials  = apperr.New(apperr.KindAuthentication, "Correo o contraseña incorrectos.")
	ErrInactiveUser        = apperr.New(apperr.KindAuthentication, "Tu cuenta está desactivada. Habla con el administrador de tu empresa.")
	ErrInvalidToken        = apperr.New(apperr.KindAuthentication, "Tu sesión venció. Inicia sesión de nuevo.")
	ErrTOTPRequired        = apperr.New(apperr.KindPreconditionRequired, "Escribe el código de verificación de tu aplicación.")
	ErrInvalidTOTP         = apperr.New(apperr.KindAuthentication, "El código de verificación no es correcto.")
	ErrNoActiveMemberships = apperr.New(apperr.KindPermissionDenied, "Tu cuenta no tiene acceso activo a ninguna empresa.")
	ErrNoMembership        = apperr.New(apperr.KindNotFound, "No tienes acceso a esa empresa.")
	ErrTooManyAttempts     = apperr.New(apperr.KindTooManyRequests, "Demasiados intentos fallidos. Espera unos minutos e inténtalo de nuevo.")
	ErrInvalidEmail        = apperr.New(apperr.KindValidation, "El correo electrónico no es válido.")
	ErrInvalidFullName     = apperr.New(apperr.KindValidation, "El nombre debe tener entre 1 y 255 caracteres.")
	ErrInvalidPassword     = apperr.New(apperr.KindValidation, "La contraseña debe tener entre 8 y 128 caracteres.")
	ErrInvalidPhone        = apperr.New(apperr.KindValidation, "El teléfono no puede pasar de 32 caracteres.")
	ErrInvalidDevice       = apperr.New(apperr.KindValidation, "Los datos del dispositivo no son válidos.")
	ErrInvalidSelection    = apperr.New(apperr.KindValidation, "Envía selection_token y device, o refresh_token.")
)
