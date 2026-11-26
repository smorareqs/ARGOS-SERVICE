-- Notificaciones (APP/docs/plans/fase03.md §2 y §4, parte de Persona B).
--
-- Diferencias con el plan:
-- * No hay `recipient_guest_id`: los huespedes reciben correo (A) y, en la
--   fase 6, mensajes de chat; la bandeja es de usuarios.
-- * `data` lleva `route` (ruta de la web) y `app_route` (ruta de la app) mas
--   los ids del recurso.
-- * `notification_preferences` solo guarda lo que el usuario apago; sin fila
--   ambos canales estan activos.

-- migrate:up
CREATE TABLE notifications (
    id                uuid         NOT NULL DEFAULT gen_random_uuid(),
    recipient_user_id uuid         NOT NULL,
    type              varchar(48)  NOT NULL,
    title             varchar(160) NOT NULL,
    body              text         NOT NULL DEFAULT '',
    data              jsonb        NOT NULL DEFAULT '{}',
    channels          jsonb        NOT NULL DEFAULT '["inapp"]',
    sent_push_at      timestamptz,
    sent_email_at     timestamptz,
    read_at           timestamptz,
    created_at        timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_notifications PRIMARY KEY (id),
    CONSTRAINT fk_notifications_recipient_user_id_users FOREIGN KEY (recipient_user_id)
        REFERENCES shared.users (id) ON DELETE CASCADE
);
CREATE INDEX ix_notifications_recipient ON notifications (recipient_user_id, read_at, created_at DESC);

CREATE TABLE notification_preferences (
    user_id    uuid        NOT NULL,
    type       varchar(48) NOT NULL,
    push       boolean     NOT NULL DEFAULT true,
    email      boolean     NOT NULL DEFAULT true,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_notification_preferences PRIMARY KEY (user_id, type),
    CONSTRAINT fk_notification_preferences_user_id_users FOREIGN KEY (user_id)
        REFERENCES shared.users (id) ON DELETE CASCADE
);

-- migrate:down
DROP TABLE notification_preferences;
DROP TABLE notifications;
