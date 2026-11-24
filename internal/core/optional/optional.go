// Package optional tiene el tipo de los campos de un PATCH que distinguen "no
// vino" de "vino null": `{"building_id": null}` quita el edificio y un cuerpo
// sin `building_id` lo deja como estaba.
//
// Uso en un esquema de huma (las etiquetas de validacion aplican al valor):
//
//	BuildingID optional.Value[uuid.UUID] `json:"building_id,omitzero" format:"uuid"`
package optional

import (
	"bytes"
	"encoding/json"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
)

// Value es un campo opcional y anulable.
type Value[T any] struct {
	Sent  bool // vino en el cuerpo, con valor o null
	Null  bool // vino null
	Value T
}

// Of crea un valor enviado (util en pruebas y en la semilla).
func Of[T any](v T) Value[T] {
	return Value[T]{Sent: true, Value: v}
}

// UnmarshalJSON marca el campo como enviado.
func (o *Value[T]) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	o.Sent = true
	if bytes.Equal(b, []byte("null")) {
		o.Null = true
		return nil
	}
	return json.Unmarshal(b, &o.Value)
}

// Schema es el del tipo T, aceptando null.
func (o Value[T]) Schema(r huma.Registry) *huma.Schema {
	s := r.Schema(reflect.TypeOf(o.Value), true, "")
	s.Nullable = true
	return s
}

// Apply devuelve el valor nuevo de un campo anulable: el actual si no vino,
// nil si vino null o el valor enviado.
func Apply[T any](current *T, o Value[T]) *T {
	switch {
	case !o.Sent:
		return current
	case o.Null:
		return nil
	default:
		v := o.Value
		return &v
	}
}
