-- Chat interno y con huespedes, y cerraduras inteligentes (APP/docs/plans/
-- fase06.md §2 y §4.2, parte de Persona B).
--
-- Diferencias con el plan:
-- * `conversations.archived_at`: cuando se archivo (24 h despues de la salida
--   del huesped, job chat_archive).
-- * `uq_conversations_reservation`: una sola conversacion por reserva.
-- * `lock_access_codes.code_hint`: los dos ultimos digitos para la web (el
--   codigo completo va cifrado) y `attempts` para los reintentos del job.

-- migrate:up
CREATE TABLE conversations (
    id              uuid         NOT NULL DEFAULT gen_random_uuid(),
    kind            varchar(8)   NOT NULL,
    title           varchar(128),
    reservation_id  uuid,
    created_by      uuid,
    last_message_at timestamptz,
    is_archived     boolean      NOT NULL DEFAULT false,
    archived_at     timestamptz,
    created_at      timestamptz  NOT NULL DEFAULT now(),
    updated_at      timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_conversations PRIMARY KEY (id),
    CONSTRAINT ck_conversations_kind CHECK (kind IN ('internal', 'guest')),
    CONSTRAINT ck_conversations_guest_reservation CHECK (kind <> 'guest' OR reservation_id IS NOT NULL),
    CONSTRAINT fk_conversations_reservation_id_reservations FOREIGN KEY (reservation_id)
        REFERENCES reservations (id) ON DELETE CASCADE,
    CONSTRAINT fk_conversations_created_by_users FOREIGN KEY (created_by)
        REFERENCES shared.users (id) ON DELETE SET NULL
);
CREATE UNIQUE INDEX uq_conversations_reservation ON conversations (reservation_id) WHERE kind = 'guest';
CREATE INDEX ix_conversations_last_message_at ON conversations (last_message_at DESC NULLS LAST);

CREATE TABLE conversation_participants (
    id               uuid         NOT NULL DEFAULT gen_random_uuid(),
    conversation_id  uuid         NOT NULL,
    participant_type varchar(5)   NOT NULL,
    user_id          uuid,
    guest_id         uuid,
    joined_at        timestamptz  NOT NULL DEFAULT now(),
    last_read_at     timestamptz,
    muted            boolean      NOT NULL DEFAULT false,
    CONSTRAINT pk_conversation_participants PRIMARY KEY (id),
    CONSTRAINT ck_conversation_participants_type CHECK (
        (participant_type = 'user' AND user_id IS NOT NULL AND guest_id IS NULL)
        OR (participant_type = 'guest' AND guest_id IS NOT NULL AND user_id IS NULL)),
    CONSTRAINT fk_conversation_participants_conversation_id_conversations FOREIGN KEY (conversation_id)
        REFERENCES conversations (id) ON DELETE CASCADE,
    CONSTRAINT fk_conversation_participants_user_id_users FOREIGN KEY (user_id)
        REFERENCES shared.users (id) ON DELETE CASCADE,
    CONSTRAINT fk_conversation_participants_guest_id_guests FOREIGN KEY (guest_id)
        REFERENCES guests (id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX uq_conversation_participants_user ON conversation_participants (conversation_id, user_id)
    WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX uq_conversation_participants_guest ON conversation_participants (conversation_id, guest_id)
    WHERE guest_id IS NOT NULL;
CREATE INDEX ix_conversation_participants_user_id ON conversation_participants (user_id);

CREATE TABLE messages (
    id              uuid         NOT NULL DEFAULT gen_random_uuid(),
    conversation_id uuid         NOT NULL,
    sender_type     varchar(8)   NOT NULL,
    sender_user_id  uuid,
    sender_guest_id uuid,
    body            text,
    file_id         uuid,
    created_at      timestamptz  NOT NULL DEFAULT now(),
    deleted_at      timestamptz,
    CONSTRAINT pk_messages PRIMARY KEY (id),
    CONSTRAINT ck_messages_sender_type CHECK (sender_type IN ('user', 'guest', 'system')),
    CONSTRAINT ck_messages_content CHECK (body IS NOT NULL OR file_id IS NOT NULL),
    CONSTRAINT fk_messages_conversation_id_conversations FOREIGN KEY (conversation_id)
        REFERENCES conversations (id) ON DELETE CASCADE,
    CONSTRAINT fk_messages_sender_user_id_users FOREIGN KEY (sender_user_id)
        REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT fk_messages_sender_guest_id_guests FOREIGN KEY (sender_guest_id)
        REFERENCES guests (id) ON DELETE SET NULL,
    CONSTRAINT fk_messages_file_id_files FOREIGN KEY (file_id)
        REFERENCES files (id) ON DELETE SET NULL
);
CREATE INDEX ix_messages_conversation_created ON messages (conversation_id, created_at);

CREATE TABLE lock_devices (
    id          uuid          NOT NULL DEFAULT gen_random_uuid(),
    property_id uuid          NOT NULL,
    provider    varchar(16)   NOT NULL DEFAULT 'nexxt',
    external_id varchar(128)  NOT NULL,
    name        varchar(128)  NOT NULL,
    is_active   boolean       NOT NULL DEFAULT true,
    credentials text,
    created_at  timestamptz   NOT NULL DEFAULT now(),
    updated_at  timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_lock_devices PRIMARY KEY (id),
    CONSTRAINT uq_lock_devices_provider_external UNIQUE (provider, external_id),
    CONSTRAINT fk_lock_devices_property_id_properties FOREIGN KEY (property_id)
        REFERENCES properties (id) ON DELETE CASCADE
);
CREATE INDEX ix_lock_devices_property_id ON lock_devices (property_id);

CREATE TABLE lock_access_codes (
    id             uuid          NOT NULL DEFAULT gen_random_uuid(),
    device_id      uuid          NOT NULL,
    reservation_id uuid          NOT NULL,
    code           text          NOT NULL,
    code_hint      varchar(4)    NOT NULL,
    valid_from     timestamptz   NOT NULL,
    valid_to       timestamptz   NOT NULL,
    status         varchar(16)   NOT NULL DEFAULT 'scheduled',
    provider_ref   varchar(128),
    last_error     text,
    attempts       smallint      NOT NULL DEFAULT 0,
    created_at     timestamptz   NOT NULL DEFAULT now(),
    updated_at     timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_lock_access_codes PRIMARY KEY (id),
    CONSTRAINT ck_lock_access_codes_status CHECK (status IN ('scheduled', 'active', 'revoked', 'failed')),
    CONSTRAINT ck_lock_access_codes_window CHECK (valid_to > valid_from),
    CONSTRAINT fk_lock_access_codes_device_id_lock_devices FOREIGN KEY (device_id)
        REFERENCES lock_devices (id) ON DELETE CASCADE,
    CONSTRAINT fk_lock_access_codes_reservation_id_reservations FOREIGN KEY (reservation_id)
        REFERENCES reservations (id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX uq_lock_access_codes_live ON lock_access_codes (device_id, reservation_id)
    WHERE status IN ('scheduled', 'active');
CREATE INDEX ix_lock_access_codes_reservation_id ON lock_access_codes (reservation_id);

-- migrate:down
DROP TABLE lock_access_codes;
DROP TABLE lock_devices;
DROP TABLE messages;
DROP TABLE conversation_participants;
DROP TABLE conversations;
