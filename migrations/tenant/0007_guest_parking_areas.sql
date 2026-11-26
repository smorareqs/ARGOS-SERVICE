-- Huespedes en la app, alarmas de seguridad, parqueos y areas comunes
-- (APP/docs/plans/fase04.md §2, parte de Persona B).
--
-- Diferencias con el plan (para seguir el contrato que ya usa la web):
-- * `parking_spots` suma `level`, `kind` (guest|visitor|staff), `covered` e
--   `is_blocked`. Las fechas de `parking_assignments` son como las de la
--   reserva: la salida es exclusiva (`daterange '[)'`).
-- * `shared_areas.capacity` son personas a la vez (no reservas) y cada reserva
--   lleva `people`; `advance_hours` es la anticipacion minima y `photo` la
--   ilustracion de la tarjeta. `shared_area_bookings.guest_name` guarda el
--   nombre tal como se reservo.
-- * `security_alarms.fired_at`: cuando sono (la alerta de "sin atender" se
--   cuenta desde ahi) y `escalated_at`: cuando se levanto esa alerta. Dos alarmas de salida por reserva: la previa y la de la
--   hora exacta (UNIQUE por reserva, tipo y hora).

-- migrate:up
CREATE TABLE guest_access_tokens (
    id             uuid         NOT NULL DEFAULT gen_random_uuid(),
    reservation_id uuid         NOT NULL,
    guest_id       uuid         NOT NULL,
    token_hash     char(64)     NOT NULL,
    kind           varchar(8)   NOT NULL,
    expires_at     timestamptz  NOT NULL,
    last_used_at   timestamptz,
    fcm_token      varchar(512),
    revoked_at     timestamptz,
    created_at     timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_guest_access_tokens PRIMARY KEY (id),
    CONSTRAINT uq_guest_access_tokens_token_hash UNIQUE (token_hash),
    CONSTRAINT ck_guest_access_tokens_kind CHECK (kind IN ('magic', 'code')),
    CONSTRAINT fk_guest_access_tokens_reservation_id_reservations FOREIGN KEY (reservation_id)
        REFERENCES reservations (id) ON DELETE CASCADE,
    CONSTRAINT fk_guest_access_tokens_guest_id_guests FOREIGN KEY (guest_id)
        REFERENCES guests (id) ON DELETE CASCADE
);
CREATE INDEX ix_guest_access_tokens_reservation ON guest_access_tokens (reservation_id);

CREATE TABLE security_alarms (
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    reservation_id uuid        NOT NULL,
    kind           varchar(16) NOT NULL,
    fire_at        timestamptz NOT NULL,
    status         varchar(16) NOT NULL DEFAULT 'scheduled',
    fired_at       timestamptz,
    escalated_at   timestamptz,
    attended_by    uuid,
    attended_at    timestamptz,
    notes          text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_security_alarms PRIMARY KEY (id),
    CONSTRAINT ck_security_alarms_kind CHECK (kind IN ('checkout', 'checkin')),
    CONSTRAINT ck_security_alarms_status CHECK (status IN ('scheduled', 'fired', 'attended', 'cancelled')),
    CONSTRAINT fk_security_alarms_reservation_id_reservations FOREIGN KEY (reservation_id)
        REFERENCES reservations (id) ON DELETE CASCADE,
    CONSTRAINT fk_security_alarms_attended_by_users FOREIGN KEY (attended_by)
        REFERENCES shared.users (id) ON DELETE SET NULL
);
CREATE UNIQUE INDEX uq_security_alarms_slot ON security_alarms (reservation_id, kind, fire_at)
    WHERE status <> 'cancelled';
CREATE INDEX ix_security_alarms_due ON security_alarms (status, fire_at);

CREATE TABLE parking_spots (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    building_id uuid,
    property_id uuid,
    code        varchar(16) NOT NULL,
    level       varchar(64) NOT NULL DEFAULT '',
    kind        varchar(16) NOT NULL DEFAULT 'guest',
    covered     boolean     NOT NULL DEFAULT false,
    is_blocked  boolean     NOT NULL DEFAULT false,
    is_active   boolean     NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_parking_spots PRIMARY KEY (id),
    CONSTRAINT ck_parking_spots_place CHECK (building_id IS NOT NULL OR property_id IS NOT NULL),
    CONSTRAINT ck_parking_spots_kind CHECK (kind IN ('guest', 'visitor', 'staff')),
    CONSTRAINT fk_parking_spots_building_id_buildings FOREIGN KEY (building_id) REFERENCES buildings (id),
    CONSTRAINT fk_parking_spots_property_id_properties FOREIGN KEY (property_id) REFERENCES properties (id)
);
CREATE UNIQUE INDEX uq_parking_spots_code ON parking_spots (coalesce(building_id, property_id), lower(code));

CREATE TABLE parking_assignments (
    id                  uuid         NOT NULL DEFAULT gen_random_uuid(),
    spot_id             uuid         NOT NULL,
    reservation_id      uuid         NOT NULL,
    vehicle_plate       varchar(16)  NOT NULL,
    vehicle_description varchar(128),
    from_date           date         NOT NULL,
    to_date             date         NOT NULL,
    created_by          uuid,
    created_at          timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_parking_assignments PRIMARY KEY (id),
    CONSTRAINT ck_parking_assignments_dates CHECK (to_date > from_date),
    CONSTRAINT fk_parking_assignments_spot_id_parking_spots FOREIGN KEY (spot_id)
        REFERENCES parking_spots (id) ON DELETE CASCADE,
    CONSTRAINT fk_parking_assignments_reservation_id_reservations FOREIGN KEY (reservation_id)
        REFERENCES reservations (id) ON DELETE CASCADE,
    CONSTRAINT fk_parking_assignments_created_by_users FOREIGN KEY (created_by)
        REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT ex_parking_assignments_overlap EXCLUDE USING gist
        (spot_id WITH =, daterange(from_date, to_date, '[)') WITH &&)
);
CREATE INDEX ix_parking_assignments_reservation ON parking_assignments (reservation_id);
CREATE INDEX ix_parking_assignments_plate ON parking_assignments (vehicle_plate);

CREATE TABLE shared_areas (
    id                          uuid         NOT NULL DEFAULT gen_random_uuid(),
    building_id                 uuid,
    property_id                 uuid,
    name                        varchar(128) NOT NULL,
    description                 text,
    capacity                    smallint     NOT NULL DEFAULT 10,
    slot_minutes                smallint     NOT NULL DEFAULT 60,
    opens_at                    time         NOT NULL DEFAULT '08:00',
    closes_at                   time         NOT NULL DEFAULT '20:00',
    max_per_reservation_per_day smallint     NOT NULL DEFAULT 1,
    advance_hours               smallint     NOT NULL DEFAULT 2,
    advance_days                smallint     NOT NULL DEFAULT 7,
    rules                       text,
    photo                       varchar(16)  NOT NULL DEFAULT 'brightRoom',
    is_active                   boolean      NOT NULL DEFAULT true,
    created_at                  timestamptz  NOT NULL DEFAULT now(),
    updated_at                  timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_shared_areas PRIMARY KEY (id),
    CONSTRAINT ck_shared_areas_place CHECK (building_id IS NOT NULL OR property_id IS NOT NULL),
    CONSTRAINT ck_shared_areas_hours CHECK (closes_at > opens_at),
    CONSTRAINT ck_shared_areas_capacity CHECK (capacity > 0),
    CONSTRAINT ck_shared_areas_slot CHECK (slot_minutes BETWEEN 15 AND 720),
    CONSTRAINT ck_shared_areas_photo CHECK (photo IN ('beachPool', 'brightRoom', 'lakeRoom', 'antiguaArch')),
    CONSTRAINT fk_shared_areas_building_id_buildings FOREIGN KEY (building_id) REFERENCES buildings (id),
    CONSTRAINT fk_shared_areas_property_id_properties FOREIGN KEY (property_id) REFERENCES properties (id)
);

CREATE TABLE shared_area_bookings (
    id                uuid         NOT NULL DEFAULT gen_random_uuid(),
    area_id           uuid         NOT NULL,
    reservation_id    uuid,
    guest_id          uuid,
    guest_name        varchar(160) NOT NULL,
    booked_by_user_id uuid,
    people            smallint     NOT NULL DEFAULT 1,
    starts_at         timestamptz  NOT NULL,
    ends_at           timestamptz  NOT NULL,
    status            varchar(16)  NOT NULL DEFAULT 'confirmed',
    cancelled_at      timestamptz,
    cancel_reason     text,
    created_at        timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_shared_area_bookings PRIMARY KEY (id),
    CONSTRAINT ck_shared_area_bookings_status CHECK (status IN ('confirmed', 'cancelled')),
    CONSTRAINT ck_shared_area_bookings_people CHECK (people > 0),
    CONSTRAINT ck_shared_area_bookings_times CHECK (ends_at > starts_at),
    CONSTRAINT fk_shared_area_bookings_area_id_shared_areas FOREIGN KEY (area_id)
        REFERENCES shared_areas (id) ON DELETE CASCADE,
    CONSTRAINT fk_shared_area_bookings_reservation_id_reservations FOREIGN KEY (reservation_id)
        REFERENCES reservations (id) ON DELETE SET NULL,
    CONSTRAINT fk_shared_area_bookings_guest_id_guests FOREIGN KEY (guest_id)
        REFERENCES guests (id) ON DELETE SET NULL,
    CONSTRAINT fk_shared_area_bookings_booked_by_user_id_users FOREIGN KEY (booked_by_user_id)
        REFERENCES shared.users (id) ON DELETE SET NULL
);
CREATE INDEX ix_shared_area_bookings_area ON shared_area_bookings (area_id, starts_at);
CREATE INDEX ix_shared_area_bookings_reservation ON shared_area_bookings (reservation_id);

-- migrate:down
DROP TABLE shared_area_bookings;
DROP TABLE shared_areas;
DROP TABLE parking_assignments;
DROP TABLE parking_spots;
DROP TABLE security_alarms;
DROP TABLE guest_access_tokens;
