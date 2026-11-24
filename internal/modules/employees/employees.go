// Package employees gestiona el perfil laboral de las personas de la empresa
// (tabla employees) y su horario semanal (employee_schedules): fase02.md §2,
// pantalla W-D2 sin las pestanas de RRHH.
//
// Los documentos (DPI, IGSS, NIT) son datos personales: la bitacora registra
// que campos cambiaron, nunca sus valores.
package employees

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/optional"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Errores del modulo.
var (
	ErrEmployeeNotFound = apperr.New(apperr.KindNotFound, "Esa persona no tiene ficha laboral.")
	ErrNotMember        = apperr.New(apperr.KindValidation, "La persona no pertenece a la empresa.")
	ErrAlreadyEmployee  = apperr.New(apperr.KindValidation, "Esta persona ya tiene ficha laboral.")
	ErrCodeTaken        = apperr.New(apperr.KindValidation, "Ya hay un colaborador con ese código.")
	ErrInvalidCode      = apperr.New(apperr.KindValidation, "El código usa hasta 32 letras, números o guiones.")
	ErrInvalidPosition  = apperr.New(apperr.KindValidation, "El puesto debe tener entre 2 y 64 caracteres.")
	ErrInvalidDate      = apperr.New(apperr.KindValidation, "La fecha no es válida.")
	ErrTermination      = apperr.New(apperr.KindValidation, "La fecha de baja no puede ser anterior al ingreso.")
	ErrDocumentType     = apperr.New(apperr.KindValidation, "Indica el tipo de documento.")
	ErrDPI              = apperr.New(apperr.KindValidation, "El DPI tiene 13 dígitos.")
	ErrNIT              = apperr.New(apperr.KindValidation, "El NIT no es válido (p. ej. 1234567-8 o CF).")
	ErrEmergency        = apperr.New(apperr.KindValidation, "El contacto de emergencia necesita nombre y teléfono.")
	ErrScheduleRange    = apperr.New(apperr.KindValidation, "Cada bloque debe terminar después de empezar.")
	ErrScheduleOverlap  = apperr.New(apperr.KindValidation, "Hay bloques que se cruzan el mismo día.")
)

const selectEmployee = `SELECT e.id, e.user_id, u.full_name, u.email, e.employee_code, e.position,
       to_char(e.hire_date, 'YYYY-MM-DD') AS hire_date, to_char(e.termination_date, 'YYYY-MM-DD') AS termination_date,
       e.document_type, e.document_number, e.igss_number, e.nit, e.pay_type, e.pay_rate::float8 AS pay_rate,
       e.on_payroll, e.emergency_contact->>'name' AS emergency_name, e.emergency_contact->>'phone' AS emergency_phone,
       e.emergency_contact->>'relation' AS emergency_relation, e.notes, e.created_at, e.updated_at,
       COALESCE((SELECT jsonb_agg(jsonb_build_object('weekday', s.weekday, 'starts_at', to_char(s.starts_at, 'HH24:MI'),
                                                     'ends_at', to_char(s.ends_at, 'HH24:MI'))
                                  ORDER BY s.weekday, s.starts_at)
                 FROM employee_schedules s WHERE s.employee_id = e.id), '[]'::jsonb) AS schedule
FROM employees e
JOIN shared.users u ON u.id = e.user_id`

// Service contiene los casos de uso del perfil laboral.
type Service struct {
	db  *database.DB
	log *slog.Logger
}

// NewService crea el servicio.
func NewService(db *database.DB, log *slog.Logger) *Service {
	return &Service{db: db, log: log}
}

// List devuelve las fichas de la empresa por nombre (userID filtra a una).
func (s *Service) List(ctx context.Context, userID *uuid.UUID) ([]EmployeeRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]EmployeeRead, error) {
		rows, err := q.Query(ctx, selectEmployee+`
			WHERE $1::uuid IS NULL OR e.user_id = $1 ORDER BY u.full_name`, userID)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[EmployeeRead])
	})
}

// Get devuelve una ficha.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (EmployeeRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (EmployeeRead, error) {
		return byID(ctx, q, id)
	})
}

// Create da de alta la ficha laboral de una persona de la empresa.
func (s *Service) Create(ctx context.Context, actor authz.Actor, in EmployeeCreate) (EmployeeRead, error) {
	d := employeeData{
		Position: in.Position, HireDate: in.HireDate, TerminationDate: in.TerminationDate,
		DocumentType: in.DocumentType, DocumentNumber: in.DocumentNumber, IGSSNumber: in.IGSSNumber, NIT: in.NIT,
		PayType: in.PayType, PayRate: in.PayRate, OnPayroll: in.OnPayroll, EmergencyName: in.EmergencyName,
		EmergencyPhone: in.EmergencyPhone, EmergencyRelation: in.EmergencyRelation, Notes: in.Notes,
	}
	if in.EmployeeCode != nil {
		d.Code = *in.EmployeeCode
	}
	if err := d.prepare(); err != nil {
		return EmployeeRead{}, err
	}
	blocks, err := cleanSchedule(in.Schedule, "body.schedule")
	if err != nil {
		return EmployeeRead{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (EmployeeRead, error) {
		if err := checkNewEmployee(ctx, q, actor.TenantID, in.UserID); err != nil {
			return EmployeeRead{}, err
		}
		if err := ensureCode(ctx, q, &d, nil); err != nil {
			return EmployeeRead{}, err
		}
		id, err := insert(ctx, q, in.UserID, d)
		if err != nil {
			return EmployeeRead{}, err
		}
		if err := replaceSchedule(ctx, q, id, blocks); err != nil {
			return EmployeeRead{}, err
		}
		e, err := byID(ctx, q, id)
		if err != nil {
			return EmployeeRead{}, err
		}
		return e, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "employee.created", Entity: "employee", EntityID: &id,
			After: map[string]any{"user_id": e.UserID, "employee_code": e.EmployeeCode, "position": e.Position},
		})
	})
}

// Update cambia los datos que vienen en el cuerpo.
func (s *Service) Update(ctx context.Context, actor authz.Actor, id uuid.UUID, in EmployeeUpdate) (EmployeeRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (EmployeeRead, error) {
		cur, err := byID(ctx, q, id)
		if err != nil {
			return EmployeeRead{}, err
		}
		d := merge(cur, in)
		if err := d.prepare(); err != nil {
			return EmployeeRead{}, err
		}
		if err := ensureCode(ctx, q, &d, &id); err != nil {
			return EmployeeRead{}, err
		}
		if err := update(ctx, q, id, d); err != nil {
			return EmployeeRead{}, err
		}
		e, err := byID(ctx, q, id)
		if err != nil {
			return EmployeeRead{}, err
		}
		return e, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "employee.updated", Entity: "employee", EntityID: &id,
			After: map[string]any{"user_id": e.UserID, "fields": changedFields(cur, e)},
		})
	})
}

// SetSchedule reemplaza el horario semanal.
func (s *Service) SetSchedule(ctx context.Context, actor authz.Actor, id uuid.UUID, in []ScheduleBlock) (EmployeeRead, error) {
	blocks, err := cleanSchedule(in, "body.blocks")
	if err != nil {
		return EmployeeRead{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (EmployeeRead, error) {
		cur, err := byID(ctx, q, id)
		if err != nil {
			return EmployeeRead{}, err
		}
		if err := replaceSchedule(ctx, q, id, blocks); err != nil {
			return EmployeeRead{}, err
		}
		e, err := byID(ctx, q, id)
		if err != nil {
			return EmployeeRead{}, err
		}
		return e, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "employee.schedule_changed", Entity: "employee", EntityID: &id,
			Before: map[string]any{"schedule": cur.Schedule}, After: map[string]any{"schedule": e.Schedule},
		})
	})
}

// --- Datos ---------------------------------------------------------------------

type employeeData struct {
	Code, Position, HireDate                 string
	TerminationDate                          *string
	DocumentType, DocumentNumber, IGSSNumber *string
	NIT                                      *string
	PayType                                  string
	PayRate                                  float64
	OnPayroll                                bool
	EmergencyName, EmergencyPhone            *string
	EmergencyRelation, Notes                 *string
}

var (
	codePattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{0,31}$`)
	dpiPattern  = regexp.MustCompile(`^[0-9]{13}$`)
	nitPattern  = regexp.MustCompile(`^([0-9]{1,12}-?[0-9K]|CF)$`)
)

// prepare normaliza y valida (el codigo vacio se genera despues).
func (d *employeeData) prepare() error {
	d.Code = strings.ToUpper(strings.TrimSpace(d.Code))
	if d.Code != "" && !codePattern.MatchString(d.Code) {
		return ErrInvalidCode.WithField("body.employee_code")
	}
	d.Position = strings.Join(strings.Fields(d.Position), " ")
	if n := utf8.RuneCountInString(d.Position); n < 2 || n > 64 {
		return ErrInvalidPosition.WithField("body.position")
	}
	if err := d.checkDates(); err != nil {
		return err
	}
	if err := d.checkDocuments(); err != nil {
		return err
	}
	d.EmergencyName, d.EmergencyPhone = clean(d.EmergencyName), clean(d.EmergencyPhone)
	d.EmergencyRelation, d.Notes, d.IGSSNumber = clean(d.EmergencyRelation), clean(d.Notes), clean(d.IGSSNumber)
	if (d.EmergencyName == nil) != (d.EmergencyPhone == nil) {
		return ErrEmergency.WithField("body.emergency_phone")
	}
	d.PayRate = math.Round(d.PayRate*100) / 100
	return nil
}

func (d *employeeData) checkDates() error {
	hire, err := time.Parse(time.DateOnly, d.HireDate)
	if err != nil {
		return ErrInvalidDate.WithField("body.hire_date")
	}
	d.TerminationDate = clean(d.TerminationDate)
	if d.TerminationDate == nil {
		return nil
	}
	end, err := time.Parse(time.DateOnly, *d.TerminationDate)
	if err != nil {
		return ErrInvalidDate.WithField("body.termination_date")
	}
	if end.Before(hire) {
		return ErrTermination.WithField("body.termination_date")
	}
	return nil
}

func (d *employeeData) checkDocuments() error {
	d.DocumentType, d.DocumentNumber = clean(d.DocumentType), clean(d.DocumentNumber)
	if d.DocumentNumber != nil {
		if d.DocumentType == nil {
			return ErrDocumentType.WithField("body.document_type")
		}
		if *d.DocumentType == "dpi" {
			digits := strings.NewReplacer(" ", "", "-", "").Replace(*d.DocumentNumber)
			if !dpiPattern.MatchString(digits) {
				return ErrDPI.WithField("body.document_number")
			}
			d.DocumentNumber = &digits
		}
	}
	d.NIT = clean(d.NIT)
	if d.NIT == nil {
		return nil
	}
	nit := strings.ToUpper(strings.ReplaceAll(*d.NIT, " ", ""))
	if !nitPattern.MatchString(nit) {
		return ErrNIT.WithField("body.nit")
	}
	d.NIT = &nit
	return nil
}

func merge(cur EmployeeRead, in EmployeeUpdate) employeeData {
	d := employeeData{
		Code: deref(in.EmployeeCode, cur.EmployeeCode), Position: deref(in.Position, cur.Position),
		HireDate: deref(in.HireDate, cur.HireDate), PayType: deref(in.PayType, cur.PayType),
		PayRate: deref(in.PayRate, cur.PayRate), OnPayroll: deref(in.OnPayroll, cur.OnPayroll),
	}
	d.TerminationDate = optional.Apply(cur.TerminationDate, in.TerminationDate)
	d.DocumentType = optional.Apply(cur.DocumentType, in.DocumentType)
	d.DocumentNumber = optional.Apply(cur.DocumentNumber, in.DocumentNumber)
	d.IGSSNumber = optional.Apply(cur.IGSSNumber, in.IGSSNumber)
	d.NIT = optional.Apply(cur.NIT, in.NIT)
	d.EmergencyName = optional.Apply(cur.EmergencyName, in.EmergencyName)
	d.EmergencyPhone = optional.Apply(cur.EmergencyPhone, in.EmergencyPhone)
	d.EmergencyRelation = optional.Apply(cur.EmergencyRelation, in.EmergencyRelation)
	d.Notes = optional.Apply(cur.Notes, in.Notes)
	return d
}

func (d employeeData) emergency() ([]byte, error) {
	if d.EmergencyName == nil {
		return nil, nil
	}
	m := map[string]string{"name": *d.EmergencyName, "phone": *d.EmergencyPhone}
	if d.EmergencyRelation != nil {
		m["relation"] = *d.EmergencyRelation
	}
	return json.Marshal(m)
}

func insert(ctx context.Context, q database.Querier, userID uuid.UUID, d employeeData) (uuid.UUID, error) {
	contact, err := d.emergency()
	if err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err = q.QueryRow(ctx, `
		INSERT INTO employees (user_id, employee_code, position, hire_date, termination_date, document_type,
		                       document_number, igss_number, nit, pay_type, pay_rate, on_payroll, emergency_contact, notes)
		VALUES ($1, $2, $3, $4::date, $5::date, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING id`,
		userID, d.Code, d.Position, d.HireDate, d.TerminationDate, d.DocumentType, d.DocumentNumber, d.IGSSNumber,
		d.NIT, d.PayType, d.PayRate, d.OnPayroll, contact, d.Notes).Scan(&id)
	return id, err
}

func update(ctx context.Context, q database.Querier, id uuid.UUID, d employeeData) error {
	contact, err := d.emergency()
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `
		UPDATE employees SET employee_code = $2, position = $3, hire_date = $4::date, termination_date = $5::date,
		       document_type = $6, document_number = $7, igss_number = $8, nit = $9, pay_type = $10, pay_rate = $11,
		       on_payroll = $12, emergency_contact = $13, notes = $14, updated_at = now()
		WHERE id = $1`,
		id, d.Code, d.Position, d.HireDate, d.TerminationDate, d.DocumentType, d.DocumentNumber, d.IGSSNumber,
		d.NIT, d.PayType, d.PayRate, d.OnPayroll, contact, d.Notes)
	return err
}

// checkNewEmployee: la persona es de la empresa y aun no tiene ficha.
func checkNewEmployee(ctx context.Context, q database.Querier, tenantID, userID uuid.UUID) error {
	var member, exists bool
	if err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM shared.memberships WHERE tenant_id = $1 AND user_id = $2),
		       EXISTS (SELECT 1 FROM employees WHERE user_id = $2)`, tenantID, userID).Scan(&member, &exists); err != nil {
		return err
	}
	if !member {
		return ErrNotMember.WithField("body.user_id")
	}
	if exists {
		return ErrAlreadyEmployee.WithField("body.user_id")
	}
	return nil
}

// ensureCode genera el codigo si falta (EMP-0001...) o valida que no se repita.
func ensureCode(ctx context.Context, q database.Querier, d *employeeData, self *uuid.UUID) error {
	if d.Code == "" {
		var n int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM employees`).Scan(&n); err != nil {
			return err
		}
		for {
			n++
			d.Code = fmt.Sprintf("EMP-%04d", n)
			taken, err := codeTaken(ctx, q, d.Code, self)
			if err != nil || !taken {
				return err
			}
		}
	}
	taken, err := codeTaken(ctx, q, d.Code, self)
	if err != nil {
		return err
	}
	if taken {
		return ErrCodeTaken.WithField("body.employee_code")
	}
	return nil
}

func codeTaken(ctx context.Context, q database.Querier, code string, self *uuid.UUID) (bool, error) {
	var taken bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM employees WHERE upper(employee_code) = upper($1) AND ($2::uuid IS NULL OR id <> $2))`,
		code, self).Scan(&taken)
	return taken, err
}

func byID(ctx context.Context, q database.Querier, id uuid.UUID) (EmployeeRead, error) {
	rows, err := q.Query(ctx, selectEmployee+` WHERE e.id = $1`, id)
	if err != nil {
		return EmployeeRead{}, err
	}
	e, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[EmployeeRead])
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EmployeeRead{}, ErrEmployeeNotFound
		}
		return EmployeeRead{}, err
	}
	if e.Schedule == nil {
		e.Schedule = []ScheduleBlock{}
	}
	return e, nil
}

// --- Horario ---------------------------------------------------------------------

type block struct {
	weekday      int
	starts, ends string
}

// cleanSchedule valida los bloques: fin despues de inicio y sin cruces el
// mismo dia.
func cleanSchedule(in []ScheduleBlock, field string) ([]block, error) {
	out := make([]block, 0, len(in))
	for _, b := range in {
		start, err1 := time.Parse("15:04", b.StartsAt)
		end, err2 := time.Parse("15:04", b.EndsAt)
		if err1 != nil || err2 != nil || !end.After(start) {
			return nil, ErrScheduleRange.WithField(field)
		}
		for _, o := range out {
			if o.weekday == b.Weekday && b.StartsAt < o.ends && o.starts < b.EndsAt {
				return nil, ErrScheduleOverlap.WithField(field)
			}
		}
		out = append(out, block{weekday: b.Weekday, starts: b.StartsAt, ends: b.EndsAt})
	}
	return out, nil
}

func replaceSchedule(ctx context.Context, q database.Querier, employeeID uuid.UUID, blocks []block) error {
	if _, err := q.Exec(ctx, `DELETE FROM employee_schedules WHERE employee_id = $1`, employeeID); err != nil {
		return err
	}
	days, starts, ends := make([]int, len(blocks)), make([]string, len(blocks)), make([]string, len(blocks))
	for i, b := range blocks {
		days[i], starts[i], ends[i] = b.weekday, b.starts, b.ends
	}
	_, err := q.Exec(ctx, `
		INSERT INTO employee_schedules (employee_id, weekday, starts_at, ends_at)
		SELECT $1, d, s::time, e::time FROM unnest($2::int[], $3::text[], $4::text[]) AS x(d, s, e)`,
		employeeID, days, starts, ends)
	return err
}

// --- Utilidades ------------------------------------------------------------------

// changedFields lista los campos que cambiaron (sin sus valores).
func changedFields(a, b EmployeeRead) []string {
	pairs := []struct {
		name string
		x, y any
	}{
		{"employee_code", a.EmployeeCode, b.EmployeeCode},
		{"position", a.Position, b.Position},
		{"hire_date", a.HireDate, b.HireDate},
		{"termination_date", str(a.TerminationDate), str(b.TerminationDate)},
		{"document_type", str(a.DocumentType), str(b.DocumentType)},
		{"document_number", str(a.DocumentNumber), str(b.DocumentNumber)},
		{"igss_number", str(a.IGSSNumber), str(b.IGSSNumber)},
		{"nit", str(a.NIT), str(b.NIT)},
		{"pay_type", a.PayType, b.PayType},
		{"pay_rate", a.PayRate, b.PayRate},
		{"on_payroll", a.OnPayroll, b.OnPayroll},
		{
			"emergency_contact", str(a.EmergencyName) + str(a.EmergencyPhone) + str(a.EmergencyRelation),
			str(b.EmergencyName) + str(b.EmergencyPhone) + str(b.EmergencyRelation),
		},
		{"notes", str(a.Notes), str(b.Notes)},
	}
	out := []string{}
	for _, p := range pairs {
		if p.x != p.y {
			out = append(out, p.name)
		}
	}
	return out
}

func clean(p *string) *string {
	if p == nil {
		return nil
	}
	t := strings.TrimSpace(*p)
	if t == "" {
		return nil
	}
	return &t
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func deref[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}
