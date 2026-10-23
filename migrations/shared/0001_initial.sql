-- Tablas globales iniciales: tenants y users.
--
-- Usa IF NOT EXISTS para adoptar sin cambios una base creada por la migracion
-- de Alembic equivalente (3f1d8a233b4f) del backend anterior en Python: los
-- nombres de tablas, columnas, indices y constraints son identicos.

-- migrate:up
CREATE TABLE IF NOT EXISTS shared.tenants (
    id          uuid         NOT NULL DEFAULT gen_random_uuid(),
    slug        varchar(63)  NOT NULL,
    name        varchar(255) NOT NULL,
    schema_name varchar(63)  NOT NULL,
    is_active   boolean      NOT NULL,
    created_at  timestamptz  NOT NULL DEFAULT now(),
    updated_at  timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_tenants PRIMARY KEY (id),
    CONSTRAINT uq_tenants_schema_name UNIQUE (schema_name)
);
CREATE UNIQUE INDEX IF NOT EXISTS ix_shared_tenants_slug ON shared.tenants (slug);

CREATE TABLE IF NOT EXISTS shared.users (
    id              uuid         NOT NULL DEFAULT gen_random_uuid(),
    email           varchar(320) NOT NULL,
    hashed_password varchar(255) NOT NULL,
    full_name       varchar(255) NOT NULL,
    role            varchar(32)  NOT NULL,
    is_active       boolean      NOT NULL,
    is_owner        boolean      NOT NULL,
    -- Los platform_admin no pertenecen a ningun tenant.
    tenant_id       uuid,
    created_at      timestamptz  NOT NULL DEFAULT now(),
    updated_at      timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_users PRIMARY KEY (id),
    CONSTRAINT fk_users_tenant_id_tenants FOREIGN KEY (tenant_id)
        REFERENCES shared.tenants (id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS ix_shared_users_email ON shared.users (email);
CREATE INDEX IF NOT EXISTS ix_shared_users_tenant_id ON shared.users (tenant_id);

-- migrate:down
DROP TABLE IF EXISTS shared.users;
DROP TABLE IF EXISTS shared.tenants;
