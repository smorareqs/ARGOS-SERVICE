# Migraciones

SQL plano, embebido en el binario (`migrations.go`) y aplicado por `argos migrate`
(motor en [`internal/core/migrate`](../internal/core/migrate)).

| Carpeta | Se aplica a | Tabla de versiones |
| --- | --- | --- |
| `shared/` | schema `shared` | `shared.schema_migrations` |
| `tenant/` | cada schema `tenant_<slug>`; también al dar de alta un tenant, en la misma transacción | `"tenant_<slug>".schema_migrations` |

## Formato

Un archivo por migración: `NNNN_nombre.sql` (minúsculas, números y `_`). La numeración
está fijada en `APP/docs/plans/reparto.md` §5.

```sql
-- Cabecera opcional: qué hace la migración y por qué.

-- migrate:up
CREATE TABLE files (
    id         uuid        NOT NULL DEFAULT gen_random_uuid(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_files PRIMARY KEY (id)
);

-- migrate:down
DROP TABLE files;
```

## Reglas

* En `shared/` las tablas se escriben siempre calificadas: `shared.tabla`.
* En `tenant/` las tablas van **sin schema**: el motor fija `search_path` al schema del
  tenant (seguido de `shared`) antes de ejecutar. Las claves foráneas a tablas globales
  sí se califican: `REFERENCES shared.users (id)`.
* Cada schema se migra en una transacción (todo o nada). No usar sentencias que no
  admiten transacción (`CREATE INDEX CONCURRENTLY`, `VACUUM`).
* Las versiones se aplican en orden. Si aparece una versión menor que la última aplicada,
  el motor se detiene (el equivalente a "dos cabezas" de Alembic): coordinar el orden
  según reparto.md antes de fusionar.
* Nunca editar una migración que ya está en `main`; crear una nueva.
* Nombres de constraints e índices (los mismos que generaba Alembic):
  `pk_<tabla>`, `fk_<tabla>_<columna>_<tabla_ref>`, `uq_<tabla>_<columna>`,
  `ck_<tabla>_<nombre>`, `ix_shared_<tabla>_<columna>` en shared e `ix_<tabla>_<columna>`
  en tenant.
* Enumeraciones como `varchar` + `CHECK` (no tipos `ENUM` de PostgreSQL).

## Comandos

```bash
argos migrate up                              # shared y después todos los tenants
argos migrate shared status
argos migrate shared down                     # revierte la última de shared
argos migrate tenant up -tenant garrent       # un tenant (por defecto: todos)
argos migrate tenant down -tenant all         # down exige -tenant explícito
```

En desarrollo, sin compilar: `go run ./cmd/argos migrate ...`. La API se niega a
arrancar si `shared` tiene migraciones pendientes.
