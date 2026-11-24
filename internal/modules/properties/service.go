// Package properties gestiona edificios y propiedades de cada empresa: datos,
// informacion de acceso cifrada, propietarios y colaboradores asignados
// (fase02.md §2 y §3). Todo se filtra por el alcance de quien consulta
// (authz.PropertyScope): una propiedad fuera de alcance responde 404.
package properties

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/optional"
	"github.com/smorareqs/argos-service/internal/core/paging"
	"github.com/smorareqs/argos-service/internal/core/security"
	"github.com/smorareqs/argos-service/internal/core/storage"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// AccessGate decide si una persona de alcance restringido (limpieza,
// mantenimiento) puede ver hoy la informacion de acceso de la propiedad. La
// registra el modulo de tareas (fase02.md §3: solo con tarea del dia); sin
// gate, esas personas no la ven.
type AccessGate func(ctx context.Context, q database.Querier, actor authz.Actor, propertyID uuid.UUID) (bool, error)

// Service contiene los casos de uso de propiedades y edificios.
type Service struct {
	db      *database.DB
	cipher  *security.FieldCipher
	storage *storage.Service
	log     *slog.Logger
	gate    AccessGate
	limits  Limits
	now     func() time.Time
}

// Limits verifica los limites del plan antes de crear (fase 6). Lo implementa
// billing.Service; sin el, no hay limites.
type Limits interface {
	CheckLimit(ctx context.Context, q database.Querier, tenantID uuid.UUID, kind, field string) error
}

// SetLimits engancha los limites del plan.
func (s *Service) SetLimits(l Limits) { s.limits = l }

// NewService crea el servicio.
func NewService(db *database.DB, cipher *security.FieldCipher, st *storage.Service, log *slog.Logger) *Service {
	return &Service{db: db, cipher: cipher, storage: st, log: log, now: time.Now}
}

// SetAccessGate registra la regla de "tarea hoy" del modulo de tareas.
func (s *Service) SetAccessGate(g AccessGate) {
	s.gate = g
}

// List devuelve una pagina de propiedades del alcance, ordenadas por codigo.
func (s *Service) List(ctx context.Context, actor authz.Actor, f ListFilter) ([]PropertyRead, *string, error) {
	limit := paging.Limit(f.Limit)
	key, err := paging.Decode(f.Cursor, 1)
	if err != nil {
		return nil, nil, err
	}
	var after *string
	if key != nil {
		after = &key[0]
	}
	items, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]PropertyRead, error) {
		scope, err := authz.ResolvePropertyScope(ctx, q, actor)
		if err != nil {
			return nil, err
		}
		return NewRepository(q).List(ctx, scope, f, after, limit)
	})
	if err != nil {
		return nil, nil, err
	}
	items, next := paging.Page(items, limit, func(p PropertyRead) []string { return []string{strings.ToUpper(p.Code)} })
	for i := range items {
		s.sign(ctx, actor.TenantID, &items[i])
	}
	return items, next, nil
}

// Get devuelve una propiedad del alcance.
func (s *Service) Get(ctx context.Context, actor authz.Actor, id uuid.UUID) (PropertyRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (PropertyRead, error) {
		return s.visible(ctx, q, actor, id)
	})
}

// Create da de alta una propiedad. Sin plantilla usa la de por defecto.
func (s *Service) Create(ctx context.Context, actor authz.Actor, in PropertyCreate) (PropertyRead, error) {
	d := propertyData{
		Code: in.Code, Name: in.Name, Type: in.Type, BuildingID: in.BuildingID, Address: in.Address, Zone: in.Zone,
		Lat: in.Lat, Lng: in.Lng, Bedrooms: in.Bedrooms, Bathrooms: in.Bathrooms, MaxGuests: in.MaxGuests,
		ChecklistTemplateID: in.ChecklistTemplateID, CoverFileID: in.CoverFileID, Notes: in.Notes, IsActive: true,
		AccessSet: in.AccessInfo != nil, AccessInfo: in.AccessInfo,
	}
	if err := s.prepare(&d); err != nil {
		return PropertyRead{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (PropertyRead, error) {
		repo := NewRepository(q)
		if s.limits != nil {
			if err := s.limits.CheckLimit(ctx, q, actor.TenantID, "properties", "body.code"); err != nil {
				return PropertyRead{}, err
			}
		}
		if err := checkRefs(ctx, repo, d, nil); err != nil {
			return PropertyRead{}, err
		}
		if d.ChecklistTemplateID == nil {
			def, err := repo.DefaultTemplate(ctx)
			if err != nil {
				return PropertyRead{}, err
			}
			d.ChecklistTemplateID = def
		}
		id, err := repo.Insert(ctx, d)
		if err != nil {
			return PropertyRead{}, err
		}
		created, err := s.read(ctx, repo, actor.TenantID, id)
		if err != nil {
			return PropertyRead{}, err
		}
		return created, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "property.created", Entity: "property", EntityID: &id, PropertyID: &id,
			After: snapshot(created),
		})
	})
}

// Update cambia los datos que vienen en el cuerpo.
func (s *Service) Update(ctx context.Context, actor authz.Actor, id uuid.UUID, in PropertyUpdate) (PropertyRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (PropertyRead, error) {
		repo := NewRepository(q)
		cur, err := s.visible(ctx, q, actor, id)
		if err != nil {
			return PropertyRead{}, err
		}
		d := merge(cur, in)
		if err := s.prepare(&d); err != nil {
			return PropertyRead{}, err
		}
		if err := checkRefs(ctx, repo, d, &id); err != nil {
			return PropertyRead{}, err
		}
		if err := repo.Update(ctx, id, d); err != nil {
			return PropertyRead{}, err
		}
		if cur.CoverFileID != nil && !sameID(cur.CoverFileID, d.CoverFileID) {
			if err := repo.DiscardFile(ctx, *cur.CoverFileID); err != nil {
				return PropertyRead{}, err
			}
		}
		updated, err := s.read(ctx, repo, actor.TenantID, id)
		if err != nil {
			return PropertyRead{}, err
		}
		after := snapshot(updated)
		if d.AccessSet {
			after["access_info_changed"] = true
		}
		return updated, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "property.updated", Entity: "property", EntityID: &id, PropertyID: &id,
			Before: snapshot(cur), After: after,
		})
	})
}

// Delete borra la propiedad (borrado logico: su historial se conserva).
func (s *Service) Delete(ctx context.Context, actor authz.Actor, id uuid.UUID) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		cur, err := s.visible(ctx, q, actor, id)
		if err != nil {
			return err
		}
		if err := NewRepository(q).SoftDelete(ctx, id); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "property.deleted", Entity: "property", EntityID: &id, PropertyID: &id,
			Before: snapshot(cur),
		})
	})
}

// AccessInfo descifra la informacion de acceso y deja la consulta en la
// bitacora. Limpieza y mantenimiento solo la ven con tarea del dia
// (AccessGate); administracion, supervision y seguridad, siempre.
func (s *Service) AccessInfo(ctx context.Context, actor authz.Actor, id uuid.UUID) (AccessInfoRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (AccessInfoRead, error) {
		p, err := s.visible(ctx, q, actor, id)
		if err != nil {
			return AccessInfoRead{}, err
		}
		if err := s.allowAccess(ctx, q, actor, id); err != nil {
			return AccessInfoRead{}, err
		}
		stored, err := NewRepository(q).EncryptedAccess(ctx, id)
		if err != nil {
			return AccessInfoRead{}, err
		}
		if stored == nil {
			return AccessInfoRead{}, ErrNoAccessInfo
		}
		text, err := s.cipher.Decrypt(*stored)
		if err != nil {
			return AccessInfoRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "property.access_viewed", Entity: "property", EntityID: &id, PropertyID: &id,
			After: map[string]any{"code": p.Code},
		}); err != nil {
			return AccessInfoRead{}, err
		}
		return AccessInfoRead{PropertyID: id, Text: text, UpdatedAt: p.AccessInfoUpdatedAt, ViewedAt: s.now().UTC()}, nil
	})
}

func (s *Service) allowAccess(ctx context.Context, q database.Querier, actor authz.Actor, id uuid.UUID) error {
	if !actor.Scoped || actor.Permissions[authz.PropertiesManage] || actor.Permissions[authz.TasksManage] {
		return nil
	}
	if s.gate == nil {
		return ErrAccessToday
	}
	ok, err := s.gate(ctx, q, actor, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrAccessToday
	}
	return nil
}

// visible devuelve la propiedad si esta en el alcance del actor (si no, 404).
func (s *Service) visible(ctx context.Context, q database.Querier, actor authz.Actor, id uuid.UUID) (PropertyRead, error) {
	scope, err := authz.ResolvePropertyScope(ctx, q, actor)
	if err != nil {
		return PropertyRead{}, err
	}
	if !scope.Allows(id) {
		return PropertyRead{}, ErrPropertyNotFound
	}
	return s.read(ctx, NewRepository(q), actor.TenantID, id)
}

func (s *Service) read(ctx context.Context, repo *Repository, tenantID, id uuid.UUID) (PropertyRead, error) {
	p, err := repo.ByID(ctx, id)
	if err != nil {
		return PropertyRead{}, err
	}
	if p == nil {
		return PropertyRead{}, ErrPropertyNotFound
	}
	s.sign(ctx, tenantID, p)
	return *p, nil
}

// sign firma la URL de la portada.
func (s *Service) sign(ctx context.Context, tenantID uuid.UUID, p *PropertyRead) {
	p.CoverURL = s.storage.LogoURL(ctx, tenantID, p.CoverKey)
	if p.Assigned == nil {
		p.Assigned = []PersonRef{}
	}
	if p.Owners == nil {
		p.Owners = []PersonRef{}
	}
}

// --- Validacion --------------------------------------------------------------

var codePattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{1,31}$`)

// merge aplica un PATCH sobre la propiedad actual.
func merge(cur PropertyRead, in PropertyUpdate) propertyData {
	d := propertyData{
		Code: cur.Code, Name: cur.Name, Type: cur.Type, IsActive: cur.IsActive,
		BuildingID:          optional.Apply(cur.BuildingID, in.BuildingID),
		Address:             optional.Apply(cur.Address, in.Address),
		Zone:                optional.Apply(cur.Zone, in.Zone),
		Lat:                 optional.Apply(cur.Lat, in.Lat),
		Lng:                 optional.Apply(cur.Lng, in.Lng),
		Bedrooms:            optional.Apply(cur.Bedrooms, in.Bedrooms),
		Bathrooms:           optional.Apply(cur.Bathrooms, in.Bathrooms),
		MaxGuests:           optional.Apply(cur.MaxGuests, in.MaxGuests),
		ChecklistTemplateID: optional.Apply(cur.ChecklistTemplateID, in.ChecklistTemplateID),
		CoverFileID:         optional.Apply(cur.CoverFileID, in.CoverFileID),
		Notes:               optional.Apply(cur.Notes, in.Notes),
		AccessSet:           in.AccessInfo.Sent,
		AccessInfo:          optional.Apply(nil, in.AccessInfo),
	}
	d.Code = deref(in.Code, d.Code)
	d.Name = deref(in.Name, d.Name)
	d.Type = deref(in.Type, d.Type)
	d.IsActive = deref(in.IsActive, d.IsActive)
	return d
}

// prepare normaliza y valida los datos, y cifra la informacion de acceso.
func (s *Service) prepare(d *propertyData) error {
	d.Code = strings.ToUpper(strings.TrimSpace(d.Code))
	if !codePattern.MatchString(d.Code) {
		return ErrInvalidCode.WithField("body.code")
	}
	name, err := cleanName(d.Name, "body.name")
	if err != nil {
		return err
	}
	d.Name = name
	d.Address, d.Zone, d.Notes = cleanText(d.Address), cleanText(d.Zone), cleanText(d.Notes)
	if (d.Lat == nil) != (d.Lng == nil) {
		return ErrCoordinates.WithField("body.lat")
	}
	if !d.AccessSet {
		return nil
	}
	d.AccessInfo = cleanText(d.AccessInfo)
	if d.AccessInfo == nil {
		return nil
	}
	enc, err := s.cipher.Encrypt(*d.AccessInfo)
	if err != nil {
		return err
	}
	d.AccessInfo = &enc
	return nil
}

// checkRefs valida edificio, plantilla, portada y codigo dentro de la
// transaccion.
func checkRefs(ctx context.Context, repo *Repository, d propertyData, self *uuid.UUID) error {
	taken, err := repo.CodeTaken(ctx, d.Code, self)
	if err != nil {
		return err
	}
	if taken {
		return ErrCodeTaken.WithField("body.code")
	}
	checks := []struct {
		id    *uuid.UUID
		table string
		fail  *apperr.Error
	}{
		{d.BuildingID, "buildings", ErrUnknownBuilding.WithField("body.building_id")},
		{d.ChecklistTemplateID, "checklist_templates", ErrUnknownTemplate.WithField("body.checklist_template_id")},
	}
	for _, c := range checks {
		if c.id == nil {
			continue
		}
		ok, err := repo.Exists(ctx, c.table, *c.id)
		if err != nil {
			return err
		}
		if !ok {
			return c.fail
		}
	}
	if d.CoverFileID == nil {
		return nil
	}
	ok, err := repo.ReadyCover(ctx, *d.CoverFileID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidCover.WithField("body.cover_file_id")
	}
	return nil
}

func cleanName(raw, field string) (string, error) {
	name := strings.Join(strings.Fields(raw), " ")
	if n := utf8.RuneCountInString(name); n < 2 || n > 128 {
		return "", ErrInvalidName.WithField(field)
	}
	return name, nil
}

// cleanText recorta y convierte "" en nil.
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

func deref[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}

func sameID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// snapshot es lo que queda en la bitacora de una propiedad (sin el texto de
// acceso, que nunca se registra).
func snapshot(p PropertyRead) map[string]any {
	return map[string]any{
		"code": p.Code, "name": p.Name, "type": p.Type, "building_id": p.BuildingID, "address": p.Address,
		"zone": p.Zone, "lat": p.Lat, "lng": p.Lng, "bedrooms": p.Bedrooms, "bathrooms": p.Bathrooms,
		"max_guests": p.MaxGuests, "checklist_template_id": p.ChecklistTemplateID, "cover_file_id": p.CoverFileID,
		"notes": p.Notes, "is_active": p.IsActive, "has_access_info": p.HasAccessInfo,
	}
}

// uniqueIDs quita repetidos conservando el orden.
func uniqueIDs(ids []uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}
