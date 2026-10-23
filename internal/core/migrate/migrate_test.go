package migrate

import (
	"testing"
	"testing/fstest"
	"time"
)

func TestLoadParsesAndSortsMigrations(t *testing.T) {
	fsys := fstest.MapFS{
		"set/0002_second.sql": {Data: []byte("-- migrate:up\nCREATE TABLE b ();\n-- migrate:down\nDROP TABLE b;\n")},
		"set/0001_first.sql":  {Data: []byte("-- cabecera ignorada\n-- migrate:up\nCREATE TABLE a ();\n")},
		"set/.gitkeep":        {},
		"set/README.md":       {Data: []byte("no es una migracion")},
	}
	set, err := Load(fsys, "set")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Migrations) != 2 {
		t.Fatalf("se esperaban 2 migraciones, hay %d", len(set.Migrations))
	}
	first, second := set.Migrations[0], set.Migrations[1]
	if first.ID() != "0001_first" || first.Up != "CREATE TABLE a ();" || first.Down != "" {
		t.Errorf("primera migracion inesperada: %+v", first)
	}
	if second.ID() != "0002_second" || second.Down != "DROP TABLE b;" {
		t.Errorf("segunda migracion inesperada: %+v", second)
	}
}

func TestLoadRejectsInvalidFiles(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"nombre invalido":    {"set/first.sql": {Data: []byte("-- migrate:up\nSELECT 1;")}},
		"mayusculas":         {"set/0001_First.sql": {Data: []byte("-- migrate:up\nSELECT 1;")}},
		"version duplicada":  {"set/0001_a.sql": {Data: []byte("-- migrate:up\nSELECT 1;")}, "set/001_b.sql": {Data: []byte("-- migrate:up\nSELECT 1;")}},
		"sin seccion up":     {"set/0001_a.sql": {Data: []byte("SELECT 1;")}},
		"up vacio":           {"set/0001_a.sql": {Data: []byte("-- migrate:up\n-- migrate:down\nSELECT 1;")}},
		"down antes que up":  {"set/0001_a.sql": {Data: []byte("-- migrate:down\nSELECT 1;\n-- migrate:up\nSELECT 1;")}},
		"up repetido":        {"set/0001_a.sql": {Data: []byte("-- migrate:up\nSELECT 1;\n-- migrate:up\nSELECT 2;")}},
		"version cero":       {"set/0000_a.sql": {Data: []byte("-- migrate:up\nSELECT 1;")}},
		"directorio ausente": {},
	}
	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(fsys, "set"); err == nil {
				t.Error("se esperaba un error")
			}
		})
	}
}

func TestEmbeddedMigrationsLoad(t *testing.T) {
	shared, err := Shared()
	if err != nil {
		t.Fatal(err)
	}
	if len(shared.Migrations) == 0 || shared.Migrations[0].ID() != "0001_initial" {
		t.Errorf("migraciones shared inesperadas: %+v", shared.Migrations)
	}
	if _, err := Tenant(); err != nil {
		t.Fatal(err)
	}
}

func TestPending(t *testing.T) {
	applied := time.Now()
	states := []State{{AppliedAt: &applied}, {}, {}}
	if got := Pending(states); got != 2 {
		t.Errorf("Pending = %d", got)
	}
}
