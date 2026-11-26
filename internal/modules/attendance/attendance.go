// Package attendance es el marcaje de entrada y salida de los colaboradores
// (fase05.md §2-3, Persona B, app B15 y web W-J1).
//
// Cada marcaje pide GPS y acepta una foto opcional. Un colaborador tiene a lo
// sumo un marcaje abierto (indice parcial unico). El job attendance_autoclose
// cierra a las 16 h los que se quedaron sin salida y avisa a RRHH. Las
// correcciones del admin exigen motivo y van a la bitacora. La nomina
// (Persona A) suma los minutos de los marcajes cerrados.
package attendance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/notifications"
)

// JobAutoclose cierra los marcajes abiertos de mas de 16 h (cada 15 min).
const JobAutoclose = "attendance_autoclose"

// MaxOpen es lo mas que dura un marcaje abierto antes de cerrarse solo.
const MaxOpen = 16 * time.Hour

// Errores del modulo.
var (
	ErrNotFound      = apperr.New(apperr.KindNotFound, "El marcaje no existe.")
	ErrNoEmployee    = apperr.New(apperr.KindValidation, "Tu usuario no tiene ficha de colaborador. Pídele al administrador que la cree.")
	ErrEmployee      = apperr.New(apperr.KindNotFound, "El colaborador no existe.")
	ErrGPS           = apperr.New(apperr.KindValidation, "Activa la ubicación para marcar.")
	ErrPhoto         = apperr.New(apperr.KindValidation, "La foto no se subió bien. Tómala de nuevo.")
	ErrAlreadyIn     = apperr.New(apperr.KindAlreadyExists, "Ya marcaste tu entrada.")
	ErrNotIn         = apperr.New(apperr.KindAlreadyExists, "No tienes una entrada abierta. Marca tu entrada primero.")
	ErrRange         = apperr.New(apperr.KindValidation, "La salida debe ser después de la entrada y el turno no puede pasar de 24 horas.")
	ErrFuture        = apperr.New(apperr.KindValidation, "No se puede marcar una hora que todavía no pasa.")
	ErrReason        = apperr.New(apperr.KindValidation, "Escribe el motivo de la corrección.")
	ErrOverlap       = apperr.New(apperr.KindAlreadyExists, "Ese horario se cruza con otro marcaje del colaborador.")
	ErrOpenElsewhere = apperr.New(apperr.KindAlreadyExists, "El colaborador ya tiene otro marcaje abierto.")
	ErrMonth         = apperr.New(apperr.KindValidation, "El mes va como AAAA-MM.")
)

// RecordRead es un marcaje.
type RecordRead struct {
	ID             uuid.UUID  `db:"id" json:"id" format:"uuid"`
	EmployeeID     uuid.UUID  `db:"employee_id" json:"employee_id" format:"uuid"`
	EmployeeName   string     `db:"employee_name" json:"employee_name"`
	Date           string     `db:"date" json:"date" format:"date" doc:"Dia (hora de la empresa) en que empezo."`
	ClockInAt      time.Time  `db:"clock_in_at" json:"clock_in_at"`
	ClockOutAt     *time.Time `db:"clock_out_at" json:"clock_out_at" nullable:"true"`
	InLat          *float64   `db:"in_lat" json:"in_lat" nullable:"true"`
	InLng          *float64   `db:"in_lng" json:"in_lng" nullable:"true"`
	OutLat         *float64   `db:"out_lat" json:"out_lat" nullable:"true"`
	OutLng         *float64   `db:"out_lng" json:"out_lng" nullable:"true"`
	InPhotoFileID  *uuid.UUID `db:"in_photo_file_id" json:"in_photo_file_id" format:"uuid" nullable:"true"`
	OutPhotoFileID *uuid.UUID `db:"out_photo_file_id" json:"out_photo_file_id" format:"uuid" nullable:"true"`
	Source         string     `db:"source" json:"source" enum:"app,manual"`
	Autoclosed     bool       `db:"autoclosed" json:"autoclosed" doc:"Lo cerro el sistema a las 16 h."`
	EditReason     *string    `db:"edit_reason" json:"edit_reason" nullable:"true"`
	EditedAt       *time.Time `db:"edited_at" json:"edited_at" nullable:"true"`
	WorkedMinutes  *int       `db:"worked_minutes" json:"worked_minutes" nullable:"true"`
	Late           bool       `db:"late" json:"late" doc:"Entro mas de 10 min despues de su horario."`
	OffSchedule    bool       `db:"off_schedule" json:"off_schedule" doc:"Tiene horario y ese dia no le tocaba."`
}

// RecordPage es la lista de marcajes (GET /attendance).
type RecordPage struct {
	Items      []RecordRead `json:"items"`
	NextCursor *string      `json:"next_cursor" nullable:"true"`
}

// Punch es el cuerpo de POST /attendance/clock-in y /clock-out.
type Punch struct {
	_           struct{}   `additionalProperties:"true"`
	Lat         *float64   `json:"lat" minimum:"-90" maximum:"90" required:"true"`
	Lng         *float64   `json:"lng" minimum:"-180" maximum:"180" required:"true"`
	Accuracy    *float64   `json:"accuracy,omitempty" minimum:"0" doc:"Metros, solo informativo."`
	PhotoFileID *uuid.UUID `json:"photo_file_id,omitempty" format:"uuid" doc:"Subida con modulo attendance y tipo selfie."`
}

// ManualCreate es el cuerpo de POST /attendance (correccion del admin). Si
// el colaborador tiene un marcaje abierto ese mismo dia, lo cierra con estas
// horas en vez de crear otro.
type ManualCreate struct {
	_          struct{}  `additionalProperties:"true"`
	EmployeeID uuid.UUID `json:"employee_id" format:"uuid"`
	ClockInAt  time.Time `json:"clock_in_at"`
	ClockOutAt time.Time `json:"clock_out_at"`
	EditReason string    `json:"edit_reason" maxLength:"500"`
}

// RecordUpdate es el cuerpo de PATCH /attendance/{id}.
type RecordUpdate struct {
	_          struct{}   `additionalProperties:"true"`
	ClockInAt  *time.Time `json:"clock_in_at,omitempty"`
	ClockOutAt *time.Time `json:"clock_out_at,omitempty"`
	EditReason string     `json:"edit_reason" maxLength:"500"`
}

// ScheduleSlot es un tramo del horario semanal (0 = domingo).
type ScheduleSlot struct {
	Weekday  int    `db:"weekday" json:"weekday" minimum:"0" maximum:"6"`
	StartsAt string `db:"starts_at" json:"starts_at" doc:"HH:MM"`
	EndsAt   string `db:"ends_at" json:"ends_at" doc:"HH:MM"`
}

// Month es GET /attendance/me: el marcaje abierto y el mes.
type Month struct {
	EmployeeID    uuid.UUID      `json:"employee_id" format:"uuid"`
	Month         string         `json:"month" doc:"AAAA-MM"`
	Open          *RecordRead    `json:"open" doc:"La entrada sin salida, si hay."`
	Items         []RecordRead   `json:"items" doc:"Marcajes del mes, los mas nuevos primero."`
	WorkedMinutes int            `json:"worked_minutes"`
	DaysWorked    int            `json:"days_worked"`
	LateCount     int            `json:"late_count"`
	Schedule      []ScheduleSlot `json:"schedule"`
}

// Service contiene los casos de uso de asistencia.
type Service struct {
	db    *database.DB
	notif *notifications.Service
	log   *slog.Logger
	now   func() time.Time
}

// NewService crea el servicio.
func NewService(db *database.DB, notif *notifications.Service, log *slog.Logger) *Service {
	return &Service{db: db, notif: notif, log: log, now: time.Now}
}

// SetClock cambia el reloj (pruebas).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// DB es la base del servicio (semilla).
func (s *Service) DB() *database.DB { return s.db }

const tenantTZ = `coalesce((SELECT timezone FROM shared.tenants WHERE schema_name = current_schema()), 'America/Guatemala')`

// recordSelect lee el marcaje con el dia y las banderas del horario.
const recordSelect = `
	WITH tz AS (SELECT ` + tenantTZ + ` AS name)
	SELECT a.id, a.employee_id, coalesce(u.full_name, e.employee_code) AS employee_name,
	       to_char((a.clock_in_at AT TIME ZONE tz.name)::date, 'YYYY-MM-DD') AS date,
	       a.clock_in_at, a.clock_out_at, a.in_lat::float8 AS in_lat, a.in_lng::float8 AS in_lng,
	       a.out_lat::float8 AS out_lat, a.out_lng::float8 AS out_lng, a.in_photo_file_id, a.out_photo_file_id,
	       a.source, a.autoclosed, a.edit_reason, a.edited_at, a.worked_minutes,
	       coalesce((SELECT (a.clock_in_at AT TIME ZONE tz.name)::time > min(sc.starts_at) + interval '10 minutes'
	                 FROM employee_schedules sc WHERE sc.employee_id = a.employee_id
	                   AND sc.weekday = extract(dow FROM a.clock_in_at AT TIME ZONE tz.name)), false) AS late,
	       (EXISTS (SELECT 1 FROM employee_schedules sc WHERE sc.employee_id = a.employee_id)
	        AND NOT EXISTS (SELECT 1 FROM employee_schedules sc WHERE sc.employee_id = a.employee_id
	                          AND sc.weekday = extract(dow FROM a.clock_in_at AT TIME ZONE tz.name))) AS off_schedule
	FROM attendance_records a
	CROSS JOIN tz
	JOIN employees e ON e.id = a.employee_id
	LEFT JOIN shared.users u ON u.id = e.user_id`

func collect(ctx context.Context, q database.Querier, where string, args ...any) ([]RecordRead, error) {
	rows, err := q.Query(ctx, recordSelect+" "+where, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[RecordRead])
}

func recordByID(ctx context.Context, q database.Querier, id uuid.UUID) (RecordRead, error) {
	list, err := collect(ctx, q, `WHERE a.id = $1`, id)
	if err != nil {
		return RecordRead{}, err
	}
	if len(list) == 0 {
		return RecordRead{}, ErrNotFound
	}
	return list[0], nil
}

// employeeOf devuelve la ficha activa del usuario.
func employeeOf(ctx context.Context, q database.Querier, userID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `SELECT id FROM employees WHERE user_id = $1 AND (termination_date IS NULL OR termination_date >= current_date)`, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNoEmployee
	}
	return id, err
}

// claimPhoto liga la foto al marcaje: subida por el mismo usuario, lista y sin entidad.
func claimPhoto(ctx context.Context, q database.Querier, actor authz.Actor, photo *uuid.UUID, recordID uuid.UUID) error {
	if photo == nil {
		return nil
	}
	tag, err := q.Exec(ctx, `
		UPDATE files SET entity_type = 'attendance_record', entity_id = $1, updated_at = now()
		WHERE id = $2 AND module = 'attendance' AND kind = 'selfie' AND status = 'ready'
		  AND uploaded_by_user_id = $3 AND entity_id IS NULL`, recordID, *photo, actor.UserID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrPhoto.WithField("body.photo_file_id")
	}
	return nil
}

func checkGPS(in Punch) error {
	if in.Lat == nil || in.Lng == nil || (*in.Lat == 0 && *in.Lng == 0) {
		return ErrGPS.WithField("body.lat")
	}
	return nil
}

func isOpenConflict(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "uq_attendance_records_open"
}

// ClockIn abre el marcaje del colaborador que tiene la sesion.
func (s *Service) ClockIn(ctx context.Context, actor authz.Actor, in Punch) (RecordRead, error) {
	if err := checkGPS(in); err != nil {
		return RecordRead{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (RecordRead, error) {
		emp, err := employeeOf(ctx, q, actor.UserID)
		if err != nil {
			return RecordRead{}, err
		}
		var id uuid.UUID
		err = q.QueryRow(ctx, `
			INSERT INTO attendance_records (employee_id, clock_in_at, in_lat, in_lng, source)
			VALUES ($1, $2, $3, $4, 'app') RETURNING id`, emp, s.now(), *in.Lat, *in.Lng).Scan(&id)
		if isOpenConflict(err) {
			return RecordRead{}, ErrAlreadyIn
		}
		if err != nil {
			return RecordRead{}, err
		}
		if err := claimPhoto(ctx, q, actor, in.PhotoFileID, id); err != nil {
			return RecordRead{}, err
		}
		if in.PhotoFileID != nil {
			if _, err := q.Exec(ctx, `UPDATE attendance_records SET in_photo_file_id = $2 WHERE id = $1`, id, *in.PhotoFileID); err != nil {
				return RecordRead{}, err
			}
		}
		return recordByID(ctx, q, id)
	})
}

// ClockOut cierra el marcaje abierto del colaborador que tiene la sesion.
func (s *Service) ClockOut(ctx context.Context, actor authz.Actor, in Punch) (RecordRead, error) {
	if err := checkGPS(in); err != nil {
		return RecordRead{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (RecordRead, error) {
		emp, err := employeeOf(ctx, q, actor.UserID)
		if err != nil {
			return RecordRead{}, err
		}
		var id uuid.UUID
		err = q.QueryRow(ctx, `
			UPDATE attendance_records
			SET clock_out_at = greatest($2::timestamptz, clock_in_at), out_lat = $3, out_lng = $4,
			    worked_minutes = floor(extract(epoch FROM greatest($2::timestamptz, clock_in_at) - clock_in_at) / 60)::int,
			    updated_at = now()
			WHERE employee_id = $1 AND clock_out_at IS NULL RETURNING id`, emp, s.now(), *in.Lat, *in.Lng).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return RecordRead{}, ErrNotIn
		}
		if err != nil {
			return RecordRead{}, err
		}
		if err := claimPhoto(ctx, q, actor, in.PhotoFileID, id); err != nil {
			return RecordRead{}, err
		}
		if in.PhotoFileID != nil {
			if _, err := q.Exec(ctx, `UPDATE attendance_records SET out_photo_file_id = $2 WHERE id = $1`, id, *in.PhotoFileID); err != nil {
				return RecordRead{}, err
			}
		}
		return recordByID(ctx, q, id)
	})
}

// monthRange convierte AAAA-MM (vacio = el mes actual de la empresa) en dias.
func monthRange(month string, now time.Time) (string, string, string, error) {
	if month == "" {
		month = now.Format("2006-01")
	}
	start, err := time.Parse("2006-01", month)
	if err != nil {
		return "", "", "", ErrMonth.WithField("month")
	}
	end := start.AddDate(0, 1, -1)
	return month, start.Format(time.DateOnly), end.Format(time.DateOnly), nil
}

// Me devuelve el estado y los marcajes del mes del colaborador.
func (s *Service) Me(ctx context.Context, actor authz.Actor, month string) (Month, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Month, error) {
		emp, err := employeeOf(ctx, q, actor.UserID)
		if err != nil {
			return Month{}, err
		}
		var tz string
		if err := q.QueryRow(ctx, `SELECT `+tenantTZ).Scan(&tz); err != nil {
			return Month{}, err
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			loc = time.UTC
		}
		m, from, to, err := monthRange(month, s.now().In(loc))
		if err != nil {
			return Month{}, err
		}
		out := Month{EmployeeID: emp, Month: m, Items: []RecordRead{}, Schedule: []ScheduleSlot{}}
		open, err := collect(ctx, q, `WHERE a.employee_id = $1 AND a.clock_out_at IS NULL`, emp)
		if err != nil {
			return Month{}, err
		}
		if len(open) > 0 {
			out.Open = &open[0]
		}
		items, err := collect(ctx, q, `WHERE a.employee_id = $1 AND (a.clock_in_at AT TIME ZONE tz.name)::date BETWEEN $2 AND $3
			ORDER BY a.clock_in_at DESC`, emp, from, to)
		if err != nil {
			return Month{}, err
		}
		days := map[string]bool{}
		for _, r := range items {
			if r.WorkedMinutes != nil {
				out.WorkedMinutes += *r.WorkedMinutes
			}
			if r.Late {
				out.LateCount++
			}
			days[r.Date] = true
		}
		out.Items = items
		out.DaysWorked = len(days)
		rows, err := q.Query(ctx, `SELECT weekday, to_char(starts_at, 'HH24:MI') AS starts_at, to_char(ends_at, 'HH24:MI') AS ends_at
			FROM employee_schedules WHERE employee_id = $1 ORDER BY weekday, starts_at`, emp)
		if err != nil {
			return Month{}, err
		}
		if out.Schedule, err = pgx.CollectRows(rows, pgx.RowToStructByName[ScheduleSlot]); err != nil {
			return Month{}, err
		}
		return out, nil
	})
}

// List devuelve los marcajes que empezaron entre esos dias (hora de la empresa).
func (s *Service) List(ctx context.Context, from, to string, employee *uuid.UUID, limit int) (RecordPage, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (RecordPage, error) {
		items, err := collect(ctx, q, `WHERE (a.clock_in_at AT TIME ZONE tz.name)::date BETWEEN $1 AND $2
			AND ($3::uuid IS NULL OR a.employee_id = $3) ORDER BY a.clock_in_at LIMIT $4`, from, to, employee, limit)
		if err != nil {
			return RecordPage{}, err
		}
		return RecordPage{Items: items}, nil
	})
}

func (s *Service) checkTimes(in time.Time, out *time.Time) error {
	limit := s.now().Add(5 * time.Minute)
	if in.After(limit) || (out != nil && out.After(limit)) {
		return ErrFuture.WithField("body.clock_out_at")
	}
	if out != nil && (out.Before(in) || out.Sub(in) > 24*time.Hour) {
		return ErrRange.WithField("body.clock_out_at")
	}
	return nil
}

func reason(r string) (string, error) {
	r = strings.TrimSpace(r)
	if len([]rune(r)) < 3 {
		return "", ErrReason.WithField("body.edit_reason")
	}
	return r, nil
}

// overlaps dice si el tramo cruza otro marcaje cerrado del colaborador.
func overlaps(ctx context.Context, q database.Querier, emp uuid.UUID, in, out time.Time, except *uuid.UUID) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM attendance_records
		               WHERE employee_id = $1 AND ($4::uuid IS NULL OR id <> $4)
		                 AND tstzrange(clock_in_at, coalesce(clock_out_at, clock_in_at), '[]') && tstzrange($2, $3, '()'))`,
		emp, in, out, except).Scan(&ok)
	return ok, err
}

// Create registra un marcaje a mano (o cierra el abierto de ese dia).
func (s *Service) Create(ctx context.Context, actor authz.Actor, in ManualCreate) (RecordRead, error) {
	why, err := reason(in.EditReason)
	if err != nil {
		return RecordRead{}, err
	}
	if err := s.checkTimes(in.ClockInAt, &in.ClockOutAt); err != nil {
		return RecordRead{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (RecordRead, error) {
		var exists bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM employees WHERE id = $1)`, in.EmployeeID).Scan(&exists); err != nil {
			return RecordRead{}, err
		}
		if !exists {
			return RecordRead{}, ErrEmployee.WithField("body.employee_id")
		}
		// El abierto del mismo dia se cierra con estas horas.
		var openID *uuid.UUID
		if err := q.QueryRow(ctx, `
			SELECT id FROM attendance_records
			WHERE employee_id = $1 AND clock_out_at IS NULL
			  AND (clock_in_at AT TIME ZONE `+tenantTZ+`)::date = ($2::timestamptz AT TIME ZONE `+tenantTZ+`)::date`,
			in.EmployeeID, in.ClockInAt).Scan(&openID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return RecordRead{}, err
		}
		cross, err := overlaps(ctx, q, in.EmployeeID, in.ClockInAt, in.ClockOutAt, openID)
		if err != nil {
			return RecordRead{}, err
		}
		if cross {
			return RecordRead{}, ErrOverlap
		}
		var before *RecordRead
		var id uuid.UUID
		if openID != nil {
			b, err := recordByID(ctx, q, *openID)
			if err != nil {
				return RecordRead{}, err
			}
			before = &b
			id = *openID
			_, err = q.Exec(ctx, `
				UPDATE attendance_records SET clock_in_at = $2, clock_out_at = $3,
				       worked_minutes = floor(extract(epoch FROM $3::timestamptz - $2::timestamptz) / 60)::int,
				       edited_by = $4, edited_at = now(), edit_reason = $5, autoclosed = false, updated_at = now()
				WHERE id = $1`, id, in.ClockInAt, in.ClockOutAt, actor.UserID, why)
			if err != nil {
				return RecordRead{}, err
			}
		} else {
			err := q.QueryRow(ctx, `
				INSERT INTO attendance_records (employee_id, clock_in_at, clock_out_at, source, edited_by, edited_at, edit_reason, worked_minutes)
				VALUES ($1, $2, $3, 'manual', $4, now(), $5, floor(extract(epoch FROM $3::timestamptz - $2::timestamptz) / 60)::int)
				RETURNING id`, in.EmployeeID, in.ClockInAt, in.ClockOutAt, actor.UserID, why).Scan(&id)
			if err != nil {
				return RecordRead{}, err
			}
		}
		after, err := recordByID(ctx, q, id)
		if err != nil {
			return RecordRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{Actor: &actor.UserID, Action: "attendance.manual", Entity: "attendance_record",
			EntityID: &id, Before: before, After: after}); err != nil {
			return RecordRead{}, err
		}
		return after, nil
	})
}

// Update corrige las horas de un marcaje.
func (s *Service) Update(ctx context.Context, actor authz.Actor, id uuid.UUID, in RecordUpdate) (RecordRead, error) {
	why, err := reason(in.EditReason)
	if err != nil {
		return RecordRead{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (RecordRead, error) {
		before, err := recordByID(ctx, q, id)
		if err != nil {
			return RecordRead{}, err
		}
		clockIn, clockOut := before.ClockInAt, before.ClockOutAt
		if in.ClockInAt != nil {
			clockIn = *in.ClockInAt
		}
		if in.ClockOutAt != nil {
			clockOut = in.ClockOutAt
		}
		if err := s.checkTimes(clockIn, clockOut); err != nil {
			return RecordRead{}, err
		}
		if clockOut != nil {
			cross, err := overlaps(ctx, q, before.EmployeeID, clockIn, *clockOut, &id)
			if err != nil {
				return RecordRead{}, err
			}
			if cross {
				return RecordRead{}, ErrOverlap
			}
		}
		_, err = q.Exec(ctx, `
			UPDATE attendance_records SET clock_in_at = $2, clock_out_at = $3,
			       worked_minutes = CASE WHEN $3::timestamptz IS NULL THEN NULL
			                             ELSE floor(extract(epoch FROM $3::timestamptz - $2::timestamptz) / 60)::int END,
			       edited_by = $4, edited_at = now(), edit_reason = $5, autoclosed = false, updated_at = now()
			WHERE id = $1`, id, clockIn, clockOut, actor.UserID, why)
		if isOpenConflict(err) {
			return RecordRead{}, ErrOpenElsewhere
		}
		if err != nil {
			return RecordRead{}, err
		}
		after, err := recordByID(ctx, q, id)
		if err != nil {
			return RecordRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{Actor: &actor.UserID, Action: "attendance.corrected", Entity: "attendance_record",
			EntityID: &id, Before: before, After: after}); err != nil {
			return RecordRead{}, err
		}
		return after, nil
	})
}

// --- Trabajo -------------------------------------------------------------------

// HandleJob corre attendance_autoclose: sin empresa encola uno por empresa
// activa; con empresa cierra los marcajes vencidos.
func (s *Service) HandleJob(ctx context.Context, job jobs.Running) error {
	if job.Type != JobAutoclose {
		return fmt.Errorf("trabajo desconocido %s", job.Type)
	}
	if job.TenantID == nil {
		return s.db.InTx(ctx, func(q database.Querier) error {
			rows, err := q.Query(ctx, `SELECT id FROM shared.tenants WHERE is_active ORDER BY slug`)
			if err != nil {
				return err
			}
			ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			if err != nil {
				return err
			}
			for _, id := range ids {
				if err := jobs.Enqueue(ctx, q, jobs.Job{
					Type: JobAutoclose, TenantID: &id, DedupeKey: JobAutoclose + ":" + id.String(), MaxAttempts: 3,
				}); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		_, err := s.Autoclose(ctx, q, *job.TenantID)
		return err
	})
}

// Autoclose cierra a las 16 h los marcajes que siguen abiertos y avisa a
// quien administra la asistencia. Devuelve cuantos cerro.
func (s *Service) Autoclose(ctx context.Context, q database.Querier, tenantID uuid.UUID) (int, error) {
	rows, err := q.Query(ctx, `
		UPDATE attendance_records SET clock_out_at = clock_in_at + make_interval(mins => $2),
		       worked_minutes = $2, autoclosed = true, updated_at = now()
		WHERE clock_out_at IS NULL AND clock_in_at < $1::timestamptz - make_interval(mins => $2)
		RETURNING id`, s.now(), int(MaxOpen.Minutes()))
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	holders, err := notifications.Holders(ctx, q, tenantID, nil, authz.HRAttendanceManage)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		r, err := recordByID(ctx, q, id)
		if err != nil {
			return 0, err
		}
		if err := audit.Record(ctx, q, audit.Entry{Action: "attendance.autoclosed", Entity: "attendance_record", EntityID: &id, After: r}); err != nil {
			return 0, err
		}
		if s.notif == nil {
			continue
		}
		if err := s.notif.Notify(ctx, q, tenantID, notifications.Notice{
			Recipients: holders, Type: notifications.TypeAttendanceAutoclosed,
			Title: r.EmployeeName + " no marcó salida",
			Body:  fmt.Sprintf("Su entrada del %s se cerró sola a las 16 horas. Corrige la hora de salida.", r.Date),
			Route: "/rrhh/asistencia?empleado=" + r.EmployeeID.String(), AppRoute: "/alerts",
			Data:     map[string]any{"attendance_id": id.String(), "employee_id": r.EmployeeID.String()},
			Channels: []string{notifications.ChannelInApp, notifications.ChannelPush},
		}); err != nil {
			return 0, err
		}
		// El colaborador tambien se entera (en la bandeja).
		var user uuid.UUID
		if err := q.QueryRow(ctx, `SELECT user_id FROM employees WHERE id = $1`, r.EmployeeID).Scan(&user); err == nil {
			if err := s.notif.Notify(ctx, q, tenantID, notifications.Notice{
				Recipients: []uuid.UUID{user}, Type: notifications.TypeAttendanceAutoclosed,
				Title: "Tu entrada se cerró sola", Body: "No marcaste salida el " + r.Date + ". RRHH corregirá la hora.",
				AppRoute: "/attendance", Data: map[string]any{"attendance_id": id.String()},
				Channels: []string{notifications.ChannelInApp},
			}); err != nil {
				return 0, err
			}
		}
	}
	s.log.InfoContext(ctx, "Marcajes cerrados solos", "count", len(ids))
	return len(ids), nil
}
