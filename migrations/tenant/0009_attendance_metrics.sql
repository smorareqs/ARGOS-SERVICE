-- Asistencia, metricas, metas e incentivos (APP/docs/plans/fase05.md §2,
-- parte de Persona B). La nomina (0008, Persona A) lee los marcajes cerrados
-- y los incentivos aprobados.
--
-- Diferencias con el plan:
-- * `attendance_records.autoclosed`: el job attendance_autoclose lo cerro a
--   las 16 h (el admin lo ve marcado y lo corrige con PATCH).
-- * `performance_snapshots.returned_count`: limpiezas devueltas por el
--   supervisor ese dia (la web lo muestra como "Devueltas").
-- * `goals.unit` y `goals.reward_text`: como se muestra la meta en la web
--   ("limpiezas", "%", y el premio cuando no es un monto).

-- migrate:up
CREATE TABLE attendance_records (
    id                uuid         NOT NULL DEFAULT gen_random_uuid(),
    employee_id       uuid         NOT NULL,
    clock_in_at       timestamptz  NOT NULL,
    clock_out_at      timestamptz,
    in_lat            numeric(9,6),
    in_lng            numeric(9,6),
    out_lat           numeric(9,6),
    out_lng           numeric(9,6),
    in_photo_file_id  uuid,
    out_photo_file_id uuid,
    source            varchar(8)   NOT NULL DEFAULT 'app',
    autoclosed        boolean      NOT NULL DEFAULT false,
    edited_by         uuid,
    edited_at         timestamptz,
    edit_reason       text,
    worked_minutes    integer,
    created_at        timestamptz  NOT NULL DEFAULT now(),
    updated_at        timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_attendance_records PRIMARY KEY (id),
    CONSTRAINT fk_attendance_records_employee_id_employees FOREIGN KEY (employee_id)
        REFERENCES employees (id) ON DELETE CASCADE,
    CONSTRAINT fk_attendance_records_in_photo_file_id_files FOREIGN KEY (in_photo_file_id)
        REFERENCES files (id) ON DELETE SET NULL,
    CONSTRAINT fk_attendance_records_out_photo_file_id_files FOREIGN KEY (out_photo_file_id)
        REFERENCES files (id) ON DELETE SET NULL,
    CONSTRAINT fk_attendance_records_edited_by_users FOREIGN KEY (edited_by)
        REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT ck_attendance_records_source CHECK (source IN ('app', 'manual')),
    CONSTRAINT ck_attendance_records_range CHECK (clock_out_at IS NULL OR clock_out_at >= clock_in_at),
    CONSTRAINT ck_attendance_records_minutes CHECK (worked_minutes IS NULL OR worked_minutes >= 0)
);
CREATE INDEX ix_attendance_records_employee ON attendance_records (employee_id, clock_in_at);
CREATE INDEX ix_attendance_records_clock_in ON attendance_records (clock_in_at);
CREATE UNIQUE INDEX uq_attendance_records_open ON attendance_records (employee_id) WHERE clock_out_at IS NULL;

CREATE TABLE performance_snapshots (
    id                   uuid         NOT NULL DEFAULT gen_random_uuid(),
    employee_id          uuid         NOT NULL,
    date                 date         NOT NULL,
    cleanings_done       integer      NOT NULL DEFAULT 0,
    avg_minutes          integer      NOT NULL DEFAULT 0,
    checklist_compliance numeric(5,2) NOT NULL DEFAULT 0,
    late_count           integer      NOT NULL DEFAULT 0,
    returned_count       integer      NOT NULL DEFAULT 0,
    tickets_reported     integer      NOT NULL DEFAULT 0,
    discrepancies_open   integer      NOT NULL DEFAULT 0,
    created_at           timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT pk_performance_snapshots PRIMARY KEY (id),
    CONSTRAINT fk_performance_snapshots_employee_id_employees FOREIGN KEY (employee_id)
        REFERENCES employees (id) ON DELETE CASCADE,
    CONSTRAINT uq_performance_snapshots_day UNIQUE (employee_id, date)
);
CREATE INDEX ix_performance_snapshots_date ON performance_snapshots (date);

CREATE TABLE goals (
    id               uuid          NOT NULL DEFAULT gen_random_uuid(),
    name             varchar(120)  NOT NULL,
    metric           varchar(32)   NOT NULL,
    target           numeric(12,2) NOT NULL,
    comparator       varchar(2)    NOT NULL DEFAULT '>=',
    period_kind      varchar(16)   NOT NULL DEFAULT 'monthly',
    applies_to       varchar(8)    NOT NULL DEFAULT 'all',
    applies_id       uuid,
    applies_role     varchar(32),
    incentive_amount numeric(12,2),
    reward_text      varchar(120),
    is_active        boolean       NOT NULL DEFAULT true,
    created_by       uuid,
    created_at       timestamptz   NOT NULL DEFAULT now(),
    updated_at       timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_goals PRIMARY KEY (id),
    CONSTRAINT fk_goals_created_by_users FOREIGN KEY (created_by) REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT ck_goals_metric CHECK (metric IN ('cleanings_done', 'checklist_compliance', 'on_time_rate',
        'late_count', 'returned_count', 'avg_minutes', 'tickets_reported')),
    CONSTRAINT ck_goals_comparator CHECK (comparator IN ('>=', '<=')),
    CONSTRAINT ck_goals_period_kind CHECK (period_kind IN ('weekly', 'monthly')),
    CONSTRAINT ck_goals_applies_to CHECK (applies_to IN ('all', 'role', 'user')),
    CONSTRAINT ck_goals_incentive_amount CHECK (incentive_amount IS NULL OR incentive_amount >= 0)
);

CREATE TABLE incentive_awards (
    id               uuid          NOT NULL DEFAULT gen_random_uuid(),
    goal_id          uuid          NOT NULL,
    employee_id      uuid          NOT NULL,
    period_start     date          NOT NULL,
    period_end       date          NOT NULL,
    achieved_value   numeric(12,2) NOT NULL,
    amount           numeric(12,2) NOT NULL DEFAULT 0,
    status           varchar(16)   NOT NULL DEFAULT 'pending',
    approved_by      uuid,
    decided_at       timestamptz,
    payroll_entry_id uuid,
    created_at       timestamptz   NOT NULL DEFAULT now(),
    updated_at       timestamptz   NOT NULL DEFAULT now(),
    CONSTRAINT pk_incentive_awards PRIMARY KEY (id),
    CONSTRAINT fk_incentive_awards_goal_id_goals FOREIGN KEY (goal_id) REFERENCES goals (id) ON DELETE CASCADE,
    CONSTRAINT fk_incentive_awards_employee_id_employees FOREIGN KEY (employee_id)
        REFERENCES employees (id) ON DELETE CASCADE,
    CONSTRAINT fk_incentive_awards_approved_by_users FOREIGN KEY (approved_by)
        REFERENCES shared.users (id) ON DELETE SET NULL,
    CONSTRAINT fk_incentive_awards_payroll_entry_id_payroll_entries FOREIGN KEY (payroll_entry_id)
        REFERENCES payroll_entries (id) ON DELETE SET NULL,
    CONSTRAINT ck_incentive_awards_status CHECK (status IN ('pending', 'approved', 'rejected', 'paid')),
    CONSTRAINT ck_incentive_awards_period CHECK (period_start <= period_end),
    CONSTRAINT uq_incentive_awards_slot UNIQUE (goal_id, employee_id, period_start)
);
CREATE INDEX ix_incentive_awards_status ON incentive_awards (status, period_end);

-- migrate:down
DROP TABLE incentive_awards;
DROP TABLE goals;
DROP TABLE performance_snapshots;
DROP TABLE attendance_records;
