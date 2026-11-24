-- Facturacion de empresas y claves API (APP/docs/plans/fase06.md §3 y §4.1).
--
-- * plans: catalogo de planes con precio mensual y anual y sus limites
--   (max_properties, max_users, features). Se siembran Inicio, Operacion y
--   Completo; la plataforma los edita (W-P2).
-- * subscriptions: una por empresa. Estados trialing, active, past_due,
--   suspended y cancelled; suspended y cancelled dejan a la empresa en solo
--   lectura (402 en toda escritura salvo facturacion).
-- * invoices, payments y payment_events (webhooks, idempotentes por event_id).
-- * api_keys: claves por empresa para POST /public/reservations. Solo se
--   guarda el hash SHA-256; la clave se muestra una vez al crearla.
--
-- Las empresas que ya existen quedan activas en el plan Completo con un
-- periodo mensual desde hoy (sin factura: la primera la genera billing_cycle
-- al terminar el periodo).

-- migrate:up
CREATE TABLE shared.plans (
    id            uuid          NOT NULL DEFAULT gen_random_uuid(),
    key           varchar(32)   NOT NULL,
    name          varchar(64)   NOT NULL,
    description   varchar(255)  NOT NULL DEFAULT '',
    price_monthly numeric(12,2) NOT NULL,
    price_yearly  numeric(12,2) NOT NULL,
    currency      char(3)       NOT NULL DEFAULT 'GTQ',
    limits        jsonb         NOT NULL DEFAULT '{}',
    is_active     boolean       NOT NULL DEFAULT true,
    sort_order    smallint      NOT NULL DEFAULT 0,
    created_at    timestamptz   NOT NULL DEFAULT now(),
    updated_at    timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_plans PRIMARY KEY (id),
    CONSTRAINT uq_plans_key UNIQUE (key),
    CONSTRAINT ck_plans_prices CHECK (price_monthly >= 0 AND price_yearly >= 0)
);

INSERT INTO shared.plans (key, name, description, price_monthly, price_yearly, limits, sort_order) VALUES
    ('inicio', 'Inicio', 'Para empezar a ordenar la operación de pocas propiedades.', 690, 6900,
     '{"max_properties": 8, "max_users": 10, "features": ["reservations"]}', 1),
    ('operacion', 'Operación', 'Reservas, parqueos, áreas comunes y nómina para una operación mediana.', 1450, 14500,
     '{"max_properties": 25, "max_users": 30, "features": ["reservations", "parking", "shared_areas", "payroll"]}', 2),
    ('completo', 'Completo', 'Todo ARGOS, sin límites de propiedades ni de personas.', 2900, 29000,
     '{"max_properties": null, "max_users": null, "features": ["reservations", "parking", "shared_areas", "guest_chat", "payroll", "smart_locks"]}', 3);

CREATE TABLE shared.subscriptions (
    id                       uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id                uuid        NOT NULL,
    plan_id                  uuid        NOT NULL,
    cycle                    varchar(8)  NOT NULL DEFAULT 'monthly',
    status                   varchar(16) NOT NULL DEFAULT 'trialing',
    current_period_start     timestamptz NOT NULL DEFAULT now(),
    current_period_end       timestamptz NOT NULL,
    trial_ends_at            timestamptz,
    grace_days               smallint    NOT NULL DEFAULT 7,
    provider                 varchar(32) NOT NULL DEFAULT 'fake',
    provider_customer_id     varchar(128),
    provider_subscription_id varchar(128),
    payment_method           varchar(64),
    cancel_at_period_end     boolean     NOT NULL DEFAULT false,
    status_changed_at        timestamptz NOT NULL DEFAULT now(),
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_subscriptions PRIMARY KEY (id),
    CONSTRAINT uq_subscriptions_tenant_id UNIQUE (tenant_id),
    CONSTRAINT fk_subscriptions_tenant_id_tenants FOREIGN KEY (tenant_id)
        REFERENCES shared.tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_subscriptions_plan_id_plans FOREIGN KEY (plan_id) REFERENCES shared.plans (id),
    CONSTRAINT ck_subscriptions_cycle CHECK (cycle IN ('monthly', 'yearly')),
    CONSTRAINT ck_subscriptions_status CHECK (status IN ('trialing', 'active', 'past_due', 'suspended', 'cancelled')),
    CONSTRAINT ck_subscriptions_period CHECK (current_period_end > current_period_start),
    CONSTRAINT ck_subscriptions_grace CHECK (grace_days BETWEEN 0 AND 60)
);
CREATE INDEX ix_shared_subscriptions_status ON shared.subscriptions (status);

CREATE SEQUENCE shared.invoice_number_seq;

CREATE TABLE shared.invoices (
    id              uuid          NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid          NOT NULL,
    subscription_id uuid          NOT NULL,
    plan_id         uuid          NOT NULL,
    number          varchar(32)   NOT NULL,
    description     varchar(255)  NOT NULL,
    period_start    timestamptz   NOT NULL,
    period_end      timestamptz   NOT NULL,
    subtotal        numeric(12,2) NOT NULL,
    tax             numeric(12,2) NOT NULL,
    total           numeric(12,2) NOT NULL,
    currency        char(3)       NOT NULL,
    status          varchar(16)   NOT NULL DEFAULT 'open',
    due_at          timestamptz   NOT NULL,
    paid_at         timestamptz,
    pdf_key         varchar(512),
    created_at      timestamptz   NOT NULL DEFAULT now(),
    updated_at      timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_invoices PRIMARY KEY (id),
    CONSTRAINT uq_invoices_number UNIQUE (number),
    CONSTRAINT fk_invoices_tenant_id_tenants FOREIGN KEY (tenant_id)
        REFERENCES shared.tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_invoices_subscription_id_subscriptions FOREIGN KEY (subscription_id)
        REFERENCES shared.subscriptions (id) ON DELETE CASCADE,
    CONSTRAINT fk_invoices_plan_id_plans FOREIGN KEY (plan_id) REFERENCES shared.plans (id),
    CONSTRAINT ck_invoices_status CHECK (status IN ('draft', 'open', 'paid', 'void', 'uncollectible')),
    CONSTRAINT ck_invoices_amounts CHECK (subtotal >= 0 AND tax >= 0 AND total = subtotal + tax)
);
CREATE INDEX ix_shared_invoices_tenant_id_created_at ON shared.invoices (tenant_id, created_at DESC);
CREATE INDEX ix_shared_invoices_open ON shared.invoices (due_at) WHERE status = 'open';

CREATE TABLE shared.payments (
    id                  uuid          NOT NULL DEFAULT gen_random_uuid(),
    invoice_id          uuid          NOT NULL,
    provider            varchar(32)   NOT NULL,
    provider_payment_id varchar(128),
    amount              numeric(12,2) NOT NULL,
    currency            char(3)       NOT NULL,
    status              varchar(16)   NOT NULL,
    failure_reason      varchar(255),
    raw                 jsonb         NOT NULL DEFAULT '{}',
    created_at          timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_payments PRIMARY KEY (id),
    CONSTRAINT fk_payments_invoice_id_invoices FOREIGN KEY (invoice_id)
        REFERENCES shared.invoices (id) ON DELETE CASCADE,
    CONSTRAINT ck_payments_status CHECK (status IN ('pending', 'succeeded', 'failed', 'refunded'))
);
CREATE INDEX ix_shared_payments_invoice_id ON shared.payments (invoice_id);

CREATE TABLE shared.payment_events (
    id           uuid        NOT NULL DEFAULT gen_random_uuid(),
    provider     varchar(32) NOT NULL,
    event_id     varchar(128) NOT NULL,
    type         varchar(64) NOT NULL,
    payload      jsonb       NOT NULL DEFAULT '{}',
    processed_at timestamptz,
    error        text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_payment_events PRIMARY KEY (id),
    CONSTRAINT uq_payment_events_provider_event_id UNIQUE (provider, event_id)
);

CREATE TABLE shared.api_keys (
    id           uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL,
    name         varchar(64) NOT NULL,
    key_prefix   char(8)     NOT NULL,
    key_hash     char(64)    NOT NULL,
    scopes       jsonb       NOT NULL DEFAULT '["reservations:write"]',
    last_used_at timestamptz,
    revoked_at   timestamptz,
    created_by   uuid,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_api_keys PRIMARY KEY (id),
    CONSTRAINT uq_api_keys_key_hash UNIQUE (key_hash),
    CONSTRAINT fk_api_keys_tenant_id_tenants FOREIGN KEY (tenant_id)
        REFERENCES shared.tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_api_keys_created_by_users FOREIGN KEY (created_by)
        REFERENCES shared.users (id) ON DELETE SET NULL
);
CREATE INDEX ix_shared_api_keys_tenant_id ON shared.api_keys (tenant_id);

-- Empresas existentes: activas en el plan Completo.
INSERT INTO shared.subscriptions (tenant_id, plan_id, cycle, status, current_period_start, current_period_end)
SELECT t.id, p.id, 'monthly', 'active', now(), now() + interval '1 month'
FROM shared.tenants t
CROSS JOIN shared.plans p
WHERE p.key = 'completo';

-- migrate:down
DROP TABLE IF EXISTS shared.api_keys;
DROP TABLE IF EXISTS shared.payment_events;
DROP TABLE IF EXISTS shared.payments;
DROP TABLE IF EXISTS shared.invoices;
DROP SEQUENCE IF EXISTS shared.invoice_number_seq;
DROP TABLE IF EXISTS shared.subscriptions;
DROP TABLE IF EXISTS shared.plans;
