// Package disciplinary registra el procedimiento disciplinario (fase05.md §3,
// parte de Persona A): llamadas de atencion, amonestaciones y suspensiones con
// adjuntos, aviso al colaborador y su acuse desde la app. Nunca toca la nomina.
package disciplinary

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/core/mail"
	"github.com/smorareqs/argos-service/internal/core/paging"
	"github.com/smorareqs/argos-service/internal/core/storage"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Eventos (B envia el push y lo muestra en la app).
const (
	// EventCreated: Data es ActionRead.
	EventCreated = "disciplinary.created"
	// EventAcknowledged: Data es ActionRead.
	EventAcknowledged = "disciplinary.acknowledged"
)

// ActionRead es una accion disciplinaria.
type ActionRead struct {
	ID                  uuid.UUID    `json:"id"`
	EmployeeID          uuid.UUID    `json:"employee_id"`
	UserID              uuid.UUID    `json:"user_id"`
	EmployeeName        string       `json:"employee_name"`
	EmployeeCode        string       `json:"employee_code"`
	Position            string       `json:"position"`
	Type                string       `json:"type" enum:"verbal,written,suspension,other"`
	OccurredOn          string       `json:"occurred_on" format:"date"`
	Description         string       `json:"description"`
	IssuedBy            *uuid.UUID   `json:"issued_by"`
	IssuedByName        *string      `json:"issued_by_name"`
	SuspensionFrom      *string      `json:"suspension_from" format:"date"`
	SuspensionTo        *string      `json:"suspension_to" format:"date"`
	AcknowledgedAt      *time.Time   `json:"acknowledged_at"`
	AcknowledgementNote *string      `json:"acknowledgement_note"`
	Attachments         []Attachment `json:"attachments"`
	CreatedAt           time.Time    `json:"created_at"`
	UpdatedAt           time.Time    `json:"updated_at"`
}

// Attachment es un adjunto con su URL firmada (15 min).
type Attachment struct {
	FileID      uuid.UUID `json:"file_id"`
	Name        string    `json:"name"`
	ContentType string    `json:"content_type"`
	URL         string    `json:"url"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// ActionCreate registra una accion.
type ActionCreate struct {
	EmployeeID        uuid.UUID   `json:"employee_id"`
	Type              string      `json:"type" enum:"verbal,written,suspension,other"`
	OccurredOn        string      `json:"occurred_on" format:"date"`
	Description       string      `json:"description" minLength:"10" maxLength:"4000"`
	SuspensionFrom    *string     `json:"suspension_from,omitempty" format:"date" doc:"Obligatorio en suspension."`
	SuspensionTo      *string     `json:"suspension_to,omitempty" format:"date"`
	AttachmentFileIDs []uuid.UUID `json:"attachment_file_ids,omitempty" maxItems:"10" doc:"Archivos subidos con modulo disciplinary y tipo attachment."`
}

// Acknowledge es el acuse del colaborador.
type Acknowledge struct {
	Note *string `json:"note,omitempty" maxLength:"2000" doc:"Su version, si quiere dejarla."`
}

// Filter filtra la lista.
type Filter struct {
	EmployeeID *uuid.UUID
	Pending    bool
	Cursor     string
	Limit      int
}

// Errores del modulo.
var (
	ErrNotFound         = apperr.New(apperr.KindNotFound, "La acción disciplinaria no existe.")
	ErrEmployee         = apperr.New(apperr.KindValidation, "Elige un colaborador con ficha laboral.")
	ErrDate             = apperr.New(apperr.KindValidation, "La fecha no es válida o es futura.")
	ErrSuspension       = apperr.New(apperr.KindValidation, "Una suspensión necesita fecha de inicio y de fin (el fin igual o después del inicio).")
	ErrNoSuspension     = apperr.New(apperr.KindValidation, "Las fechas de suspensión solo van en una suspensión.")
	ErrAttachments      = apperr.New(apperr.KindValidation, "Algún adjunto no se subió bien. Vuelve a adjuntarlo.")
	ErrAcknowledged     = apperr.New(apperr.KindValidation, "Ya diste acuse de esta acción.")
	ErrDescriptionShort = apperr.New(apperr.KindValidation, "Describe qué pasó (al menos 10 caracteres).")
)

// Service contiene los casos de uso.
type Service struct {
	db      *database.DB
	storage *storage.Service
	bus     *events.Bus
	log     *slog.Logger
	now     func() time.Time
}

// NewService crea el servicio.
func NewService(db *database.DB, st *storage.Service, bus *events.Bus, log *slog.Logger) *Service {
	return &Service{db: db, storage: st, bus: bus, log: log, now: time.Now}
}

const selectAction = `
	SELECT a.id, a.employee_id, em.user_id, u.full_name AS employee_name, em.employee_code, em.position, a.type,
	       to_char(a.occurred_on, 'YYYY-MM-DD') AS occurred_on, a.description, a.issued_by, ib.full_name AS issued_by_name,
	       to_char(a.suspension_from, 'YYYY-MM-DD') AS suspension_from, to_char(a.suspension_to, 'YYYY-MM-DD') AS suspension_to,
	       a.acknowledged_at, a.acknowledgement_note, a.created_at, a.updated_at
	FROM disciplinary_actions a
	JOIN employees em ON em.id = a.employee_id
	JOIN shared.users u ON u.id = em.user_id
	LEFT JOIN shared.users ib ON ib.id = a.issued_by`

func (s *Service) query(ctx context.Context, q database.Querier, tenantID uuid.UUID, where string, args ...any) ([]ActionRead, error) {
	rows, err := q.Query(ctx, selectAction+` WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByNameLax[ActionRead])
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Attachments, err = s.attachments(ctx, q, tenantID, list[i].ID); err != nil {
			return nil, err
		}
	}
	return list, nil
}

func (s *Service) attachments(ctx context.Context, q database.Querier, tenantID, id uuid.UUID) ([]Attachment, error) {
	rows, err := q.Query(ctx, `SELECT id, s3_key, coalesce(original_name, 'adjunto'), content_type FROM files
		WHERE entity_type = 'disciplinary_action' AND entity_id = $1 AND status = 'ready' ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		var a Attachment
		var key string
		if err := rows.Scan(&a.FileID, &key, &a.Name, &a.ContentType); err != nil {
			return nil, err
		}
		if a.URL, err = s.storage.PresignDownload(ctx, tenantID, key, 15*time.Minute); err != nil {
			return nil, err
		}
		a.ExpiresAt = s.now().Add(15 * time.Minute)
		out = append(out, a)
	}
	return out, rows.Err()
}

// List lista las acciones (las mas recientes primero), por colaborador o solo
// las que esperan acuse (W-J4).
func (s *Service) List(ctx context.Context, actor authz.Actor, f Filter) ([]ActionRead, *string, error) {
	key, err := paging.Decode(f.Cursor, 2)
	if err != nil {
		return nil, nil, err
	}
	limit := paging.Limit(f.Limit)
	var afterAt, afterID *string
	if key != nil {
		afterAt, afterID = &key[0], &key[1]
	}
	list, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]ActionRead, error) {
		return s.query(ctx, q, actor.TenantID, `($1::uuid IS NULL OR a.employee_id = $1)
			AND (NOT $2::bool OR a.acknowledged_at IS NULL)
			AND ($3::timestamptz IS NULL OR (a.created_at, a.id) < ($3::timestamptz, $4::uuid))
			ORDER BY a.created_at DESC, a.id DESC LIMIT $5`, f.EmployeeID, f.Pending, afterAt, afterID, limit+1)
	})
	if err != nil {
		return nil, nil, err
	}
	page, next := paging.Page(list, limit, func(a ActionRead) []string {
		return []string{a.CreatedAt.Format(time.RFC3339Nano), a.ID.String()}
	})
	return page, next, nil
}

// Get devuelve una accion: quien administra disciplina ve todas; el
// colaborador, solo las suyas (404 en las demas).
func (s *Service) Get(ctx context.Context, actor authz.Actor, id uuid.UUID) (ActionRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (ActionRead, error) {
		return s.visible(ctx, q, actor, id)
	})
}

func (s *Service) visible(ctx context.Context, q database.Querier, actor authz.Actor, id uuid.UUID) (ActionRead, error) {
	list, err := s.query(ctx, q, actor.TenantID, `a.id = $1`, id)
	if err != nil {
		return ActionRead{}, err
	}
	if len(list) == 0 || (!actor.Permissions[authz.HRDisciplinaryManage] && list[0].UserID != actor.UserID) {
		return ActionRead{}, ErrNotFound
	}
	return list[0], nil
}

// Me son las acciones del colaborador (app).
func (s *Service) Me(ctx context.Context, actor authz.Actor) ([]ActionRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]ActionRead, error) {
		list, err := s.query(ctx, q, actor.TenantID, `em.user_id = $1 ORDER BY a.occurred_on DESC, a.created_at DESC`, actor.UserID)
		if list == nil {
			list = []ActionRead{}
		}
		return list, err
	})
}

// Create registra una accion, liga sus adjuntos, avisa al colaborador por
// correo y publica disciplinary.created.
func (s *Service) Create(ctx context.Context, actor authz.Actor, in ActionCreate) (ActionRead, error) {
	if err := validate(in, s.now()); err != nil {
		return ActionRead{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (ActionRead, error) {
		var email, name string
		err := q.QueryRow(ctx, `SELECT u.email, u.full_name FROM employees em JOIN shared.users u ON u.id = em.user_id WHERE em.id = $1`,
			in.EmployeeID).Scan(&email, &name)
		if errors.Is(err, pgx.ErrNoRows) {
			return ActionRead{}, ErrEmployee.WithField("body.employee_id")
		}
		if err != nil {
			return ActionRead{}, err
		}
		var id uuid.UUID
		if err := q.QueryRow(ctx, `
			INSERT INTO disciplinary_actions (employee_id, type, occurred_on, description, issued_by, suspension_from, suspension_to)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			in.EmployeeID, in.Type, in.OccurredOn, strings.TrimSpace(in.Description), actor.UserID, in.SuspensionFrom, in.SuspensionTo).
			Scan(&id); err != nil {
			return ActionRead{}, err
		}
		if err := attach(ctx, q, actor, id, in.AttachmentFileIDs); err != nil {
			return ActionRead{}, err
		}
		a, err := s.visible(ctx, q, actor, id)
		if err != nil {
			return ActionRead{}, err
		}
		if err := s.notify(ctx, q, actor, email, a); err != nil {
			return ActionRead{}, err
		}
		return a, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "disciplinary.created", Entity: "disciplinary_action", EntityID: &id,
			After: map[string]any{"employee": name, "type": a.Type, "occurred_on": a.OccurredOn},
		})
	})
}

func validate(in ActionCreate, now time.Time) error {
	if len([]rune(strings.TrimSpace(in.Description))) < 10 {
		return ErrDescriptionShort.WithField("body.description")
	}
	occurred, err := time.Parse(time.DateOnly, in.OccurredOn)
	if err != nil || occurred.After(now.AddDate(0, 0, 1)) {
		return ErrDate.WithField("body.occurred_on")
	}
	if in.Type != "suspension" {
		if in.SuspensionFrom != nil || in.SuspensionTo != nil {
			return ErrNoSuspension.WithField("body.suspension_from")
		}
		return nil
	}
	if in.SuspensionFrom == nil || in.SuspensionTo == nil {
		return ErrSuspension.WithField("body.suspension_from")
	}
	from, err1 := time.Parse(time.DateOnly, *in.SuspensionFrom)
	to, err2 := time.Parse(time.DateOnly, *in.SuspensionTo)
	if err1 != nil || err2 != nil || to.Before(from) {
		return ErrSuspension.WithField("body.suspension_to")
	}
	return nil
}

// attach liga los adjuntos: subidos por quien registra, listos, del modulo
// disciplinary y sin otra entidad.
func attach(ctx context.Context, q database.Querier, actor authz.Actor, id uuid.UUID, files []uuid.UUID) error {
	if len(files) == 0 {
		return nil
	}
	tag, err := q.Exec(ctx, `
		UPDATE files SET entity_type = 'disciplinary_action', entity_id = $1, updated_at = now()
		WHERE id = ANY($2) AND module = 'disciplinary' AND kind = 'attachment' AND status = 'ready'
		  AND uploaded_by_user_id = $3 AND entity_id IS NULL`, id, files, actor.UserID)
	if err != nil {
		return err
	}
	if int(tag.RowsAffected()) != len(files) {
		return ErrAttachments.WithField("body.attachment_file_ids")
	}
	return nil
}

var typeLabel = map[string]string{
	"verbal": "una llamada de atención verbal", "written": "una amonestación escrita",
	"suspension": "una suspensión", "other": "una acción disciplinaria",
}

func (s *Service) notify(ctx context.Context, q database.Querier, actor authz.Actor, email string, a ActionRead) error {
	var tenant string
	if err := q.QueryRow(ctx, `SELECT name FROM shared.tenants WHERE id = $1`, actor.TenantID).Scan(&tenant); err != nil {
		return err
	}
	if email != "" {
		data := map[string]any{
			"Name": firstName(a.EmployeeName), "TenantName": tenant, "TypeLabel": typeLabel[a.Type], "Date": longDate(a.OccurredOn),
			"Suspension": a.Type == "suspension",
		}
		if a.SuspensionFrom != nil && a.SuspensionTo != nil {
			data["SuspensionFrom"], data["SuspensionTo"] = longDate(*a.SuspensionFrom), longDate(*a.SuspensionTo)
		}
		if err := jobs.EnqueueEmail(ctx, q, &actor.TenantID, jobs.Email{To: email, Template: mail.TemplateDisciplinaryNotice, Data: data}); err != nil {
			return err
		}
	}
	return s.bus.Publish(ctx, q, events.Event{Type: EventCreated, TenantID: actor.TenantID, ActorID: &actor.UserID, Data: a})
}

// Acknowledge registra el acuse del colaborador (una sola vez, solo las suyas).
func (s *Service) Acknowledge(ctx context.Context, actor authz.Actor, id uuid.UUID, in Acknowledge) (ActionRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (ActionRead, error) {
		a, err := s.visible(ctx, q, authz.Actor{UserID: actor.UserID, TenantID: actor.TenantID}, id)
		if err != nil {
			return ActionRead{}, err
		}
		if a.AcknowledgedAt != nil {
			return ActionRead{}, ErrAcknowledged
		}
		var note *string
		if in.Note != nil && strings.TrimSpace(*in.Note) != "" {
			n := strings.TrimSpace(*in.Note)
			note = &n
		}
		if _, err := q.Exec(ctx, `UPDATE disciplinary_actions SET acknowledged_at = now(), acknowledgement_note = $2, updated_at = now()
			WHERE id = $1`, id, note); err != nil {
			return ActionRead{}, err
		}
		a, err = s.visible(ctx, q, actor, id)
		if err != nil {
			return ActionRead{}, err
		}
		if err := s.bus.Publish(ctx, q, events.Event{Type: EventAcknowledged, TenantID: actor.TenantID, ActorID: &actor.UserID, Data: a}); err != nil {
			return ActionRead{}, err
		}
		return a, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "disciplinary.acknowledged", Entity: "disciplinary_action", EntityID: &id,
			After: map[string]any{"note": note != nil},
		})
	})
}

var monthsES = [...]string{"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre", "diciembre"}

func longDate(d string) string {
	t, err := time.Parse(time.DateOnly, d)
	if err != nil {
		return d
	}
	return strconv.Itoa(t.Day()) + " de " + monthsES[t.Month()-1] + " de " + strconv.Itoa(t.Year())
}

func firstName(full string) string {
	if i := strings.IndexByte(full, ' '); i > 0 {
		return full[:i]
	}
	return full
}
