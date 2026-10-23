// Package apperr define los errores base del dominio ARGOS.
//
// Son agnosticos al transporte: la capa de dominio (services) los devuelve sin
// conocer HTTP y la capa API (internal/api/errors.go) los traduce a respuestas
// HTTP coherentes.
package apperr

// Kind clasifica un error de dominio. La capa API decide el codigo HTTP.
type Kind uint8

const (
	// KindInternal es un error de dominio sin clasificar.
	KindInternal Kind = iota
	// KindNotFound indica que la entidad solicitada no existe.
	KindNotFound
	// KindAlreadyExists indica un conflicto por unicidad de negocio.
	KindAlreadyExists
	// KindValidation indica la violacion de una regla de validacion de negocio.
	KindValidation
	// KindAuthentication indica credenciales invalidas o token no valido.
	KindAuthentication
	// KindPermissionDenied indica que el usuario autenticado no tiene permisos.
	KindPermissionDenied
)

var defaultMessages = map[Kind]string{
	KindInternal:         "Ha ocurrido un error en el sistema.",
	KindNotFound:         "Recurso no encontrado.",
	KindAlreadyExists:    "El recurso ya existe.",
	KindValidation:       "Datos invalidos.",
	KindAuthentication:   "No autenticado.",
	KindPermissionDenied: "No tiene permisos para realizar esta accion.",
}

// Error es un error de dominio con un tipo y un mensaje apto para el cliente.
type Error struct {
	Kind    Kind
	Message string
}

// New crea un error de dominio. Un mensaje vacio usa el mensaje por defecto
// del tipo.
func New(kind Kind, message string) *Error {
	if message == "" {
		message = defaultMessages[kind]
	}
	return &Error{Kind: kind, Message: message}
}

func (e *Error) Error() string {
	return e.Message
}
