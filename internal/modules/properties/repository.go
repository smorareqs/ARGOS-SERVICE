package properties

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Repository persiste propiedades, edificios, propietarios y asignados en el
// schema del tenant (el search_path de q). Es pasivo: no contiene reglas.
type Repository struct {
	q database.Querier
}

// NewRepository crea un repositorio sobre q.
func NewRepository(q database.Querier) *Repository {
	return &Repository{q: q}
}

const selectProperty = `SELECT p.id, p.code, p.name, p.type, p.building_id, b.name AS building_name, b.zone AS building_zone,
       p.address, p.zone, p.lat::float8 AS lat, p.lng::float8 AS lng,
       p.bedrooms::int AS bedrooms, p.bathrooms::int AS bathrooms, p.max_guests::int AS max_guests,
       p.checklist_template_id, ct.name AS checklist_template_name,
       (p.access_info IS NOT NULL) AS has_access_info, p.access_info_updated_at,
       p.cover_file_id, f.s3_key AS cover_key, p.notes, p.is_active, p.created_at, p.updated_at,
       COALESCE((SELECT jsonb_agg(jsonb_build_object('user_id', u.id, 'full_name', u.full_name) ORDER BY u.full_name)
                 FROM property_assignments pa JOIN shared.users u ON u.id = pa.user_id
                 WHERE pa.property_id = p.id), '[]'::jsonb) AS assigned,
       COALESCE((SELECT jsonb_agg(jsonb_build_object('user_id', u.id, 'full_name', u.full_name) ORDER BY u.full_name)
                 FROM property_owners po JOIN shared.users u ON u.id = po.user_id
                 WHERE po.property_id = p.id), '[]'::jsonb) AS owners
FROM properties p
LEFT JOIN buildings b ON b.id = p.building_id
LEFT JOIN checklist_templates ct ON ct.id = p.checklist_template_id
LEFT JOIN files f ON f.id = p.cover_file_id AND f.status = 'ready'`

// likeEscaper escapa los comodines de LIKE en un texto de busqueda.
var likeEscaper = strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)

// accentFolder quita las tildes del texto buscado, igual que fold en SQL:
// "botan" encuentra "Botánica" sin instalar la extension unaccent.
var accentFolder = strings.NewReplacer("á", "a", "é", "e", "í", "i", "ó", "o", "ú", "u", "ü", "u", "ñ", "n",
	"à", "a", "è", "e", "ì", "i", "ò", "o", "ù", "u")

// fold compara sin mayusculas ni tildes (los mismos pares que accentFolder).
const fold = `translate(lower(%s), 'áéíóúüñàèìòù', 'aeiouunaeiou')`

var searchClause = fmt.Sprintf(`($3::text = '' OR `+fold+` LIKE '%%' || $3 || '%%' OR `+fold+` LIKE '%%' || $3 || '%%'
		       OR `+fold+` LIKE '%%' || $3 || '%%' OR `+fold+` LIKE '%%' || $3 || '%%')`,
	"p.code", "p.name", "p.zone", "b.name")

// List devuelve hasta limit+1 propiedades del alcance, por codigo.
func (r *Repository) List(ctx context.Context, scope authz.PropertyScope, f ListFilter, after *string, limit int) ([]PropertyRead, error) {
	all, ids := scope.SQLArgs()
	q := likeEscaper.Replace(accentFolder.Replace(strings.ToLower(strings.TrimSpace(f.Query))))
	rows, err := r.q.Query(ctx, selectProperty+`
		WHERE p.deleted_at IS NULL
		  AND ($1::bool OR p.id = ANY($2::uuid[]))
		  AND `+searchClause+`
		  AND ($4::uuid IS NULL OR p.building_id = $4)
		  AND ($5::bool IS NULL OR p.is_active = $5)
		  AND ($6::uuid IS NULL OR EXISTS (SELECT 1 FROM property_assignments x WHERE x.property_id = p.id AND x.user_id = $6))
		  AND ($7::uuid IS NULL OR EXISTS (SELECT 1 FROM property_owners x WHERE x.property_id = p.id AND x.user_id = $7))
		  AND ($8::text IS NULL OR upper(p.code) > $8)
		ORDER BY upper(p.code)
		LIMIT $9`,
		all, ids, q, f.BuildingID, f.Active, f.AssigneeID, f.OwnerID, after, limit+1)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[PropertyRead])
}

// ByID devuelve la propiedad (no borrada) o nil.
func (r *Repository) ByID(ctx context.Context, id uuid.UUID) (*PropertyRead, error) {
	rows, err := r.q.Query(ctx, selectProperty+` WHERE p.id = $1 AND p.deleted_at IS NULL`, id)
	if err != nil {
		return nil, err
	}
	return collectOne[PropertyRead](rows)
}

// propertyData son las columnas que se escriben.
type propertyData struct {
	Code, Name, Type    string
	BuildingID          *uuid.UUID
	Address, Zone       *string
	Lat, Lng            *float64
	Bedrooms, Bathrooms *int
	MaxGuests           *int
	ChecklistTemplateID *uuid.UUID
	CoverFileID         *uuid.UUID
	Notes               *string
	IsActive            bool
	// AccessSet indica que cambia la informacion de acceso; AccessInfo ya va
	// cifrada (nil la borra).
	AccessSet  bool
	AccessInfo *string
}

// Insert crea la propiedad y devuelve su id.
func (r *Repository) Insert(ctx context.Context, d propertyData) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.q.QueryRow(ctx, `
		INSERT INTO properties (code, name, type, building_id, address, zone, lat, lng, bedrooms, bathrooms,
		                        max_guests, checklist_template_id, cover_file_id, notes, is_active,
		                        access_info, access_info_updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16::text,
		        CASE WHEN $16::text IS NULL THEN NULL ELSE now() END)
		RETURNING id`,
		d.Code, d.Name, d.Type, d.BuildingID, d.Address, d.Zone, d.Lat, d.Lng, d.Bedrooms, d.Bathrooms,
		d.MaxGuests, d.ChecklistTemplateID, d.CoverFileID, d.Notes, d.IsActive, d.AccessInfo).Scan(&id)
	return id, err
}

// Update reescribe la propiedad.
func (r *Repository) Update(ctx context.Context, id uuid.UUID, d propertyData) error {
	_, err := r.q.Exec(ctx, `
		UPDATE properties SET code = $2, name = $3, type = $4, building_id = $5, address = $6, zone = $7,
		       lat = $8, lng = $9, bedrooms = $10, bathrooms = $11, max_guests = $12,
		       checklist_template_id = $13, cover_file_id = $14, notes = $15, is_active = $16,
		       access_info = CASE WHEN $17::bool THEN $18::text ELSE access_info END,
		       access_info_updated_at = CASE WHEN $17::bool THEN (CASE WHEN $18::text IS NULL THEN NULL ELSE now() END)
		                                     ELSE access_info_updated_at END,
		       updated_at = now()
		WHERE id = $1`,
		id, d.Code, d.Name, d.Type, d.BuildingID, d.Address, d.Zone, d.Lat, d.Lng, d.Bedrooms, d.Bathrooms,
		d.MaxGuests, d.ChecklistTemplateID, d.CoverFileID, d.Notes, d.IsActive, d.AccessSet, d.AccessInfo)
	return err
}

// SoftDelete marca la propiedad como borrada y quita sus asignaciones.
func (r *Repository) SoftDelete(ctx context.Context, id uuid.UUID) error {
	if _, err := r.q.Exec(ctx, `
		UPDATE properties SET deleted_at = now(), is_active = false, updated_at = now() WHERE id = $1`, id); err != nil {
		return err
	}
	if _, err := r.q.Exec(ctx, `DELETE FROM property_assignments WHERE property_id = $1`, id); err != nil {
		return err
	}
	_, err := r.q.Exec(ctx, `DELETE FROM property_owners WHERE property_id = $1`, id)
	return err
}

// CodeTaken indica si otra propiedad no borrada usa el codigo.
func (r *Repository) CodeTaken(ctx context.Context, code string, except *uuid.UUID) (bool, error) {
	var taken bool
	err := r.q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM properties
		               WHERE upper(code) = upper($1) AND deleted_at IS NULL AND ($2::uuid IS NULL OR id <> $2))`,
		code, except).Scan(&taken)
	return taken, err
}

// EncryptedAccess devuelve la informacion de acceso cifrada (nil si no hay).
func (r *Repository) EncryptedAccess(ctx context.Context, id uuid.UUID) (*string, error) {
	var stored *string
	err := r.q.QueryRow(ctx, `SELECT access_info FROM properties WHERE id = $1`, id).Scan(&stored)
	return stored, err
}

// Exists indica si la fila existe en la tabla (edificios y plantillas).
func (r *Repository) Exists(ctx context.Context, table string, id uuid.UUID) (bool, error) {
	var sql string
	switch table {
	case "buildings":
		sql = `SELECT EXISTS (SELECT 1 FROM buildings WHERE id = $1)`
	case "checklist_templates":
		sql = `SELECT EXISTS (SELECT 1 FROM checklist_templates WHERE id = $1)`
	default:
		return false, errors.New("tabla no permitida")
	}
	var ok bool
	err := r.q.QueryRow(ctx, sql, id).Scan(&ok)
	return ok, err
}

// DefaultTemplate devuelve la plantilla por defecto o nil.
func (r *Repository) DefaultTemplate(ctx context.Context) (*uuid.UUID, error) {
	var id uuid.UUID
	err := r.q.QueryRow(ctx, `SELECT id FROM checklist_templates WHERE is_default`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// ReadyCover indica si el archivo es una portada lista.
func (r *Repository) ReadyCover(ctx context.Context, id uuid.UUID) (bool, error) {
	var ok bool
	err := r.q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM files WHERE id = $1 AND status = 'ready' AND module = 'properties'
		               AND kind = 'cover' AND deleted_at IS NULL)`, id).Scan(&ok)
	return ok, err
}

// DiscardFile marca un archivo para que lo borre storage_cleanup.
func (r *Repository) DiscardFile(ctx context.Context, id uuid.UUID) error {
	_, err := r.q.Exec(ctx, `UPDATE files SET status = 'deleted', updated_at = now() WHERE id = $1`, id)
	return err
}

// --- Personas ---------------------------------------------------------------

// relation es la tabla que une personas y propiedades.
type relation string

const (
	relAssigned relation = "property_assignments"
	relOwner    relation = "property_owners"
)

// PeopleOf devuelve los usuarios de la relacion de la propiedad.
func (r *Repository) PeopleOf(ctx context.Context, rel relation, propertyID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := r.q.Query(ctx, `SELECT user_id FROM `+string(rel)+` WHERE property_id = $1 ORDER BY user_id`, propertyID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// PropertiesOf devuelve las propiedades (no borradas) de la persona.
func (r *Repository) PropertiesOf(ctx context.Context, rel relation, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := r.q.Query(ctx, `
		SELECT x.property_id FROM `+string(rel)+` x JOIN properties p ON p.id = x.property_id
		WHERE x.user_id = $1 AND p.deleted_at IS NULL ORDER BY p.code`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// ReplacePeople deja exactamente esas personas en la propiedad.
func (r *Repository) ReplacePeople(ctx context.Context, rel relation, propertyID uuid.UUID, users []uuid.UUID, by *uuid.UUID) error {
	if _, err := r.q.Exec(ctx, `DELETE FROM `+string(rel)+` WHERE property_id = $1 AND NOT (user_id = ANY($2::uuid[]))`,
		propertyID, users); err != nil {
		return err
	}
	return r.link(ctx, rel, []uuid.UUID{propertyID}, users, by)
}

// ReplaceProperties deja exactamente esas propiedades a la persona.
func (r *Repository) ReplaceProperties(ctx context.Context, rel relation, userID uuid.UUID, props []uuid.UUID, by *uuid.UUID) error {
	if _, err := r.q.Exec(ctx, `DELETE FROM `+string(rel)+` WHERE user_id = $1 AND NOT (property_id = ANY($2::uuid[]))`,
		userID, props); err != nil {
		return err
	}
	return r.link(ctx, rel, props, []uuid.UUID{userID}, by)
}

// link agrega cada combinacion propiedad-persona (las que ya existen quedan).
func (r *Repository) link(ctx context.Context, rel relation, props, users []uuid.UUID, by *uuid.UUID) error {
	if rel == relOwner {
		_, err := r.q.Exec(ctx, `
			INSERT INTO property_owners (property_id, user_id)
			SELECT p, u FROM unnest($1::uuid[]) p CROSS JOIN unnest($2::uuid[]) u
			ON CONFLICT DO NOTHING`, props, users)
		return err
	}
	_, err := r.q.Exec(ctx, `
		INSERT INTO property_assignments (property_id, user_id, assigned_by)
		SELECT p, u, $3 FROM unnest($1::uuid[]) p CROSS JOIN unnest($2::uuid[]) u
		ON CONFLICT DO NOTHING`, props, users, by)
	return err
}

// CountMembers cuenta cuantos de los usuarios pertenecen a la empresa.
func (r *Repository) CountMembers(ctx context.Context, tenantID uuid.UUID, users []uuid.UUID) (int, error) {
	var n int
	err := r.q.QueryRow(ctx, `
		SELECT count(DISTINCT user_id) FROM shared.memberships WHERE tenant_id = $1 AND user_id = ANY($2::uuid[])`,
		tenantID, users).Scan(&n)
	return n, err
}

// CountProperties cuenta cuantas de las propiedades existen (no borradas).
func (r *Repository) CountProperties(ctx context.Context, ids []uuid.UUID) (int, error) {
	var n int
	err := r.q.QueryRow(ctx, `SELECT count(*) FROM properties WHERE id = ANY($1::uuid[]) AND deleted_at IS NULL`,
		ids).Scan(&n)
	return n, err
}

func collectOne[T any](rows pgx.Rows) (*T, error) {
	v, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[T])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}
