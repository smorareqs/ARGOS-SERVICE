package authz

import (
	"context"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
)

// PropertyScope es el ScopeResolver de la fase 2 (fase02.md §3, PRD §2.2 capa
// 3): el conjunto de propiedades que una persona puede ver en su empresa.
// Todo repositorio de propiedades, tareas, inventario por propiedad,
// mantenimiento y reservas filtra por el. Un recurso fuera de alcance responde
// 404, nunca 403.
type PropertyScope struct {
	// All: ve todas las propiedades de la empresa.
	All bool
	// IDs: las que ve cuando All es false (asignadas o propias).
	IDs []uuid.UUID
}

// Allows indica si la propiedad esta dentro del alcance.
func (s PropertyScope) Allows(id uuid.UUID) bool {
	return s.All || slices.Contains(s.IDs, id)
}

// SQLArgs devuelve los dos parametros del filtro estandar:
//
//	WHERE ($1::bool OR p.id = ANY($2::uuid[]))
func (s PropertyScope) SQLArgs() (bool, []uuid.UUID) {
	ids := s.IDs
	if ids == nil {
		ids = []uuid.UUID{}
	}
	return s.All, ids
}

// ResolvePropertyScope calcula el alcance del actor. q debe operar en el
// schema de su empresa (la transaccion de la peticion).
//
// Ve todas si su rol no es de alcance restringido (roles.scoped = false) o si
// tiene properties.manage o tasks.manage. Si no, ve las de
// property_assignments (limpieza, mantenimiento) y property_owners
// (propietarios).
func ResolvePropertyScope(ctx context.Context, q database.Querier, actor Actor) (PropertyScope, error) {
	if !actor.Scoped || actor.Permissions[PropertiesManage] || actor.Permissions[TasksManage] {
		return PropertyScope{All: true}, nil
	}
	rows, err := q.Query(ctx, `
		SELECT property_id FROM property_assignments WHERE user_id = $1
		UNION
		SELECT property_id FROM property_owners WHERE user_id = $1`, actor.UserID)
	if err != nil {
		return PropertyScope{}, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return PropertyScope{}, err
	}
	return PropertyScope{IDs: ids}, nil
}
