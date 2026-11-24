-- Mantenimiento y alertas (APP/docs/plans/fase03.md §2, parte de Persona A):
-- proveedores, activos por propiedad o edificio, planes preventivos,
-- incidencias (tickets) con su linea de tiempo y alertas operativas. Las
-- notificaciones (0005_notifications, Persona B) se aplican despues.
--
-- Diferencias con el plan:
-- * `maintenance_tickets.number`: folio corto por empresa ("#42") que la app
--   muestra al reportar (ux-ui.md F3).
-- * `maintenance_tickets.building_id`: el preventivo de un activo del edificio
--   (elevador) no es de ninguna propiedad. Un ticket es de una propiedad, de
--   un edificio o de ambos (se reporto desde la propiedad sobre el elevador).
-- * `files.original_name`: nombre del archivo subido (documentos de activos).
-- * `alerts.resolved_by`: quien la resolvio a mano (las que se resuelven solas
--   lo dejan vacio).
-- * `maintenance_tickets.task_id` apunta a `tasks` (0003, Persona B). La FK se
--   crea solo si la tabla existe: en el orden normal (0003 antes que 0004)
--   siempre existe. Una base local que aplico 0004 sin 0003 debe revertir esta
--   migracion antes de aplicar 0003 (el motor detiene el desorden).
-- * `reported_by_guest_id` recibe su FK en 0006_reservations (fase 4).

-- migrate:up
ALTER TABLE files ADD COLUMN original_name varchar(255);

CREATE TABLE vendors (
    id           uuid         NOT NULL DEFAULT gen_random_uuid(),
    name         varchar(128) NOT NULL,
    service_type varchar(64),
    contact_name varchar(128),
    phone        varchar(32),
    email        varchar(254),
    notes        text,
    is_active    boolean      NOT NULL DEFAULT true,
    created_at   timestamptz  NOT NULL DEFAULT now(),
    updated_at   timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_vendors PRIMARY KEY (id)
);
CREATE UNIQUE INDEX uq_vendors_name ON vendors (lower(name));

CREATE TABLE assets (
    id             uuid         NOT NULL DEFAULT gen_random_uuid(),
    property_id    uuid,
    building_id    uuid,
    category       varchar(24)  NOT NULL,
    name           varchar(128) NOT NULL,
    brand          varchar(128),
    model          varchar(128),
    serial         varchar(128),
    location       varchar(128),
    installed_at   date,
    warranty_until date,
    vendor_id      uuid,
    status         varchar(24)  NOT NULL DEFAULT 'operational',
    notes          text,
    is_active      boolean      NOT NULL DEFAULT true,
    created_at     timestamptz  NOT NULL DEFAULT now(),
    updated_at     timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_assets PRIMARY KEY (id),
    CONSTRAINT fk_assets_property_id_properties FOREIGN KEY (property_id) REFERENCES properties (id),
    CONSTRAINT fk_assets_building_id_buildings FOREIGN KEY (building_id) REFERENCES buildings (id),
    CONSTRAINT fk_assets_vendor_id_vendors FOREIGN KEY (vendor_id) REFERENCES vendors (id) ON DELETE SET NULL,
    -- Un activo es de una propiedad o de un edificio (elevadores, camaras).
    CONSTRAINT ck_assets_owner CHECK (num_nonnulls(property_id, building_id) = 1),
    CONSTRAINT ck_assets_category CHECK (category IN ('elevator', 'camera', 'water', 'paint', 'appliance',
        'plumbing', 'electrical', 'hvac', 'hospitality', 'furniture', 'other')),
    CONSTRAINT ck_assets_status CHECK (status IN ('operational', 'needs_attention', 'in_repair', 'out_of_service'))
);
CREATE INDEX ix_assets_property_id ON assets (property_id);
CREATE INDEX ix_assets_building_id ON assets (building_id);
CREATE INDEX ix_assets_vendor_id ON assets (vendor_id);

CREATE TABLE maintenance_plans (
    id                  uuid         NOT NULL DEFAULT gen_random_uuid(),
    asset_id            uuid         NOT NULL,
    name                varchar(128) NOT NULL,
    frequency_days      integer      NOT NULL,
    responsible_user_id uuid,
    vendor_id           uuid,
    last_done_at        date,
    next_due_at         date         NOT NULL,
    lead_days           integer      NOT NULL DEFAULT 7,
    is_active           boolean      NOT NULL DEFAULT true,
    created_at          timestamptz  NOT NULL DEFAULT now(),
    updated_at          timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_maintenance_plans PRIMARY KEY (id),
    CONSTRAINT fk_maintenance_plans_asset_id_assets FOREIGN KEY (asset_id) REFERENCES assets (id) ON DELETE CASCADE,
    CONSTRAINT fk_maintenance_plans_responsible_user_id_users FOREIGN KEY (responsible_user_id)
        REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT fk_maintenance_plans_vendor_id_vendors FOREIGN KEY (vendor_id) REFERENCES vendors (id) ON DELETE SET NULL,
    CONSTRAINT ck_maintenance_plans_frequency_days CHECK (frequency_days BETWEEN 1 AND 3650),
    CONSTRAINT ck_maintenance_plans_lead_days CHECK (lead_days BETWEEN 0 AND 365)
);
CREATE INDEX ix_maintenance_plans_asset_id ON maintenance_plans (asset_id);
CREATE INDEX ix_maintenance_plans_next_due_at ON maintenance_plans (next_due_at) WHERE is_active;

CREATE TABLE maintenance_tickets (
    id                   uuid          NOT NULL DEFAULT gen_random_uuid(),
    number               integer       GENERATED BY DEFAULT AS IDENTITY,
    property_id          uuid,
    building_id          uuid,
    asset_id             uuid,
    plan_id              uuid,
    title                varchar(160)  NOT NULL,
    description          text          NOT NULL DEFAULT '',
    urgency              varchar(16)   NOT NULL,
    status               varchar(16)   NOT NULL DEFAULT 'open',
    reported_by_user_id  uuid,
    reported_by_guest_id uuid,
    assigned_to          uuid,
    vendor_id            uuid,
    task_id              uuid,
    resolved_at          timestamptz,
    closed_at            timestamptz,
    resolution           text,
    cost                 numeric(12,2),
    created_at           timestamptz   NOT NULL DEFAULT now(),
    updated_at           timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_maintenance_tickets PRIMARY KEY (id),
    CONSTRAINT uq_maintenance_tickets_number UNIQUE (number),
    CONSTRAINT fk_maintenance_tickets_property_id_properties FOREIGN KEY (property_id) REFERENCES properties (id),
    CONSTRAINT fk_maintenance_tickets_building_id_buildings FOREIGN KEY (building_id) REFERENCES buildings (id),
    CONSTRAINT fk_maintenance_tickets_asset_id_assets FOREIGN KEY (asset_id) REFERENCES assets (id) ON DELETE SET NULL,
    CONSTRAINT fk_maintenance_tickets_plan_id_maintenance_plans FOREIGN KEY (plan_id)
        REFERENCES maintenance_plans (id) ON DELETE SET NULL,
    CONSTRAINT fk_maintenance_tickets_reported_by_user_id_users FOREIGN KEY (reported_by_user_id)
        REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT fk_maintenance_tickets_assigned_to_users FOREIGN KEY (assigned_to)
        REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT fk_maintenance_tickets_vendor_id_vendors FOREIGN KEY (vendor_id) REFERENCES vendors (id) ON DELETE SET NULL,
    CONSTRAINT ck_maintenance_tickets_place CHECK (num_nonnulls(property_id, building_id) >= 1),
    CONSTRAINT ck_maintenance_tickets_urgency CHECK (urgency IN ('low', 'medium', 'high', 'critical')),
    CONSTRAINT ck_maintenance_tickets_status CHECK (status IN ('open', 'assigned', 'in_progress', 'resolved', 'closed', 'rejected')),
    CONSTRAINT ck_maintenance_tickets_cost CHECK (cost >= 0)
);
CREATE INDEX ix_maintenance_tickets_property_id_status ON maintenance_tickets (property_id, status);
CREATE INDEX ix_maintenance_tickets_status_created_at ON maintenance_tickets (status, created_at);
CREATE INDEX ix_maintenance_tickets_assigned_to ON maintenance_tickets (assigned_to)
    WHERE status IN ('assigned', 'in_progress');
CREATE INDEX ix_maintenance_tickets_asset_id ON maintenance_tickets (asset_id);
CREATE INDEX ix_maintenance_tickets_building_id ON maintenance_tickets (building_id);
-- Un preventivo tiene a lo mas un ticket abierto: el job maintenance_due es idempotente.
CREATE UNIQUE INDEX uq_maintenance_tickets_open_plan ON maintenance_tickets (plan_id)
    WHERE plan_id IS NOT NULL AND status IN ('open', 'assigned', 'in_progress');

DO $$
BEGIN
    IF to_regclass('tasks') IS NOT NULL THEN
        ALTER TABLE maintenance_tickets ADD CONSTRAINT fk_maintenance_tickets_task_id_tasks
            FOREIGN KEY (task_id) REFERENCES tasks (id) ON DELETE SET NULL;
    END IF;
END $$;

-- Linea de tiempo del ticket (inmutable).
CREATE TABLE ticket_events (
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    ticket_id      uuid        NOT NULL,
    type           varchar(32) NOT NULL,
    actor_user_id  uuid,
    actor_guest_id uuid,
    data           jsonb       NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_ticket_events PRIMARY KEY (id),
    CONSTRAINT fk_ticket_events_ticket_id_maintenance_tickets FOREIGN KEY (ticket_id)
        REFERENCES maintenance_tickets (id) ON DELETE CASCADE,
    CONSTRAINT fk_ticket_events_actor_user_id_users FOREIGN KEY (actor_user_id)
        REFERENCES shared.users (id) ON DELETE SET NULL
);
CREATE INDEX ix_ticket_events_ticket_id_created_at ON ticket_events (ticket_id, created_at);

CREATE TABLE alerts (
    id              uuid         NOT NULL DEFAULT gen_random_uuid(),
    type            varchar(32)  NOT NULL,
    severity        varchar(8)   NOT NULL,
    entity_type     varchar(32)  NOT NULL,
    entity_id       uuid         NOT NULL,
    property_id     uuid,
    message         varchar(255) NOT NULL,
    status          varchar(16)  NOT NULL DEFAULT 'active',
    acknowledged_by uuid,
    acknowledged_at timestamptz,
    resolved_by     uuid,
    resolved_at     timestamptz,
    created_at      timestamptz  NOT NULL DEFAULT now(),
    updated_at      timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_alerts PRIMARY KEY (id),
    CONSTRAINT fk_alerts_property_id_properties FOREIGN KEY (property_id) REFERENCES properties (id) ON DELETE SET NULL,
    CONSTRAINT fk_alerts_acknowledged_by_users FOREIGN KEY (acknowledged_by) REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT fk_alerts_resolved_by_users FOREIGN KEY (resolved_by) REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT ck_alerts_type CHECK (type IN ('asset_failure', 'maintenance_overdue', 'task_late', 'low_stock',
        'discrepancy_open', 'checkout_alarm_unattended')),
    CONSTRAINT ck_alerts_severity CHECK (severity IN ('low', 'medium', 'high')),
    CONSTRAINT ck_alerts_status CHECK (status IN ('active', 'acknowledged', 'resolved'))
);
-- Una sola alerta viva por causa: levantarla de nuevo no la duplica.
CREATE UNIQUE INDEX uq_alerts_open_entity ON alerts (type, entity_type, entity_id) WHERE status <> 'resolved';
CREATE INDEX ix_alerts_status_severity ON alerts (status, severity);
CREATE INDEX ix_alerts_property_id ON alerts (property_id);
CREATE INDEX ix_alerts_created_at ON alerts (created_at);

-- migrate:down
DROP TABLE alerts;
DROP TABLE ticket_events;
DROP TABLE maintenance_tickets;
DROP TABLE maintenance_plans;
DROP TABLE assets;
DROP TABLE vendors;
ALTER TABLE files DROP COLUMN original_name;
