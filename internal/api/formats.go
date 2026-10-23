package api

import (
	"encoding/json"
	"net/url"

	"github.com/danielgtaylor/huma/v2"
)

// FormURLEncoded permite que huma lea cuerpos application/x-www-form-urlencoded
// (lo exige el flujo OAuth2 password del login) y los valide contra el mismo
// schema que un cuerpo JSON. Cada campo toma el primer valor recibido.
//
// Es solo de entrada: si un cliente pidiera ese formato en Accept, la
// respuesta se codifica como JSON.
var FormURLEncoded = huma.Format{
	Marshal: huma.DefaultJSONFormat.Marshal,
	Unmarshal: func(data []byte, v any) error {
		values, err := url.ParseQuery(string(data))
		if err != nil {
			return err
		}
		fields := make(map[string]any, len(values))
		for key := range values {
			fields[key] = values.Get(key)
		}
		raw, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, v)
	},
}
