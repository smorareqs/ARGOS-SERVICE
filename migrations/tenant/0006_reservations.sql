-- Reservas y hospitalidad (APP/docs/plans/fase04.md §2, parte de Persona A):
-- canales de venta, calendarios iCal por propiedad, huespedes, reservas con su
-- linea de tiempo, depositos e inspecciones antes y despues de la estancia.
-- Accesos de huesped, parqueos, areas comunes y alarmas de seguridad son de
-- 0007_guest_parking_areas (Persona B).
--
-- Diferencias con el plan:
-- * `guests.document_type` y `document_number` son opcionales: en una venta
--   directa el huesped se crea con nombre y correo, y el documento se registra
--   antes del check-in. El par es unico cuando existe.
-- * `guests.blacklist_reason`: por que esta en lista negra (W-H5).
-- * `reservations.manual_fields`: campos que alguien corrigio a mano en una
--   reserva importada; la sincronizacion iCal ya no los sobrescribe.
-- * `reservations.external_summary`: el SUMMARY del evento iCal.
-- * `property_ical_feeds.last_success_at` y `events_count`: estado de la
--   ultima lectura correcta (W-H6).
-- * Una inspeccion por tipo y reserva (UNIQUE).
-- * Los canales Directo, Booking.com y Airbnb se siembran aqui (en todas las
--   empresas; solo se usan con la bandera reservations).
-- * Las FK hacia `tasks` (0003, Persona B) se crean solo si la tabla y la
--   columna existen; en el orden normal siempre existen.
-- * Requiere la extension btree_gist (shared/0004_btree_gist).

-- migrate:up
CREATE TABLE booking_channels (
    id                     uuid          NOT NULL DEFAULT gen_random_uuid(),
    name                   varchar(64)   NOT NULL,
    kind                   varchar(16)   NOT NULL,
    requires_deposit       boolean       NOT NULL DEFAULT false,
    has_checkin            boolean       NOT NULL DEFAULT false,
    default_deposit_amount numeric(12,2),
    is_active              boolean       NOT NULL DEFAULT true,
    created_at             timestamptz   NOT NULL DEFAULT now(),
    updated_at             timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_booking_channels PRIMARY KEY (id),
    CONSTRAINT ck_booking_channels_kind CHECK (kind IN ('direct', 'booking', 'airbnb', 'agency', 'other')),
    CONSTRAINT ck_booking_channels_deposit CHECK (default_deposit_amount >= 0)
);
CREATE UNIQUE INDEX uq_booking_channels_name ON booking_channels (lower(name));

INSERT INTO booking_channels (name, kind, requires_deposit, has_checkin) VALUES
    ('Directo', 'direct', true, true),
    ('Booking.com', 'booking', false, false),
    ('Airbnb', 'airbnb', false, false);

CREATE TABLE property_ical_feeds (
    id              uuid         NOT NULL DEFAULT gen_random_uuid(),
    property_id     uuid         NOT NULL,
    channel_id      uuid         NOT NULL,
    url             text         NOT NULL,
    is_active       boolean      NOT NULL DEFAULT true,
    last_synced_at  timestamptz,
    last_success_at timestamptz,
    last_error      text,
    events_count    integer,
    created_at      timestamptz  NOT NULL DEFAULT now(),
    updated_at      timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_property_ical_feeds PRIMARY KEY (id),
    CONSTRAINT uq_property_ical_feeds_property_id_channel_id UNIQUE (property_id, channel_id),
    CONSTRAINT fk_property_ical_feeds_property_id_properties FOREIGN KEY (property_id)
        REFERENCES properties (id) ON DELETE CASCADE,
    CONSTRAINT fk_property_ical_feeds_channel_id_booking_channels FOREIGN KEY (channel_id)
        REFERENCES booking_channels (id),
    CONSTRAINT ck_property_ical_feeds_url CHECK (char_length(url) <= 2048)
);

CREATE TABLE guests (
    id               uuid         NOT NULL DEFAULT gen_random_uuid(),
    full_name        varchar(160) NOT NULL,
    email            varchar(320),
    phone            varchar(32),
    document_type    varchar(16),
    document_number  varchar(32),
    nationality      char(2),
    notes            text,
    blacklisted      boolean      NOT NULL DEFAULT false,
    blacklist_reason varchar(500),
    created_at       timestamptz  NOT NULL DEFAULT now(),
    updated_at       timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_guests PRIMARY KEY (id),
    CONSTRAINT ck_guests_document_type CHECK (document_type IN ('dpi', 'passport')),
    CONSTRAINT ck_guests_document CHECK ((document_type IS NULL) = (document_number IS NULL))
);
CREATE UNIQUE INDEX uq_guests_document ON guests (document_type, document_number) WHERE document_number IS NOT NULL;
CREATE INDEX ix_guests_full_name ON guests (lower(full_name));
CREATE INDEX ix_guests_email ON guests (lower(email));

CREATE TABLE reservations (
    id               uuid          NOT NULL DEFAULT gen_random_uuid(),
    property_id      uuid          NOT NULL,
    channel_id       uuid          NOT NULL,
    external_ref     varchar(255),
    external_summary varchar(255),
    code             varchar(12)   NOT NULL,
    primary_guest_id uuid,
    status           varchar(16)   NOT NULL DEFAULT 'pending',
    check_in_date    date          NOT NULL,
    check_out_date   date          NOT NULL,
    check_in_time    time          NOT NULL,
    check_out_time   time          NOT NULL,
    checked_in_at    timestamptz,
    checked_out_at   timestamptz,
    adults           smallint      NOT NULL DEFAULT 1,
    children         smallint      NOT NULL DEFAULT 0,
    total_amount     numeric(12,2),
    currency         char(3)       NOT NULL,
    deposit_amount   numeric(12,2),
    deposit_status   varchar(16)   NOT NULL DEFAULT 'none',
    source           varchar(16)   NOT NULL DEFAULT 'manual',
    manual_fields    text[]        NOT NULL DEFAULT '{}',
    notes            text,
    created_by       uuid,
    cancelled_at     timestamptz,
    cancel_reason    text,
    created_at       timestamptz   NOT NULL DEFAULT now(),
    updated_at       timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_reservations PRIMARY KEY (id),
    CONSTRAINT uq_reservations_code UNIQUE (code),
    CONSTRAINT fk_reservations_property_id_properties FOREIGN KEY (property_id) REFERENCES properties (id),
    CONSTRAINT fk_reservations_channel_id_booking_channels FOREIGN KEY (channel_id) REFERENCES booking_channels (id),
    CONSTRAINT fk_reservations_primary_guest_id_guests FOREIGN KEY (primary_guest_id) REFERENCES guests (id),
    CONSTRAINT fk_reservations_created_by_users FOREIGN KEY (created_by) REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT ck_reservations_status CHECK (status IN ('pending', 'confirmed', 'checked_in', 'checked_out', 'cancelled', 'no_show')),
    CONSTRAINT ck_reservations_dates CHECK (check_out_date > check_in_date),
    CONSTRAINT ck_reservations_people CHECK (adults BETWEEN 0 AND 50 AND children BETWEEN 0 AND 50),
    CONSTRAINT ck_reservations_amounts CHECK (total_amount >= 0 AND deposit_amount >= 0),
    CONSTRAINT ck_reservations_deposit_status CHECK (deposit_status IN ('none', 'pending', 'received', 'returned',
        'retained_partial', 'retained_full')),
    CONSTRAINT ck_reservations_source CHECK (source IN ('manual', 'ical', 'api')),
    -- La base rechaza dos estancias vivas de la misma propiedad que se cruzan
    -- (la salida es exclusiva: entrar el dia que otro sale no se cruza).
    CONSTRAINT ex_reservations_overlap EXCLUDE USING gist (
        property_id WITH =, daterange(check_in_date, check_out_date) WITH &&
    ) WHERE (status NOT IN ('cancelled', 'no_show'))
);
CREATE INDEX ix_reservations_property_id_check_in_date ON reservations (property_id, check_in_date);
CREATE INDEX ix_reservations_check_out_date ON reservations (check_out_date);
CREATE INDEX ix_reservations_status ON reservations (status);
CREATE INDEX ix_reservations_primary_guest_id ON reservations (primary_guest_id);
CREATE UNIQUE INDEX uq_reservations_property_id_external_ref ON reservations (property_id, external_ref)
    WHERE external_ref IS NOT NULL;

CREATE TABLE reservation_guests (
    reservation_id uuid        NOT NULL,
    guest_id       uuid        NOT NULL,
    is_primary     boolean     NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_reservation_guests PRIMARY KEY (reservation_id, guest_id),
    CONSTRAINT fk_reservation_guests_reservation_id_reservations FOREIGN KEY (reservation_id)
        REFERENCES reservations (id) ON DELETE CASCADE,
    CONSTRAINT fk_reservation_guests_guest_id_guests FOREIGN KEY (guest_id) REFERENCES guests (id)
);
CREATE UNIQUE INDEX uq_reservation_guests_primary ON reservation_guests (reservation_id) WHERE is_primary;
CREATE INDEX ix_reservation_guests_guest_id ON reservation_guests (guest_id);

-- Linea de tiempo de la reserva (inmutable).
CREATE TABLE reservation_events (
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    reservation_id uuid        NOT NULL,
    type           varchar(32) NOT NULL,
    actor_user_id  uuid,
    actor_guest_id uuid,
    data           jsonb       NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_reservation_events PRIMARY KEY (id),
    CONSTRAINT fk_reservation_events_reservation_id_reservations FOREIGN KEY (reservation_id)
        REFERENCES reservations (id) ON DELETE CASCADE,
    CONSTRAINT fk_reservation_events_actor_user_id_users FOREIGN KEY (actor_user_id)
        REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT fk_reservation_events_actor_guest_id_guests FOREIGN KEY (actor_guest_id)
        REFERENCES guests (id) ON DELETE SET NULL
);
CREATE INDEX ix_reservation_events_reservation_id_created_at ON reservation_events (reservation_id, created_at);

-- Movimientos del deposito (inmutables): deposit_status se recalcula en el service.
CREATE TABLE deposit_transactions (
    id             uuid          NOT NULL DEFAULT gen_random_uuid(),
    reservation_id uuid          NOT NULL,
    type           varchar(16)   NOT NULL,
    amount         numeric(12,2) NOT NULL,
    method         varchar(16)   NOT NULL,
    reference      varchar(128),
    notes          text,
    registered_by  uuid,
    created_at     timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_deposit_transactions PRIMARY KEY (id),
    CONSTRAINT fk_deposit_transactions_reservation_id_reservations FOREIGN KEY (reservation_id)
        REFERENCES reservations (id) ON DELETE CASCADE,
    CONSTRAINT fk_deposit_transactions_registered_by_users FOREIGN KEY (registered_by)
        REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT ck_deposit_transactions_type CHECK (type IN ('received', 'returned', 'retained')),
    CONSTRAINT ck_deposit_transactions_method CHECK (method IN ('cash', 'transfer', 'card', 'other')),
    CONSTRAINT ck_deposit_transactions_amount CHECK (amount > 0)
);
CREATE INDEX ix_deposit_transactions_reservation_id ON deposit_transactions (reservation_id, created_at);

CREATE TABLE inspections (
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    reservation_id uuid        NOT NULL,
    property_id    uuid        NOT NULL,
    kind           varchar(16) NOT NULL,
    performed_by   uuid,
    performed_at   timestamptz NOT NULL DEFAULT now(),
    task_id        uuid,
    notes          text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_inspections PRIMARY KEY (id),
    CONSTRAINT uq_inspections_reservation_id_kind UNIQUE (reservation_id, kind),
    CONSTRAINT fk_inspections_reservation_id_reservations FOREIGN KEY (reservation_id)
        REFERENCES reservations (id) ON DELETE CASCADE,
    CONSTRAINT fk_inspections_property_id_properties FOREIGN KEY (property_id) REFERENCES properties (id),
    CONSTRAINT fk_inspections_performed_by_users FOREIGN KEY (performed_by) REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT ck_inspections_kind CHECK (kind IN ('pre_arrival', 'post_departure'))
);

CREATE TABLE inspection_findings (
    id                uuid          NOT NULL DEFAULT gen_random_uuid(),
    inspection_id     uuid          NOT NULL,
    area              varchar(64)   NOT NULL,
    description       text          NOT NULL,
    severity          varchar(8)    NOT NULL,
    estimated_cost    numeric(12,2),
    charge_to_deposit boolean       NOT NULL DEFAULT false,
    ticket_id         uuid,
    created_by        uuid,
    created_at        timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_inspection_findings PRIMARY KEY (id),
    CONSTRAINT fk_inspection_findings_inspection_id_inspections FOREIGN KEY (inspection_id)
        REFERENCES inspections (id) ON DELETE CASCADE,
    CONSTRAINT fk_inspection_findings_ticket_id_maintenance_tickets FOREIGN KEY (ticket_id)
        REFERENCES maintenance_tickets (id) ON DELETE SET NULL,
    CONSTRAINT fk_inspection_findings_created_by_users FOREIGN KEY (created_by)
        REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT ck_inspection_findings_severity CHECK (severity IN ('low', 'medium', 'high')),
    CONSTRAINT ck_inspection_findings_cost CHECK (estimated_cost >= 0)
);
CREATE INDEX ix_inspection_findings_inspection_id ON inspection_findings (inspection_id);

ALTER TABLE maintenance_tickets ADD CONSTRAINT fk_maintenance_tickets_reported_by_guest_id_guests
    FOREIGN KEY (reported_by_guest_id) REFERENCES guests (id) ON DELETE SET NULL;

DO $$
BEGIN
    IF to_regclass('tasks') IS NOT NULL THEN
        ALTER TABLE inspections ADD CONSTRAINT fk_inspections_task_id_tasks
            FOREIGN KEY (task_id) REFERENCES tasks (id) ON DELETE SET NULL;
        IF EXISTS (SELECT 1 FROM information_schema.columns
                   WHERE table_schema = current_schema() AND table_name = 'tasks' AND column_name = 'reservation_id') THEN
            ALTER TABLE tasks ADD CONSTRAINT fk_tasks_reservation_id_reservations
                FOREIGN KEY (reservation_id) REFERENCES reservations (id) ON DELETE SET NULL;
        END IF;
    END IF;
END $$;

-- migrate:down
DO $$
BEGIN
    IF to_regclass('tasks') IS NOT NULL THEN
        ALTER TABLE tasks DROP CONSTRAINT IF EXISTS fk_tasks_reservation_id_reservations;
    END IF;
END $$;
ALTER TABLE maintenance_tickets DROP CONSTRAINT fk_maintenance_tickets_reported_by_guest_id_guests;
DROP TABLE inspection_findings;
DROP TABLE inspections;
DROP TABLE deposit_transactions;
DROP TABLE reservation_events;
DROP TABLE reservation_guests;
DROP TABLE reservations;
DROP TABLE guests;
DROP TABLE property_ical_feeds;
DROP TABLE booking_channels;
