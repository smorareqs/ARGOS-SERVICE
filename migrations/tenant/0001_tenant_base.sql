-- Tablas transversales de cada tenant (APP/docs/plans/fase01.md §3.5): archivos
-- (S3) y bitacora de cambios. Las tablas van sin schema: el motor fija el
-- search_path al schema del tenant.

-- migrate:up
CREATE TABLE files (
    id                   uuid         NOT NULL DEFAULT gen_random_uuid(),
    s3_key               varchar(512) NOT NULL,
    module               varchar(32)  NOT NULL,
    kind                 varchar(32)  NOT NULL,
    entity_type          varchar(64),
    entity_id            uuid,
    content_type         varchar(128) NOT NULL,
    size_bytes           bigint       NOT NULL,
    sha256               char(64),
    uploaded_by_user_id  uuid,
    uploaded_by_guest_id uuid,
    status               varchar(16)  NOT NULL DEFAULT 'pending',
    expires_at           timestamptz,
    deleted_at           timestamptz,
    created_at           timestamptz  NOT NULL DEFAULT now(),
    updated_at           timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_files PRIMARY KEY (id),
    CONSTRAINT uq_files_s3_key UNIQUE (s3_key),
    CONSTRAINT fk_files_uploaded_by_user_id_users FOREIGN KEY (uploaded_by_user_id)
        REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT ck_files_status CHECK (status IN ('pending', 'ready', 'deleted'))
);
CREATE INDEX ix_files_entity ON files (entity_type, entity_id);
CREATE INDEX ix_files_expires_at ON files (expires_at) WHERE expires_at IS NOT NULL;

CREATE TABLE audit_log (
    id             uuid         NOT NULL DEFAULT gen_random_uuid(),
    actor_user_id  uuid,
    actor_guest_id uuid,
    action         varchar(64)  NOT NULL,
    entity         varchar(64)  NOT NULL,
    entity_id      uuid,
    property_id    uuid,
    before         jsonb,
    after          jsonb,
    ip             inet,
    user_agent     varchar(256),
    created_at     timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_audit_log PRIMARY KEY (id),
    CONSTRAINT fk_audit_log_actor_user_id_users FOREIGN KEY (actor_user_id)
        REFERENCES shared.users (id) ON DELETE SET NULL
);
CREATE INDEX ix_audit_log_created_at ON audit_log (created_at);
CREATE INDEX ix_audit_log_entity ON audit_log (entity, entity_id);
CREATE INDEX ix_audit_log_actor_user_id ON audit_log (actor_user_id);
CREATE INDEX ix_audit_log_property_id ON audit_log (property_id);

-- migrate:down
DROP TABLE audit_log;
DROP TABLE files;
