// Package paging implementa la paginacion por cursor de las listas de la API
// (fase02.md §4: `?cursor=&limit=`, respuesta `{items, next_cursor}`).
//
// El cursor es opaco para el cliente: codifica en base64url los valores de la
// clave de orden de la ultima fila devuelta. El repositorio pide Limit+1 filas;
// si llega la fila extra hay pagina siguiente.
package paging

import (
	"encoding/base64"
	"encoding/json"

	"github.com/smorareqs/argos-service/internal/core/apperr"
)

// Limites de pagina.
const (
	DefaultLimit = 50
	MaxLimit     = 500
)

// ErrInvalidCursor es un cursor que no salio de esta API.
var ErrInvalidCursor = apperr.New(apperr.KindValidation, "El cursor de la página no es válido.").WithField("query.cursor")

// Limit normaliza el tamano de pagina pedido.
func Limit(n int) int {
	switch {
	case n <= 0:
		return DefaultLimit
	case n > MaxLimit:
		return MaxLimit
	default:
		return n
	}
}

// Encode crea un cursor con los valores de la clave de orden.
func Encode(key ...string) string {
	b, _ := json.Marshal(key)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Decode lee un cursor de n valores. Un cursor vacio devuelve nil (primera
// pagina).
func Decode(cursor string, n int) ([]string, error) {
	if cursor == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	var key []string
	if err := json.Unmarshal(b, &key); err != nil || len(key) != n {
		return nil, ErrInvalidCursor
	}
	return key, nil
}

// Page recorta la fila extra y calcula el cursor siguiente con key.
func Page[T any](items []T, limit int, key func(T) []string) ([]T, *string) {
	if items == nil {
		items = []T{}
	}
	if len(items) <= limit {
		return items, nil
	}
	items = items[:limit]
	next := Encode(key(items[len(items)-1])...)
	return items, &next
}
