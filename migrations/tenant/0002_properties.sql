-- Datos maestros de la operacion (APP/docs/plans/fase02.md §2, parte de Persona A):
-- edificios, propiedades (acceso cifrado, propietarios y asignados), perfil
-- laboral con horario y plantillas de checklist. Las tareas y el inventario
-- (0003_tasks_inventory, Persona B) se aplican despues.
--
-- Diferencias con el plan: `zone` en edificios y propiedades (barrio o ciudad que
-- se muestra en listas y mapas), `cover_file_id` (foto de portada en S3) y
-- `access_info_updated_at`. El codigo es unico entre las propiedades no borradas.

-- migrate:up
CREATE TABLE buildings (
    id         uuid         NOT NULL DEFAULT gen_random_uuid(),
    name       varchar(128) NOT NULL,
    address    text,
    zone       varchar(128),
    lat        numeric(9,6),
    lng        numeric(9,6),
    notes      text,
    is_active  boolean      NOT NULL DEFAULT true,
    created_at timestamptz  NOT NULL DEFAULT now(),
    updated_at timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_buildings PRIMARY KEY (id),
    CONSTRAINT ck_buildings_lat CHECK (lat BETWEEN -90 AND 90),
    CONSTRAINT ck_buildings_lng CHECK (lng BETWEEN -180 AND 180)
);
CREATE UNIQUE INDEX uq_buildings_name ON buildings (lower(name));

CREATE TABLE checklist_templates (
    id          uuid         NOT NULL DEFAULT gen_random_uuid(),
    name        varchar(128) NOT NULL,
    description text,
    is_default  boolean      NOT NULL DEFAULT false,
    created_at  timestamptz  NOT NULL DEFAULT now(),
    updated_at  timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_checklist_templates PRIMARY KEY (id)
);
CREATE UNIQUE INDEX uq_checklist_templates_name ON checklist_templates (lower(name));
-- Una sola plantilla por defecto.
CREATE UNIQUE INDEX uq_checklist_templates_is_default ON checklist_templates (is_default) WHERE is_default;

CREATE TABLE checklist_template_items (
    id                    uuid         NOT NULL DEFAULT gen_random_uuid(),
    template_id           uuid         NOT NULL,
    section               varchar(64)  NOT NULL,
    position              integer      NOT NULL,
    text                  varchar(255) NOT NULL,
    requires_photo        boolean      NOT NULL DEFAULT false,
    requires_before_after boolean      NOT NULL DEFAULT false,
    created_at            timestamptz  NOT NULL DEFAULT now(),
    updated_at            timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_checklist_template_items PRIMARY KEY (id),
    CONSTRAINT fk_checklist_template_items_template_id_checklist_templates FOREIGN KEY (template_id)
        REFERENCES checklist_templates (id) ON DELETE CASCADE,
    CONSTRAINT uq_checklist_template_items_template_id UNIQUE (template_id, position)
);

CREATE TABLE properties (
    id                     uuid         NOT NULL DEFAULT gen_random_uuid(),
    building_id            uuid,
    code                   varchar(32)  NOT NULL,
    name                   varchar(128) NOT NULL,
    type                   varchar(16)  NOT NULL,
    address                text,
    zone                   varchar(128),
    lat                    numeric(9,6),
    lng                    numeric(9,6),
    bedrooms               smallint,
    bathrooms              smallint,
    max_guests             smallint,
    checklist_template_id  uuid,
    -- Cifrado con AES-256-GCM (security.FieldCipher): llaves, codigos, wifi.
    access_info            text,
    access_info_updated_at timestamptz,
    cover_file_id          uuid,
    notes                  text,
    is_active              boolean      NOT NULL DEFAULT true,
    deleted_at             timestamptz,
    created_at             timestamptz  NOT NULL DEFAULT now(),
    updated_at             timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_properties PRIMARY KEY (id),
    CONSTRAINT fk_properties_building_id_buildings FOREIGN KEY (building_id)
        REFERENCES buildings (id),
    CONSTRAINT fk_properties_checklist_template_id_checklist_templates FOREIGN KEY (checklist_template_id)
        REFERENCES checklist_templates (id) ON DELETE SET NULL,
    CONSTRAINT fk_properties_cover_file_id_files FOREIGN KEY (cover_file_id)
        REFERENCES files (id) ON DELETE SET NULL,
    CONSTRAINT ck_properties_type CHECK (type IN ('apartment', 'house', 'room', 'other')),
    CONSTRAINT ck_properties_lat CHECK (lat BETWEEN -90 AND 90),
    CONSTRAINT ck_properties_lng CHECK (lng BETWEEN -180 AND 180),
    CONSTRAINT ck_properties_rooms CHECK (
        (bedrooms IS NULL OR bedrooms >= 0) AND (bathrooms IS NULL OR bathrooms >= 0)
        AND (max_guests IS NULL OR max_guests >= 0))
);
CREATE UNIQUE INDEX uq_properties_code ON properties (upper(code)) WHERE deleted_at IS NULL;
CREATE INDEX ix_properties_building_id ON properties (building_id);
CREATE INDEX ix_properties_checklist_template_id ON properties (checklist_template_id);

-- Propietarios: alcance del rol owner.
CREATE TABLE property_owners (
    property_id uuid        NOT NULL,
    user_id     uuid        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_property_owners PRIMARY KEY (property_id, user_id),
    CONSTRAINT fk_property_owners_property_id_properties FOREIGN KEY (property_id)
        REFERENCES properties (id) ON DELETE CASCADE,
    CONSTRAINT fk_property_owners_user_id_users FOREIGN KEY (user_id)
        REFERENCES shared.users (id) ON DELETE CASCADE
);
CREATE INDEX ix_property_owners_user_id ON property_owners (user_id);

-- Asignados: alcance de limpieza y mantenimiento.
CREATE TABLE property_assignments (
    property_id uuid        NOT NULL,
    user_id     uuid        NOT NULL,
    assigned_by uuid,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_property_assignments PRIMARY KEY (property_id, user_id),
    CONSTRAINT fk_property_assignments_property_id_properties FOREIGN KEY (property_id)
        REFERENCES properties (id) ON DELETE CASCADE,
    CONSTRAINT fk_property_assignments_user_id_users FOREIGN KEY (user_id)
        REFERENCES shared.users (id) ON DELETE CASCADE,
    CONSTRAINT fk_property_assignments_assigned_by_users FOREIGN KEY (assigned_by)
        REFERENCES shared.users (id) ON DELETE SET NULL
);
CREATE INDEX ix_property_assignments_user_id ON property_assignments (user_id);

-- Perfil laboral (W-D2). Los datos de RRHH (asistencia, nomina) llegan en la fase 5.
CREATE TABLE employees (
    id                uuid          NOT NULL DEFAULT gen_random_uuid(),
    user_id           uuid          NOT NULL,
    employee_code     varchar(32)   NOT NULL,
    position          varchar(64)   NOT NULL,
    hire_date         date          NOT NULL,
    termination_date  date,
    document_type     varchar(16),
    document_number   varchar(32),
    igss_number       varchar(32),
    nit               varchar(16),
    pay_type          varchar(16)   NOT NULL,
    pay_rate          numeric(12,2) NOT NULL DEFAULT 0,
    on_payroll        boolean       NOT NULL DEFAULT false,
    emergency_contact jsonb,
    notes             text,
    created_at        timestamptz   NOT NULL DEFAULT now(),
    updated_at        timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_employees PRIMARY KEY (id),
    CONSTRAINT uq_employees_user_id UNIQUE (user_id),
    CONSTRAINT fk_employees_user_id_users FOREIGN KEY (user_id) REFERENCES shared.users (id),
    CONSTRAINT ck_employees_document_type CHECK (document_type IN ('dpi', 'passport')),
    CONSTRAINT ck_employees_pay_type CHECK (pay_type IN ('fixed_monthly', 'per_day', 'per_hour', 'per_cleaning')),
    CONSTRAINT ck_employees_pay_rate CHECK (pay_rate >= 0),
    CONSTRAINT ck_employees_termination_date CHECK (termination_date IS NULL OR termination_date >= hire_date)
);
CREATE UNIQUE INDEX uq_employees_employee_code ON employees (upper(employee_code));

-- Horario semanal: weekday 0 = domingo ... 6 = sabado (como EXTRACT(DOW)).
CREATE TABLE employee_schedules (
    id          uuid        NOT NULL DEFAULT gen_random_uuid(),
    employee_id uuid        NOT NULL,
    weekday     smallint    NOT NULL,
    starts_at   time        NOT NULL,
    ends_at     time        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_employee_schedules PRIMARY KEY (id),
    CONSTRAINT fk_employee_schedules_employee_id_employees FOREIGN KEY (employee_id)
        REFERENCES employees (id) ON DELETE CASCADE,
    CONSTRAINT ck_employee_schedules_weekday CHECK (weekday BETWEEN 0 AND 6),
    CONSTRAINT ck_employee_schedules_range CHECK (starts_at < ends_at)
);
CREATE INDEX ix_employee_schedules_employee_id ON employee_schedules (employee_id);

-- migrate:down
DROP TABLE employee_schedules;
DROP TABLE employees;
DROP TABLE property_assignments;
DROP TABLE property_owners;
DROP TABLE properties;
DROP TABLE checklist_template_items;
DROP TABLE checklist_templates;
DROP TABLE buildings;
