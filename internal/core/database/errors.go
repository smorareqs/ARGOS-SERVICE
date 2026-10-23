package database

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// Codigos SQLSTATE que los repositorios traducen a errores de dominio.
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeDuplicateSchema     = "42P06"
)

// IsUniqueViolation indica si err es una violacion de unicidad.
func IsUniqueViolation(err error) bool {
	return hasCode(err, codeUniqueViolation)
}

// IsForeignKeyViolation indica si err es una violacion de clave foranea.
func IsForeignKeyViolation(err error) bool {
	return hasCode(err, codeForeignKeyViolation)
}

// IsDuplicateSchema indica si err se debe a que el schema ya existe.
func IsDuplicateSchema(err error) bool {
	return hasCode(err, codeDuplicateSchema)
}

func hasCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
