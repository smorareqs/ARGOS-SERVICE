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

// Errores de cuenta, sesiones, contrasenas, 2FA e invitaciones. En endpoints
// autenticados nunca se usa KindAuthentication para un dato incorrecto: 401
// significa "token vencido" y el cliente cerraria la sesion.
var (
	ErrSessionNotFound    = apperr.New(apperr.KindNotFound, "Esa sesión ya no existe.")
	ErrWrongPassword      = apperr.New(apperr.KindValidation, "La contraseña actual no es correcta.")
	ErrResetLinkInvalid   = apperr.New(apperr.KindNotFound, "El enlace para restablecer la contraseña venció o ya se usó. Pide uno nuevo.")
	ErrTOTPAlreadyEnabled = apperr.New(apperr.KindValidation, "La verificación en dos pasos ya está activa.")
	ErrTOTPNotSetup       = apperr.New(apperr.KindValidation, "Primero genera el código QR para activar la verificación en dos pasos.")
	ErrTOTPNotEnabled     = apperr.New(apperr.KindValidation, "La verificación en dos pasos no está activa.")
	ErrTOTPCodeIncorrect  = apperr.New(apperr.KindValidation, "El código no es correcto. Revisa que la hora de tu teléfono esté bien y escribe el código vigente.")
	ErrInvitationInvalid  = apperr.New(apperr.KindNotFound, "La invitación venció, ya se usó o fue cancelada. Pide una nueva a quien te invitó.")
	ErrInvitationNotFound = apperr.New(apperr.KindNotFound, "Esa invitación ya no existe.")
	ErrAlreadyMember      = apperr.New(apperr.KindValidation, "Esa persona ya es parte de la empresa.")
	ErrAlreadyInvited     = apperr.New(apperr.KindValidation, "Ya hay una invitación pendiente para ese correo. Puedes reenviarla.")
	ErrInvitationRole     = apperr.New(apperr.KindValidation, "Elige un rol de la empresa.")
	ErrInvitationMessage  = apperr.New(apperr.KindValidation, "El mensaje no puede pasar de 500 caracteres.")
	ErrInvitationAccepted = apperr.New(apperr.KindValidation, "Esa invitación ya fue aceptada.")
	ErrPlatformNotAllowed = apperr.New(apperr.KindNotFound, "No tienes acceso a la administración de ARGOS.")
)
