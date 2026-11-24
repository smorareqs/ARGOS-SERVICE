// Package checklists gestiona las plantillas de checklist de limpieza (W-C4):
// secciones (Habitacion 1, Bano, Cocina...) con items que pueden exigir foto o
// antes y despues. Las tareas copian la plantilla al iniciar (fase02.md §3), asi
// que editarla no cambia las tareas en curso.
package checklists

import (
	"context"
	"log/slog"
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
	ErrTemplateNotFound = apperr.New(apperr.KindNotFound, "La plantilla no existe.")
	ErrInvalidName      = apperr.New(apperr.KindValidation, "El nombre debe tener entre 2 y 128 caracteres.")
	ErrNameTaken        = apperr.New(apperr.KindValidation, "Ya hay una plantilla con ese nombre.")
	ErrNoItems          = apperr.New(apperr.KindValidation, "Agrega al menos un punto a revisar.")
	ErrInvalidItem      = apperr.New(apperr.KindValidation, "Cada punto necesita sección y texto.")
)

// ItemRead es un punto de la plantilla.
type ItemRead struct {
	ID                  uuid.UUID `json:"id" format:"uuid"`
	Section             string    `json:"section"`
	Position            int       `json:"position" doc:"Orden global dentro de la plantilla, desde 1."`
	Text                string    `json:"text"`
	RequiresPhoto       bool      `json:"requires_photo"`
	RequiresBeforeAfter bool      `json:"requires_before_after" doc:"Exige foto de antes y de despues (implica requires_photo)."`
}

// TemplateRead es una plantilla con sus puntos en orden.
type TemplateRead struct {
	ID              uuid.UUID  `db:"id" json:"id" format:"uuid"`
	Name            string     `db:"name" json:"name"`
	Description     *string    `db:"description" json:"description" nullable:"true"`
	IsDefault       bool       `db:"is_default" json:"is_default" doc:"La usan las propiedades nuevas sin plantilla."`
	PropertiesCount int        `db:"properties_count" json:"properties_count"`
	Items           []ItemRead `db:"items" json:"items"`
	CreatedAt       time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time  `db:"updated_at" json:"updated_at"`
}

// ItemInput es un punto al crear o editar (el orden de la lista es el orden).
type ItemInput struct {
	_                   struct{} `additionalProperties:"true"`
	Section             string   `json:"section" minLength:"1" maxLength:"64"`
	Text                string   `json:"text" minLength:"1" maxLength:"255"`
	RequiresPhoto       bool     `json:"requires_photo,omitempty"`
	RequiresBeforeAfter bool     `json:"requires_before_after,omitempty"`
}

// TemplateCreate es el cuerpo de POST /checklist-templates.
type TemplateCreate struct {
	_           struct{}    `additionalProperties:"true"`
	Name        string      `json:"name" minLength:"1" maxLength:"128"`
	Description *string     `json:"description,omitempty" maxLength:"500" nullable:"true"`
	IsDefault   bool        `json:"is_default,omitempty"`
	Items       []ItemInput `json:"items" maxItems:"300"`
}

// TemplateUpdate es el cuerpo de PATCH /checklist-templates/{id}. Items, si
// viene, reemplaza todos los puntos.
type TemplateUpdate struct {
	_           struct{}               `additionalProperties:"true"`
	Name        *string                `json:"name,omitempty" minLength:"1" maxLength:"128"`
	Description optional.Value[string] `json:"description,omitzero" maxLength:"500"`
	IsDefault   *bool                  `json:"is_default,omitempty"`
	Items       *[]ItemInput           `json:"items,omitempty" maxItems:"300"`
}

const selectTemplate = `SELECT t.id, t.name, t.description, t.is_default, t.created_at, t.updated_at,
       (SELECT count(*) FROM properties p WHERE p.checklist_template_id = t.id AND p.deleted_at IS NULL)::int
         AS properties_count,
       COALESCE((SELECT jsonb_agg(jsonb_build_object(
                    'id', i.id, 'section', i.section, 'position', i.position, 'text', i.text,
                    'requires_photo', i.requires_photo, 'requires_before_after', i.requires_before_after)
                  ORDER BY i.position)
                 FROM checklist_template_items i WHERE i.template_id = t.id), '[]'::jsonb) AS items
FROM checklist_templates t`

// Service contiene los casos de uso de plantillas.
type Service struct {
	db  *database.DB
	log *slog.Logger
}

// NewService crea el servicio.
func NewService(db *database.DB, log *slog.Logger) *Service {
	return &Service{db: db, log: log}
}

// List devuelve las plantillas, la de por defecto primero.
func (s *Service) List(ctx context.Context) ([]TemplateRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]TemplateRead, error) {
		return List(ctx, q, nil)
	})
}

// List devuelve las plantillas (todas si ids es nil). La usa tambien sync.
func List(ctx context.Context, q database.Querier, ids []uuid.UUID) ([]TemplateRead, error) {
	rows, err := q.Query(ctx, selectTemplate+`
		WHERE $1::uuid[] IS NULL OR t.id = ANY($1::uuid[])
		ORDER BY t.is_default DESC, lower(t.name)`, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[TemplateRead])
}

// Get devuelve una plantilla.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (TemplateRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TemplateRead, error) {
		return byID(ctx, q, id)
	})
}

// Create da de alta una plantilla.
func (s *Service) Create(ctx context.Context, actor authz.Actor, in TemplateCreate) (TemplateRead, error) {
	name, err := cleanName(in.Name)
	if err != nil {
		return TemplateRead{}, err
	}
	items, err := cleanItems(in.Items)
	if err != nil {
		return TemplateRead{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TemplateRead, error) {
		if err := uniqueName(ctx, q, name, nil); err != nil {
			return TemplateRead{}, err
		}
		if in.IsDefault {
			if err := clearDefault(ctx, q); err != nil {
				return TemplateRead{}, err
			}
		}
		var id uuid.UUID
		if err := q.QueryRow(ctx, `
			INSERT INTO checklist_templates (name, description, is_default) VALUES ($1, $2, $3) RETURNING id`,
			name, cleanText(in.Description), in.IsDefault).Scan(&id); err != nil {
			return TemplateRead{}, err
		}
		if err := replaceItems(ctx, q, id, items); err != nil {
			return TemplateRead{}, err
		}
		t, err := byID(ctx, q, id)
		if err != nil {
			return TemplateRead{}, err
		}
		return t, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "checklist_template.created", Entity: "checklist_template", EntityID: &id,
			After: snapshot(t),
		})
	})
}

// Update cambia los datos que vienen; items reemplaza todos los puntos.
func (s *Service) Update(ctx context.Context, actor authz.Actor, id uuid.UUID, in TemplateUpdate) (TemplateRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (TemplateRead, error) {
		cur, err := byID(ctx, q, id)
		if err != nil {
			return TemplateRead{}, err
		}
		if err := s.apply(ctx, q, cur, in); err != nil {
			return TemplateRead{}, err
		}
		t, err := byID(ctx, q, id)
		if err != nil {
			return TemplateRead{}, err
		}
		return t, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "checklist_template.updated", Entity: "checklist_template", EntityID: &id,
			Before: snapshot(cur), After: snapshot(t),
		})
	})
}

func (s *Service) apply(ctx context.Context, q database.Querier, cur TemplateRead, in TemplateUpdate) error {
	name := cur.Name
	if in.Name != nil {
		var err error
		if name, err = cleanName(*in.Name); err != nil {
			return err
		}
		if err := uniqueName(ctx, q, name, &cur.ID); err != nil {
			return err
		}
	}
	isDefault := cur.IsDefault
	if in.IsDefault != nil && *in.IsDefault != cur.IsDefault {
		isDefault = *in.IsDefault
		if isDefault {
			if err := clearDefault(ctx, q); err != nil {
				return err
			}
		}
	}
	if _, err := q.Exec(ctx, `
		UPDATE checklist_templates SET name = $2, description = $3, is_default = $4, updated_at = now() WHERE id = $1`,
		cur.ID, name, cleanText(optional.Apply(cur.Description, in.Description)), isDefault); err != nil {
		return err
	}
	if in.Items == nil {
		return nil
	}
	items, err := cleanItems(*in.Items)
	if err != nil {
		return err
	}
	return replaceItems(ctx, q, cur.ID, items)
}

// Delete borra la plantilla; las propiedades que la usaban quedan sin plantilla
// (las tareas ya iniciadas conservan su copia).
func (s *Service) Delete(ctx context.Context, actor authz.Actor, id uuid.UUID) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		cur, err := byID(ctx, q, id)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `DELETE FROM checklist_templates WHERE id = $1`, id); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "checklist_template.deleted", Entity: "checklist_template", EntityID: &id,
			Before: snapshot(cur),
		})
	})
}

func byID(ctx context.Context, q database.Querier, id uuid.UUID) (TemplateRead, error) {
	list, err := List(ctx, q, []uuid.UUID{id})
	if err != nil {
		return TemplateRead{}, err
	}
	if len(list) == 0 {
		return TemplateRead{}, ErrTemplateNotFound
	}
	return list[0], nil
}

func replaceItems(ctx context.Context, q database.Querier, templateID uuid.UUID, items []ItemInput) error {
	if _, err := q.Exec(ctx, `DELETE FROM checklist_template_items WHERE template_id = $1`, templateID); err != nil {
		return err
	}
	sections, texts := make([]string, len(items)), make([]string, len(items))
	photos, pairs := make([]bool, len(items)), make([]bool, len(items))
	for i, it := range items {
		sections[i], texts[i], photos[i], pairs[i] = it.Section, it.Text, it.RequiresPhoto, it.RequiresBeforeAfter
	}
	_, err := q.Exec(ctx, `
		INSERT INTO checklist_template_items (template_id, section, position, text, requires_photo, requires_before_after)
		SELECT $1, s, n, t, p, b
		FROM unnest($2::text[], $3::text[], $4::bool[], $5::bool[]) WITH ORDINALITY AS x(s, t, p, b, n)`,
		templateID, sections, texts, photos, pairs)
	return err
}

func clearDefault(ctx context.Context, q database.Querier) error {
	_, err := q.Exec(ctx, `UPDATE checklist_templates SET is_default = false, updated_at = now() WHERE is_default`)
	return err
}

func uniqueName(ctx context.Context, q database.Querier, name string, except *uuid.UUID) error {
	var taken bool
	if err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM checklist_templates WHERE lower(name) = lower($1) AND ($2::uuid IS NULL OR id <> $2))`,
		name, except).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return ErrNameTaken.WithField("body.name")
	}
	return nil
}

func cleanName(raw string) (string, error) {
	name := strings.Join(strings.Fields(raw), " ")
	if n := utf8.RuneCountInString(name); n < 2 || n > 128 {
		return "", ErrInvalidName.WithField("body.name")
	}
	return name, nil
}

// cleanItems recorta textos y hace que "antes y despues" implique foto.
func cleanItems(in []ItemInput) ([]ItemInput, error) {
	if len(in) == 0 {
		return nil, ErrNoItems.WithField("body.items")
	}
	out := make([]ItemInput, 0, len(in))
	for _, it := range in {
		section := strings.Join(strings.Fields(it.Section), " ")
		text := strings.Join(strings.Fields(it.Text), " ")
		if section == "" || text == "" {
			return nil, ErrInvalidItem.WithField("body.items")
		}
		out = append(out, ItemInput{
			Section: section, Text: text,
			RequiresPhoto: it.RequiresPhoto || it.RequiresBeforeAfter, RequiresBeforeAfter: it.RequiresBeforeAfter,
		})
	}
	return out, nil
}

func cleanText(p *string) *string {
	if p == nil {
		return nil
	}
	t := strings.TrimSpace(*p)
	if t == "" {
		return nil
	}
	return &t
}

func snapshot(t TemplateRead) map[string]any {
	sections := []string{}
	for _, it := range t.Items {
		if len(sections) == 0 || sections[len(sections)-1] != it.Section {
			sections = append(sections, it.Section)
		}
	}
	return map[string]any{"name": t.Name, "is_default": t.IsDefault, "items": len(t.Items), "sections": sections}
}
