// Package database es la fabrica de conexiones a PostgreSQL (pgx).
//
// Estrategia multi-tenant: schema-per-tenant.
//
//   - Un unico pool de conexiones.
//   - Las tablas globales (tenants, usuarios) viven en el schema compartido
//     (SharedSchema) y se referencian siempre calificadas: shared.tenants.
//   - Cada tenant tiene un schema dedicado (tenant_<slug>). Toda operacion de
//     datos corre dentro de InTx, que fija `search_path` con SET LOCAL al
//     schema del tenant presente en el contexto (WithTenantSchema) seguido de
//     shared. Al ser LOCAL, el valor muere con la transaccion y nunca se filtra
//     a otra peticion que reutilice la conexion del pool.
package database

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/tracelog"
)

// SharedSchema es el schema de las tablas globales.
const SharedSchema = "shared"

// Querier es lo que los repositorios necesitan para consultar. Lo cumplen
// pgx.Tx y *pgxpool.Pool.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Options ajusta el pool de conexiones.
type Options struct {
	MaxConns int32
	// EchoLogger, si no es nil, registra cada sentencia SQL en nivel DEBUG.
	EchoLogger *slog.Logger
}

// DB envuelve el pool de conexiones.
type DB struct {
	pool *pgxpool.Pool
}

// Connect crea el pool. No abre conexiones hasta la primera consulta.
func Connect(ctx context.Context, dsn string, opts Options) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("configuracion de base de datos invalida: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.EchoLogger != nil {
		cfg.ConnConfig.Tracer = &tracelog.TraceLog{
			Logger:   echoLogger(opts.EchoLogger),
			LogLevel: tracelog.LogLevelDebug,
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("creando el pool de conexiones: %w", err)
	}
	return &DB{pool: pool}, nil
}

func echoLogger(log *slog.Logger) tracelog.Logger {
	return tracelog.LoggerFunc(func(ctx context.Context, _ tracelog.LogLevel, msg string, data map[string]any) {
		log.DebugContext(ctx, msg, "sql", data["sql"], "args", data["args"])
	})
}

// Close cierra el pool.
func (db *DB) Close() {
	db.pool.Close()
}

// Ping verifica la conectividad con la base de datos (usado por el health check).
func (db *DB) Ping(ctx context.Context) error {
	return db.pool.Ping(ctx)
}

// Pool expone el pool para herramientas (migraciones, pruebas).
func (db *DB) Pool() *pgxpool.Pool {
	return db.pool
}

type tenantSchemaKey struct{}

// WithTenantSchema marca el contexto para que las transacciones posteriores
// operen sobre el schema del tenant. Lo fija la capa API tras autenticar.
func WithTenantSchema(ctx context.Context, schema string) context.Context {
	return context.WithValue(ctx, tenantSchemaKey{}, schema)
}

// TenantSchema devuelve el schema del tenant activo en el contexto, si hay.
func TenantSchema(ctx context.Context) (string, bool) {
	schema, ok := ctx.Value(tenantSchemaKey{}).(string)
	return schema, ok && schema != ""
}

// InTx ejecuta fn en una transaccion con el search_path del tenant del
// contexto (o solo shared si no hay tenant). Hace commit si fn no devuelve
// error y rollback en caso contrario.
func (db *DB) InTx(ctx context.Context, fn func(q Querier) error) error {
	return pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error {
		schemas := []string{SharedSchema}
		if tenant, ok := TenantSchema(ctx); ok {
			schemas = []string{tenant, SharedSchema}
		}
		if err := SetSearchPath(ctx, tx, schemas...); err != nil {
			return err
		}
		return fn(tx)
	})
}

// InTxValue es InTx para funciones que devuelven un valor.
func InTxValue[T any](ctx context.Context, db *DB, fn func(q Querier) (T, error)) (T, error) {
	var result T
	err := db.InTx(ctx, func(q Querier) error {
		var err error
		result, err = fn(q)
		return err
	})
	return result, err
}

// SetSearchPath fija el search_path de la transaccion en curso (equivale a
// SET LOCAL). Los nombres se citan como identificadores, nunca se interpolan
// crudos.
func SetSearchPath(ctx context.Context, q Querier, schemas ...string) error {
	quoted := make([]string, len(schemas))
	for i, s := range schemas {
		quoted[i] = pgx.Identifier{s}.Sanitize()
	}
	if _, err := q.Exec(ctx, "SELECT set_config('search_path', $1, true)", strings.Join(quoted, ", ")); err != nil {
		return fmt.Errorf("fijando search_path: %w", err)
	}
	return nil
}
