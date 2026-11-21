-- Identidad multiempresa (APP/docs/plans/fase01.md §3.3 y §3.4).
--
-- * tenants y users ganan las columnas de la fase 1.
-- * Roles por tenant, permisos por rol, membresias usuario-tenant, refresh
--   tokens rotativos, tokens de un solo uso, invitaciones, tokens de push,
--   cola de trabajos y bitacora de plataforma.
-- * Migracion de datos: platform_admin pasa a users.is_platform_admin; cada
--   usuario con tenant obtiene una membresia con el rol equivalente
--   (employee -> cleaner). Los roles de sistema se insertan aqui solo con
--   clave y nombre: sus permisos los escribe `argos migrate up` desde la
--   matriz en Go (internal/modules/authz/default_roles.go), unica fuente.
--
-- Diferencia con el plan: refresh_tokens.tenant_id guarda el tenant activo de
-- la sesion para que /auth/refresh emita el access token del mismo tenant
-- (null = modo plataforma). Documentado en fase01.md §3.3.

-- migrate:up
ALTER TABLE shared.tenants
    ADD COLUMN IF NOT EXISTS timezone          varchar(64)  NOT NULL DEFAULT 'America/Guatemala',
    ADD COLUMN IF NOT EXISTS currency          char(3)      NOT NULL DEFAULT 'GTQ',
    ADD COLUMN IF NOT EXISTS logo_key          varchar(512),
    ADD COLUMN IF NOT EXISTS features          jsonb        NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS settings          jsonb        NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS require_2fa_roles jsonb        NOT NULL DEFAULT '[]';

ALTER TABLE shared.users
    ADD COLUMN IF NOT EXISTS phone               varchar(32),
    ADD COLUMN IF NOT EXISTS is_platform_admin   boolean      NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS totp_secret         varchar(255),
    ADD COLUMN IF NOT EXISTS totp_enabled        boolean      NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS password_changed_at timestamptz,
    ADD COLUMN IF NOT EXISTS last_login_at       timestamptz;
ALTER TABLE shared.users ALTER COLUMN hashed_password DROP NOT NULL;
-- El login compara el correo sin distinguir mayusculas.
CREATE UNIQUE INDEX ix_shared_users_email_lower ON shared.users (lower(email));

CREATE TABLE shared.roles (
    id                  uuid         NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid         NOT NULL,
    key                 varchar(64)  NOT NULL,
    name                varchar(128) NOT NULL,
    description         text,
    is_system           boolean      NOT NULL DEFAULT false,
    permissions_version integer      NOT NULL DEFAULT 1,
    created_at          timestamptz  NOT NULL DEFAULT now(),
    updated_at          timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_roles PRIMARY KEY (id),
    CONSTRAINT fk_roles_tenant_id_tenants FOREIGN KEY (tenant_id)
        REFERENCES shared.tenants (id) ON DELETE CASCADE,
    CONSTRAINT uq_roles_tenant_id_key UNIQUE (tenant_id, key)
);
CREATE INDEX ix_shared_roles_tenant_id ON shared.roles (tenant_id);

CREATE TABLE shared.role_permissions (
    role_id    uuid        NOT NULL,
    permission varchar(64) NOT NULL,
    CONSTRAINT pk_role_permissions PRIMARY KEY (role_id, permission),
    CONSTRAINT fk_role_permissions_role_id_roles FOREIGN KEY (role_id)
        REFERENCES shared.roles (id) ON DELETE CASCADE
);

CREATE TABLE shared.memberships (
    id         uuid        NOT NULL DEFAULT gen_random_uuid(),
    user_id    uuid        NOT NULL,
    tenant_id  uuid        NOT NULL,
    role_id    uuid        NOT NULL,
    status     varchar(16) NOT NULL,
    invited_by uuid,
    joined_at  timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_memberships PRIMARY KEY (id),
    CONSTRAINT fk_memberships_user_id_users FOREIGN KEY (user_id)
        REFERENCES shared.users (id) ON DELETE CASCADE,
    CONSTRAINT fk_memberships_tenant_id_tenants FOREIGN KEY (tenant_id)
        REFERENCES shared.tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_memberships_role_id_roles FOREIGN KEY (role_id)
        REFERENCES shared.roles (id) ON DELETE RESTRICT,
    CONSTRAINT fk_memberships_invited_by_users FOREIGN KEY (invited_by)
        REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT uq_memberships_user_id_tenant_id UNIQUE (user_id, tenant_id),
    CONSTRAINT ck_memberships_status CHECK (status IN ('invited', 'active', 'suspended'))
);
CREATE INDEX ix_shared_memberships_tenant_id ON shared.memberships (tenant_id);
CREATE INDEX ix_shared_memberships_user_id ON shared.memberships (user_id);

CREATE TABLE shared.refresh_tokens (
    id           uuid         NOT NULL DEFAULT gen_random_uuid(),
    user_id      uuid         NOT NULL,
    tenant_id    uuid,
    token_hash   char(64)     NOT NULL,
    family_id    uuid         NOT NULL,
    device_id    varchar(128) NOT NULL,
    device_name  varchar(128),
    platform     varchar(16)  NOT NULL,
    expires_at   timestamptz  NOT NULL,
    revoked_at   timestamptz,
    last_used_at timestamptz,
    ip           inet,
    created_at   timestamptz  NOT NULL DEFAULT now(),
    updated_at   timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_refresh_tokens PRIMARY KEY (id),
    CONSTRAINT fk_refresh_tokens_user_id_users FOREIGN KEY (user_id)
        REFERENCES shared.users (id) ON DELETE CASCADE,
    CONSTRAINT fk_refresh_tokens_tenant_id_tenants FOREIGN KEY (tenant_id)
        REFERENCES shared.tenants (id) ON DELETE CASCADE,
    CONSTRAINT uq_refresh_tokens_token_hash UNIQUE (token_hash),
    CONSTRAINT ck_refresh_tokens_platform CHECK (platform IN ('android', 'ios', 'web'))
);
CREATE INDEX ix_shared_refresh_tokens_user_id ON shared.refresh_tokens (user_id);
CREATE INDEX ix_shared_refresh_tokens_family_id ON shared.refresh_tokens (family_id);

CREATE TABLE shared.one_time_tokens (
    id         uuid        NOT NULL DEFAULT gen_random_uuid(),
    user_id    uuid,
    purpose    varchar(32) NOT NULL,
    token_hash char(64)    NOT NULL,
    payload    jsonb       NOT NULL DEFAULT '{}',
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_one_time_tokens PRIMARY KEY (id),
    CONSTRAINT fk_one_time_tokens_user_id_users FOREIGN KEY (user_id)
        REFERENCES shared.users (id) ON DELETE CASCADE,
    CONSTRAINT uq_one_time_tokens_token_hash UNIQUE (token_hash),
    CONSTRAINT ck_one_time_tokens_purpose
        CHECK (purpose IN ('password_reset', 'invitation', 'email_verify', 'magic_login'))
);

CREATE TABLE shared.invitations (
    id          uuid         NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   uuid         NOT NULL,
    email       varchar(320) NOT NULL,
    role_id     uuid         NOT NULL,
    invited_by  uuid,
    token_id    uuid,
    status      varchar(16)  NOT NULL,
    accepted_at timestamptz,
    created_at  timestamptz  NOT NULL DEFAULT now(),
    updated_at  timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_invitations PRIMARY KEY (id),
    CONSTRAINT fk_invitations_tenant_id_tenants FOREIGN KEY (tenant_id)
        REFERENCES shared.tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_invitations_role_id_roles FOREIGN KEY (role_id)
        REFERENCES shared.roles (id) ON DELETE CASCADE,
    CONSTRAINT fk_invitations_invited_by_users FOREIGN KEY (invited_by)
        REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT fk_invitations_token_id_one_time_tokens FOREIGN KEY (token_id)
        REFERENCES shared.one_time_tokens (id) ON DELETE SET NULL,
    CONSTRAINT ck_invitations_status CHECK (status IN ('pending', 'accepted', 'expired', 'revoked'))
);
CREATE UNIQUE INDEX ix_shared_invitations_tenant_id_email_pending
    ON shared.invitations (tenant_id, lower(email)) WHERE status = 'pending';

CREATE TABLE shared.device_tokens (
    id           uuid         NOT NULL DEFAULT gen_random_uuid(),
    user_id      uuid         NOT NULL,
    fcm_token    varchar(512) NOT NULL,
    platform     varchar(16)  NOT NULL,
    last_seen_at timestamptz  NOT NULL DEFAULT now(),
    created_at   timestamptz  NOT NULL DEFAULT now(),
    updated_at   timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_device_tokens PRIMARY KEY (id),
    CONSTRAINT fk_device_tokens_user_id_users FOREIGN KEY (user_id)
        REFERENCES shared.users (id) ON DELETE CASCADE,
    CONSTRAINT uq_device_tokens_fcm_token UNIQUE (fcm_token)
);

CREATE TABLE shared.jobs (
    id           uuid         NOT NULL DEFAULT gen_random_uuid(),
    tenant_id    uuid,
    type         varchar(64)  NOT NULL,
    payload      jsonb        NOT NULL DEFAULT '{}',
    status       varchar(16)  NOT NULL DEFAULT 'pending',
    scheduled_at timestamptz  NOT NULL DEFAULT now(),
    attempts     integer      NOT NULL DEFAULT 0,
    max_attempts integer      NOT NULL DEFAULT 5,
    locked_at    timestamptz,
    locked_by    varchar(64),
    last_error   text,
    finished_at  timestamptz,
    dedupe_key   varchar(128),
    created_at   timestamptz  NOT NULL DEFAULT now(),
    updated_at   timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_jobs PRIMARY KEY (id),
    CONSTRAINT fk_jobs_tenant_id_tenants FOREIGN KEY (tenant_id)
        REFERENCES shared.tenants (id) ON DELETE CASCADE,
    CONSTRAINT ck_jobs_status CHECK (status IN ('pending', 'running', 'done', 'failed', 'cancelled'))
);
CREATE INDEX ix_shared_jobs_status_scheduled_at ON shared.jobs (status, scheduled_at);
CREATE UNIQUE INDEX ix_shared_jobs_dedupe_key_active
    ON shared.jobs (dedupe_key) WHERE status IN ('pending', 'running');

CREATE TABLE shared.platform_audit_log (
    id            uuid        NOT NULL DEFAULT gen_random_uuid(),
    actor_user_id uuid,
    tenant_id     uuid,
    action        varchar(64) NOT NULL,
    entity        varchar(64) NOT NULL,
    entity_id     uuid,
    metadata      jsonb       NOT NULL DEFAULT '{}',
    ip            inet,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_platform_audit_log PRIMARY KEY (id),
    CONSTRAINT fk_platform_audit_log_actor_user_id_users FOREIGN KEY (actor_user_id)
        REFERENCES shared.users (id) ON DELETE SET NULL
);
CREATE INDEX ix_shared_platform_audit_log_created_at ON shared.platform_audit_log (created_at);

-- Migracion de datos ---------------------------------------------------------
UPDATE shared.users SET is_platform_admin = true WHERE role = 'platform_admin';

INSERT INTO shared.roles (tenant_id, key, name, is_system)
SELECT t.id, r.key, r.name, true
FROM shared.tenants t
CROSS JOIN (VALUES
    ('admin', 'Administrador'),
    ('supervisor', 'Supervisor'),
    ('cleaner', 'Colaborador de limpieza'),
    ('maintenance', 'Mantenimiento'),
    ('security', 'Seguridad'),
    ('owner', 'Propietario'),
    ('guest', 'Huésped')
) AS r (key, name)
ON CONFLICT (tenant_id, key) DO NOTHING;

INSERT INTO shared.memberships (user_id, tenant_id, role_id, status, joined_at)
SELECT u.id, u.tenant_id, r.id, CASE WHEN u.is_active THEN 'active' ELSE 'suspended' END, u.created_at
FROM shared.users u
JOIN shared.roles r
  ON r.tenant_id = u.tenant_id
 AND r.key = CASE u.role WHEN 'employee' THEN 'cleaner' ELSE u.role END
WHERE u.tenant_id IS NOT NULL;

DROP INDEX IF EXISTS shared.ix_shared_users_tenant_id;
ALTER TABLE shared.users
    DROP COLUMN role,
    DROP COLUMN tenant_id,
    DROP COLUMN is_owner;

-- migrate:down
-- Reversion con perdida: cada usuario recupera solo su primera membresia
-- activa; las demas se descartan.
ALTER TABLE shared.users
    ADD COLUMN role      varchar(32) NOT NULL DEFAULT 'employee',
    ADD COLUMN tenant_id uuid,
    ADD COLUMN is_owner  boolean     NOT NULL DEFAULT false;
ALTER TABLE shared.users
    ADD CONSTRAINT fk_users_tenant_id_tenants FOREIGN KEY (tenant_id)
        REFERENCES shared.tenants (id) ON DELETE CASCADE;
CREATE INDEX ix_shared_users_tenant_id ON shared.users (tenant_id);

UPDATE shared.users u
SET tenant_id = m.tenant_id,
    role = CASE r.key WHEN 'admin' THEN 'admin' WHEN 'supervisor' THEN 'supervisor' ELSE 'employee' END
FROM (
    SELECT DISTINCT ON (user_id) user_id, tenant_id, role_id
    FROM shared.memberships
    WHERE status = 'active'
    ORDER BY user_id, created_at
) m
JOIN shared.roles r ON r.id = m.role_id
WHERE u.id = m.user_id;
UPDATE shared.users SET role = 'platform_admin' WHERE is_platform_admin;
ALTER TABLE shared.users ALTER COLUMN role DROP DEFAULT;

DROP TABLE shared.platform_audit_log;
DROP TABLE shared.jobs;
DROP TABLE shared.device_tokens;
DROP TABLE shared.invitations;
DROP TABLE shared.one_time_tokens;
DROP TABLE shared.refresh_tokens;
DROP TABLE shared.memberships;
DROP TABLE shared.role_permissions;
DROP TABLE shared.roles;

DROP INDEX shared.ix_shared_users_email_lower;
UPDATE shared.users SET hashed_password = '' WHERE hashed_password IS NULL;
ALTER TABLE shared.users ALTER COLUMN hashed_password SET NOT NULL;
ALTER TABLE shared.users
    DROP COLUMN last_login_at,
    DROP COLUMN password_changed_at,
    DROP COLUMN totp_enabled,
    DROP COLUMN totp_secret,
    DROP COLUMN is_platform_admin,
    DROP COLUMN phone;

ALTER TABLE shared.tenants
    DROP COLUMN require_2fa_roles,
    DROP COLUMN settings,
    DROP COLUMN features,
    DROP COLUMN logo_key,
    DROP COLUMN currency,
    DROP COLUMN timezone;
