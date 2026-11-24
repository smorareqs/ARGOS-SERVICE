-- Nomina y disciplina (APP/docs/plans/fase05.md §2, parte de Persona A):
-- configuracion de nomina, periodos, una linea por colaborador y periodo, y
-- acciones disciplinarias con acuse. Asistencia, metricas, metas e incentivos
-- son de 0009_attendance_metrics (Persona B); el calculo los lee si existen.
--
-- Diferencias con el plan:
-- * `payroll_settings.igss_employer_rate` (cuota patronal, solo informativa en
--   el Excel) e `incentive_bonus_monthly` (bonificacion incentivo del Decreto
--   37-2001, Q250 al mes por defecto, sin IGSS).
-- * `payroll_entries` guarda tambien codigo, nombre y puesto del colaborador
--   (la boleta de un periodo cerrado no cambia si luego cambian), de donde
--   salieron dias y horas (`attendance_source`), la bonificacion incentivo, el
--   detalle del calculo de Bono 14 y aguinaldo (`bonus_detail`), los campos
--   corregidos a mano (`manual_fields`) y el historial de cambios (`history`).
--   Los ajustes libres del plan (`adjustments`) se cubren con otros devengos,
--   otras deducciones y la nota de la linea.

-- migrate:up
CREATE TABLE payroll_settings (
    id                      smallint      NOT NULL DEFAULT 1,
    igss_employee_rate      numeric(5,4)  NOT NULL DEFAULT 0.0483,
    igss_employer_rate      numeric(5,4)  NOT NULL DEFAULT 0.1267,
    incentive_bonus_monthly numeric(12,2) NOT NULL DEFAULT 250,
    bonus14_month           smallint      NOT NULL DEFAULT 7,
    aguinaldo_month         smallint      NOT NULL DEFAULT 12,
    monthly_days            smallint      NOT NULL DEFAULT 30,
    daily_hours             smallint      NOT NULL DEFAULT 8,
    min_minutes_for_day     smallint      NOT NULL DEFAULT 240,
    updated_by              uuid,
    updated_at              timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_payroll_settings PRIMARY KEY (id),
    CONSTRAINT ck_payroll_settings_single CHECK (id = 1),
    CONSTRAINT ck_payroll_settings_rates CHECK (igss_employee_rate BETWEEN 0 AND 0.5 AND igss_employer_rate BETWEEN 0 AND 0.5),
    CONSTRAINT ck_payroll_settings_bonus CHECK (incentive_bonus_monthly >= 0),
    CONSTRAINT ck_payroll_settings_months CHECK (bonus14_month BETWEEN 1 AND 12 AND aguinaldo_month BETWEEN 1 AND 12),
    CONSTRAINT ck_payroll_settings_days CHECK (monthly_days BETWEEN 20 AND 31 AND daily_hours BETWEEN 1 AND 24),
    CONSTRAINT ck_payroll_settings_minutes CHECK (min_minutes_for_day BETWEEN 0 AND 1440),
    CONSTRAINT fk_payroll_settings_updated_by_users FOREIGN KEY (updated_by)
        REFERENCES shared.users (id) ON DELETE SET NULL
);
INSERT INTO payroll_settings DEFAULT VALUES;

CREATE TABLE payroll_periods (
    id             uuid         NOT NULL DEFAULT gen_random_uuid(),
    name           varchar(64)  NOT NULL,
    kind           varchar(16)  NOT NULL,
    period_start   date         NOT NULL,
    period_end     date         NOT NULL,
    status         varchar(16)  NOT NULL DEFAULT 'draft',
    computed_at    timestamptz,
    reviewed_at    timestamptz,
    closed_by      uuid,
    closed_at      timestamptz,
    export_file_id uuid,
    created_by     uuid,
    created_at     timestamptz  NOT NULL DEFAULT now(),
    updated_at     timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_payroll_periods PRIMARY KEY (id),
    CONSTRAINT ck_payroll_periods_kind CHECK (kind IN ('biweekly', 'monthly')),
    CONSTRAINT ck_payroll_periods_status CHECK (status IN ('draft', 'review', 'closed')),
    CONSTRAINT ck_payroll_periods_range CHECK (period_end >= period_start AND period_end - period_start < 62),
    CONSTRAINT ex_payroll_periods_overlap EXCLUDE USING gist (kind WITH =, daterange(period_start, period_end, '[]') WITH &&),
    CONSTRAINT fk_payroll_periods_closed_by_users FOREIGN KEY (closed_by) REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT fk_payroll_periods_created_by_users FOREIGN KEY (created_by) REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT fk_payroll_periods_export_file_id_files FOREIGN KEY (export_file_id) REFERENCES files (id) ON DELETE SET NULL
);
CREATE INDEX ix_payroll_periods_period_start ON payroll_periods (period_start DESC);

CREATE TABLE payroll_entries (
    id                     uuid          NOT NULL DEFAULT gen_random_uuid(),
    period_id              uuid          NOT NULL,
    employee_id            uuid          NOT NULL,
    employee_code          varchar(32)   NOT NULL,
    full_name              varchar(160)  NOT NULL,
    position               varchar(64)   NOT NULL,
    pay_type               varchar(16)   NOT NULL,
    pay_rate               numeric(12,2) NOT NULL,
    on_payroll             boolean       NOT NULL,
    attendance_source      varchar(16)   NOT NULL DEFAULT 'schedule',
    days_worked            numeric(5,2)  NOT NULL DEFAULT 0,
    hours_worked           numeric(7,2)  NOT NULL DEFAULT 0,
    cleanings_count        integer       NOT NULL DEFAULT 0,
    base_amount            numeric(12,2) NOT NULL DEFAULT 0,
    overtime_amount        numeric(12,2) NOT NULL DEFAULT 0,
    incentives_amount      numeric(12,2) NOT NULL DEFAULT 0,
    other_earnings         numeric(12,2) NOT NULL DEFAULT 0,
    incentive_bonus_amount numeric(12,2) NOT NULL DEFAULT 0,
    bonus14_amount         numeric(12,2) NOT NULL DEFAULT 0,
    aguinaldo_amount       numeric(12,2) NOT NULL DEFAULT 0,
    igss_employee          numeric(12,2) NOT NULL DEFAULT 0,
    other_deductions       numeric(12,2) NOT NULL DEFAULT 0,
    gross_amount           numeric(12,2) NOT NULL DEFAULT 0,
    net_amount             numeric(12,2) NOT NULL DEFAULT 0,
    bonus_detail           jsonb,
    manual_fields          text[]        NOT NULL DEFAULT '{}',
    history                jsonb         NOT NULL DEFAULT '[]',
    notes                  text,
    payslip_file_id        uuid,
    created_at             timestamptz   NOT NULL DEFAULT now(),
    updated_at             timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_payroll_entries PRIMARY KEY (id),
    CONSTRAINT uq_payroll_entries_period_id_employee_id UNIQUE (period_id, employee_id),
    CONSTRAINT ck_payroll_entries_pay_type CHECK (pay_type IN ('fixed_monthly', 'per_day', 'per_hour', 'per_cleaning')),
    CONSTRAINT ck_payroll_entries_attendance_source CHECK (attendance_source IN ('attendance', 'schedule')),
    CONSTRAINT ck_payroll_entries_amounts CHECK (
        days_worked >= 0 AND hours_worked >= 0 AND cleanings_count >= 0 AND base_amount >= 0
        AND overtime_amount >= 0 AND incentives_amount >= 0 AND other_earnings >= 0 AND incentive_bonus_amount >= 0
        AND bonus14_amount >= 0 AND aguinaldo_amount >= 0 AND igss_employee >= 0 AND other_deductions >= 0),
    CONSTRAINT fk_payroll_entries_period_id_payroll_periods FOREIGN KEY (period_id)
        REFERENCES payroll_periods (id) ON DELETE CASCADE,
    CONSTRAINT fk_payroll_entries_employee_id_employees FOREIGN KEY (employee_id) REFERENCES employees (id),
    CONSTRAINT fk_payroll_entries_payslip_file_id_files FOREIGN KEY (payslip_file_id) REFERENCES files (id) ON DELETE SET NULL
);
CREATE INDEX ix_payroll_entries_employee_id ON payroll_entries (employee_id);

CREATE TABLE disciplinary_actions (
    id                   uuid        NOT NULL DEFAULT gen_random_uuid(),
    employee_id          uuid        NOT NULL,
    type                 varchar(16) NOT NULL,
    occurred_on          date        NOT NULL,
    description          text        NOT NULL,
    issued_by            uuid,
    suspension_from      date,
    suspension_to        date,
    acknowledged_at      timestamptz,
    acknowledgement_note text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_disciplinary_actions PRIMARY KEY (id),
    CONSTRAINT ck_disciplinary_actions_type CHECK (type IN ('verbal', 'written', 'suspension', 'other')),
    CONSTRAINT ck_disciplinary_actions_suspension CHECK (
        (type = 'suspension' AND suspension_from IS NOT NULL AND suspension_to IS NOT NULL AND suspension_to >= suspension_from)
        OR (type <> 'suspension' AND suspension_from IS NULL AND suspension_to IS NULL)),
    CONSTRAINT fk_disciplinary_actions_employee_id_employees FOREIGN KEY (employee_id) REFERENCES employees (id),
    CONSTRAINT fk_disciplinary_actions_issued_by_users FOREIGN KEY (issued_by) REFERENCES shared.users (id) ON DELETE SET NULL
);
CREATE INDEX ix_disciplinary_actions_employee_id ON disciplinary_actions (employee_id, occurred_on DESC);
CREATE INDEX ix_disciplinary_actions_pending ON disciplinary_actions (created_at) WHERE acknowledged_at IS NULL;

-- migrate:down
DROP TABLE disciplinary_actions;
DROP TABLE payroll_entries;
DROP TABLE payroll_periods;
DROP TABLE payroll_settings;
