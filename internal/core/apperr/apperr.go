// Package apperr define los errores base del dominio ARGOS.
//
// Son agnosticos al transporte: la capa de dominio (services) los devuelve sin
// conocer HTTP y la capa API (internal/api/errors.go) los traduce a respuestas
// HTTP coherentes.
package apperr

import "time"

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
	// KindPreconditionRequired indica que falta un dato previo obligatorio
	// (p. ej. el codigo de verificacion en dos pasos).
	KindPreconditionRequired
	// KindTooManyRequests indica un bloqueo temporal por demasiados intentos.
	KindTooManyRequests
	// KindPaymentRequired indica una empresa en solo lectura por falta de pago
	// (402, fase06.md §3.2).
	KindPaymentRequired
)

var defaultMessages = map[Kind]string{
	KindInternal:             "Ha ocurrido un error en el sistema.",
	KindNotFound:             "Recurso no encontrado.",
	KindAlreadyExists:        "El recurso ya existe.",
	KindValidation:           "Datos invalidos.",
	KindAuthentication:       "No autenticado.",
	KindPermissionDenied:     "No tiene permisos para realizar esta accion.",
	KindPreconditionRequired: "Falta un dato obligatorio para continuar.",
	KindTooManyRequests:      "Demasiados intentos. Intente de nuevo mas tarde.",
	KindPaymentRequired:      "La empresa está en solo lectura por un pago pendiente.",
}

// Error es un error de dominio con un tipo y un mensaje apto para el cliente.
// RetryAfter, si es mayor que cero, indica cuando reintentar (bloqueos).
type Error struct {
	Kind       Kind
	Message    string
	RetryAfter time.Duration
	// Field es el campo de la peticion al que se refiere un error de
	// validacion (p. ej. "body.email"); vacio si es general.
	Field string
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

// Is hace que errors.Is reconozca las copias de un error (WithField,
// WithRetryAfter) como el error original: mismo tipo y mismo mensaje.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Kind == e.Kind && t.Message == e.Message
}

// WithRetryAfter devuelve una copia de e que indica cuando reintentar.
func (e *Error) WithRetryAfter(d time.Duration) *Error {
	c := *e
	c.RetryAfter = d
	return &c
}

// WithMessage devuelve una copia de e con un mensaje concreto del caso (p. ej.
// "Se cruza con ARG-7K3P9Q"). La copia ya no es errors.Is del original.
func (e *Error) WithMessage(message string) *Error {
	c := *e
	c.Message = message
	return &c
}

// WithField devuelve una copia de e asociada a un campo de la peticion (p. ej.
// "body.email"): en un 422 el cliente muestra el mensaje junto a ese campo.
func (e *Error) WithField(location string) *Error {
	c := *e
	c.Field = location
	return &c
}
