package authz

import "github.com/smorareqs/argos-service/internal/core/apperr"

// Errores del modulo de autorizacion.
var (
	ErrRoleNotFound = apperr.New(apperr.KindNotFound, "El rol indicado no existe en esta empresa.")
)

// Errores de la administracion de roles y miembros.
var (
	ErrMemberNotFound      = apperr.New(apperr.KindNotFound, "Ese colaborador ya no está en la empresa.")
	ErrLastAdmin           = apperr.New(apperr.KindValidation, "Debe quedar al menos un administrador activo.")
	ErrSelfSuspend         = apperr.New(apperr.KindValidation, "No puedes suspender tu propia cuenta.")
	ErrSystemRoleLocked    = apperr.New(apperr.KindValidation, "Los roles del sistema no se editan. Duplícalo para crear uno personalizado.")
	ErrRoleInUse           = apperr.New(apperr.KindValidation, "El rol tiene colaboradores o invitaciones pendientes. Cámbialos de rol antes de borrarlo.")
	ErrInvalidRoleName     = apperr.New(apperr.KindValidation, "El nombre del rol debe tener entre 2 y 128 caracteres.")
	ErrRoleNameTaken       = apperr.New(apperr.KindValidation, "Ya hay un rol con ese nombre.")
	ErrInvalidPermission   = apperr.New(apperr.KindValidation, "Hay un permiso que no existe en el catálogo.")
	ErrCannotGrant         = apperr.New(apperr.KindValidation, "No puedes dar permisos que tú no tienes.")
	ErrRoleWithoutPerms    = apperr.New(apperr.KindValidation, "Elige al menos un permiso para el rol.")
	ErrGuestRoleNotAllowed = apperr.New(apperr.KindValidation, "El rol de huésped no se asigna a colaboradores.")
)
