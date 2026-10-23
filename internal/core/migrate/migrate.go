// Package migrate aplica las migraciones SQL de ARGOS.
//
// Hay dos conjuntos (ver migrations/README.md):
//
//   - shared: tablas globales del schema compartido.
//   - tenant: se aplica a cada schema tenant_<slug>.
//
// Cada schema guarda sus versiones aplicadas en su propia tabla
// "<schema>".schema_migrations. Las operaciones corren dentro de la
// transaccion que recibe el llamador; asi el alta de un tenant (insertar el
// registro, crear el schema y migrarlo) es una sola transaccion atomica.
package migrate

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/migrations"
)

const (
	markerUp   = "-- migrate:up"
	markerDown = "-- migrate:down"
)

var fileNameRe = regexp.MustCompile(`^(\d+)_([a-z0-9_]+)\.sql$`)

// Migration es un archivo NNNN_nombre.sql con sus secciones up y down.
type Migration struct {
	Version int64
	Name    string
	Up      string
	Down    string
}

// ID devuelve el identificador legible, p. ej. "0001_initial".
func (m Migration) ID() string {
	return fmt.Sprintf("%04d_%s", m.Version, m.Name)
}

// State es una migracion y, si ya se aplico, cuando.
type State struct {
	Migration
	AppliedAt *time.Time
}

// Set es un conjunto ordenado de migraciones (shared o tenant).
type Set struct {
	Name       string
	Migrations []Migration
}

// Shared carga las migraciones del schema compartido embebidas en el binario.
func Shared() (Set, error) {
	return Load(migrations.FS, "shared")
}

// Tenant carga las migraciones de tenant embebidas en el binario.
func Tenant() (Set, error) {
	return Load(migrations.FS, "tenant")
}

// Load lee los archivos *.sql de dir. Ignora cualquier otro archivo.
func Load(fsys fs.FS, dir string) (Set, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return Set{}, fmt.Errorf("leyendo migraciones %s: %w", dir, err)
	}
	set := Set{Name: dir}
	seen := map[int64]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		m, err := parseFile(fsys, dir, entry.Name())
		if err != nil {
			return Set{}, err
		}
		if other, dup := seen[m.Version]; dup {
			return Set{}, fmt.Errorf("migraciones %s: version %d duplicada (%s y %s)", dir, m.Version, other, entry.Name())
		}
		seen[m.Version] = entry.Name()
		set.Migrations = append(set.Migrations, m)
	}
	slices.SortFunc(set.Migrations, func(a, b Migration) int { return cmp.Compare(a.Version, b.Version) })
	return set, nil
}

func parseFile(fsys fs.FS, dir, name string) (Migration, error) {
	match := fileNameRe.FindStringSubmatch(name)
	if match == nil {
		return Migration{}, fmt.Errorf("migracion %s/%s: el nombre debe ser NNNN_nombre.sql (minusculas, numeros y _)", dir, name)
	}
	version, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil || version <= 0 {
		return Migration{}, fmt.Errorf("migracion %s/%s: version invalida", dir, name)
	}
	content, err := fs.ReadFile(fsys, path.Join(dir, name))
	if err != nil {
		return Migration{}, fmt.Errorf("leyendo %s/%s: %w", dir, name, err)
	}
	up, down, err := splitSections(string(content))
	if err != nil {
		return Migration{}, fmt.Errorf("migracion %s/%s: %w", dir, name, err)
	}
	return Migration{Version: version, Name: match[2], Up: up, Down: down}, nil
}

// splitSections separa el contenido en las secciones up y down. Lo que haya
// antes del marcador up (comentarios de cabecera) se descarta.
func splitSections(content string) (up, down string, err error) {
	var upLines, downLines []string
	var current *[]string
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		switch strings.TrimSpace(line) {
		case markerUp:
			if current != nil {
				return "", "", errors.New(markerUp + " debe ser la primera seccion y aparecer una sola vez")
			}
			current = &upLines
		case markerDown:
			if current != &upLines {
				return "", "", errors.New(markerDown + " debe ir despues de " + markerUp + " y aparecer una sola vez")
			}
			current = &downLines
		default:
			if current != nil {
				*current = append(*current, line)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", err
	}
	up = strings.TrimSpace(strings.Join(upLines, "\n"))
	if up == "" {
		return "", "", errors.New("falta la seccion " + markerUp + " o esta vacia")
	}
	return up, strings.TrimSpace(strings.Join(downLines, "\n")), nil
}

// Up aplica en orden las migraciones pendientes sobre schema y devuelve las
// aplicadas. q debe ser una transaccion: si algo falla, el llamador hace
// rollback y no queda nada a medias.
func (s Set) Up(ctx context.Context, q database.Querier, schema string) ([]Migration, error) {
	var applied []Migration
	err := s.withSchema(ctx, q, schema, true, func(done map[int64]time.Time) error {
		latest := latestVersion(done)
		for _, m := range s.Migrations {
			if _, ok := done[m.Version]; ok {
				continue
			}
			if m.Version < latest {
				return fmt.Errorf("%s: la migracion %s no esta aplicada pero ya hay una posterior (version %d); revise el orden de las migraciones", schema, m.ID(), latest)
			}
			if _, err := q.Exec(ctx, m.Up); err != nil {
				return fmt.Errorf("%s: aplicando %s: %w", schema, m.ID(), err)
			}
			if _, err := q.Exec(ctx, insertVersionSQL(schema), m.Version, m.Name); err != nil {
				return fmt.Errorf("%s: registrando %s: %w", schema, m.ID(), err)
			}
			applied = append(applied, m)
		}
		return nil
	})
	return applied, err
}

// Down revierte la ultima migracion aplicada sobre schema. Devuelve nil si no
// habia nada que revertir. q debe ser una transaccion.
func (s Set) Down(ctx context.Context, q database.Querier, schema string) (*Migration, error) {
	var reverted *Migration
	err := s.withSchema(ctx, q, schema, true, func(done map[int64]time.Time) error {
		latest := latestVersion(done)
		if latest == 0 {
			return nil
		}
		idx := slices.IndexFunc(s.Migrations, func(m Migration) bool { return m.Version == latest })
		if idx < 0 {
			return fmt.Errorf("%s: la version aplicada %d no existe en los archivos de %s", schema, latest, s.Name)
		}
		m := s.Migrations[idx]
		if m.Down == "" {
			return fmt.Errorf("%s: %s no tiene seccion %s", schema, m.ID(), markerDown)
		}
		if _, err := q.Exec(ctx, m.Down); err != nil {
			return fmt.Errorf("%s: revirtiendo %s: %w", schema, m.ID(), err)
		}
		if _, err := q.Exec(ctx, deleteVersionSQL(schema), m.Version); err != nil {
			return fmt.Errorf("%s: desregistrando %s: %w", schema, m.ID(), err)
		}
		reverted = &m
		return nil
	})
	return reverted, err
}

// Status devuelve el estado de cada migracion del conjunto sobre schema.
// No modifica la base de datos.
func (s Set) Status(ctx context.Context, q database.Querier, schema string) ([]State, error) {
	var states []State
	err := s.withSchema(ctx, q, schema, false, func(done map[int64]time.Time) error {
		for _, m := range s.Migrations {
			state := State{Migration: m}
			if at, ok := done[m.Version]; ok {
				state.AppliedAt = &at
			}
			states = append(states, state)
		}
		return nil
	})
	return states, err
}

// Pending cuenta las migraciones no aplicadas.
func Pending(states []State) int {
	n := 0
	for _, s := range states {
		if s.AppliedAt == nil {
			n++
		}
	}
	return n
}

// withSchema serializa a los migradores concurrentes del mismo schema, fija el
// search_path al schema (para que el DDL sin calificar caiga ahi), lee las
// versiones aplicadas, ejecuta fn y restaura el search_path previo.
func (s Set) withSchema(ctx context.Context, q database.Querier, schema string, create bool, fn func(done map[int64]time.Time) error) error {
	if _, err := q.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "argos.migrate."+schema); err != nil {
		return fmt.Errorf("%s: obteniendo el bloqueo de migracion: %w", schema, err)
	}
	var previous string
	if err := q.QueryRow(ctx, "SELECT current_setting('search_path')").Scan(&previous); err != nil {
		return fmt.Errorf("%s: leyendo search_path: %w", schema, err)
	}
	searchPath := []string{schema}
	if schema != database.SharedSchema {
		searchPath = append(searchPath, database.SharedSchema)
	}
	if err := database.SetSearchPath(ctx, q, searchPath...); err != nil {
		return err
	}
	done, err := appliedVersions(ctx, q, schema, create)
	if err != nil {
		return err
	}
	if err := fn(done); err != nil {
		return err
	}
	if _, err := q.Exec(ctx, "SELECT set_config('search_path', $1, true)", previous); err != nil {
		return fmt.Errorf("%s: restaurando search_path: %w", schema, err)
	}
	return nil
}

func appliedVersions(ctx context.Context, q database.Querier, schema string, create bool) (map[int64]time.Time, error) {
	table := versionTable(schema)
	if create {
		if _, err := q.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+table+` (
			version    bigint      PRIMARY KEY,
			name       text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
			return nil, fmt.Errorf("%s: creando la tabla de versiones: %w", schema, err)
		}
	} else {
		var exists bool
		if err := q.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
			return nil, fmt.Errorf("%s: buscando la tabla de versiones: %w", schema, err)
		}
		if !exists {
			return map[int64]time.Time{}, nil
		}
	}

	rows, err := q.Query(ctx, "SELECT version, applied_at FROM "+table)
	if err != nil {
		return nil, fmt.Errorf("%s: leyendo versiones aplicadas: %w", schema, err)
	}
	done := map[int64]time.Time{}
	var version int64
	var appliedAt time.Time
	_, err = pgx.ForEachRow(rows, []any{&version, &appliedAt}, func() error {
		done[version] = appliedAt.UTC()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s: leyendo versiones aplicadas: %w", schema, err)
	}
	return done, nil
}

func latestVersion(done map[int64]time.Time) int64 {
	var latest int64
	for v := range done {
		latest = max(latest, v)
	}
	return latest
}

func versionTable(schema string) string {
	return pgx.Identifier{schema, "schema_migrations"}.Sanitize()
}

func insertVersionSQL(schema string) string {
	return "INSERT INTO " + versionTable(schema) + " (version, name) VALUES ($1, $2)"
}

func deleteVersionSQL(schema string) string {
	return "DELETE FROM " + versionTable(schema) + " WHERE version = $1"
}
