package maintenance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/optional"
	"github.com/smorareqs/argos-service/internal/core/paging"
	"github.com/smorareqs/argos-service/internal/modules/alerts"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

const selectAsset = `
	SELECT a.id, a.property_id, p.code AS property_code, p.name AS property_name,
	       coalesce(a.building_id, p.building_id) AS building_id, b.name AS building_name,
	       a.category, a.name, a.brand, a.model, a.serial, a.location,
	       to_char(a.installed_at, 'YYYY-MM-DD') AS installed_at, to_char(a.warranty_until, 'YYYY-MM-DD') AS warranty_until,
	       a.vendor_id, v.name AS vendor_name, a.status, a.notes, a.is_active,
	       (SELECT count(*)::int FROM maintenance_tickets t
	         WHERE t.asset_id = a.id AND t.status IN ('open', 'assigned', 'in_progress')) AS open_tickets,
	       (SELECT count(*)::int FROM maintenance_tickets t WHERE t.asset_id = a.id) AS tickets_count,
	       (SELECT coalesce(sum(t.cost), 0)::float8 FROM maintenance_tickets t
	         WHERE t.asset_id = a.id AND t.created_at >= date_trunc('year', now())) AS cost_year,
	       (SELECT to_char(min(mp.next_due_at), 'YYYY-MM-DD') FROM maintenance_plans mp
	         WHERE mp.asset_id = a.id AND mp.is_active) AS next_due_at,
	       a.created_at, a.updated_at
	FROM assets a
	LEFT JOIN properties p ON p.id = a.property_id
	LEFT JOIN buildings b ON b.id = coalesce(a.building_id, p.building_id)
	LEFT JOIN vendors v ON v.id = a.vendor_id`

// assetScope es el filtro de alcance de activos: los de sus propiedades y los
// de los edificios de esas propiedades ($1 todas, $2 ids).
const assetScope = `($1::bool OR a.property_id = ANY($2::uuid[])
	OR a.building_id IN (SELECT building_id FROM properties WHERE id = ANY($2::uuid[]) AND building_id IS NOT NULL))`

// Assets devuelve una pagina de activos del alcance, por nombre.
func (s *Service) Assets(ctx context.Context, actor authz.Actor, f AssetFilter) ([]AssetRead, *string, error) {
	limit := paging.Limit(f.Limit)
	key, err := paging.Decode(f.Cursor, 2)
	if err != nil {
		return nil, nil, err
	}
	var afterName, afterID *string
	if key != nil {
		afterName, afterID = &key[0], &key[1]
	}
	items, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]AssetRead, error) {
		scope, err := scopeOf(ctx, q, actor)
		if err != nil {
			return nil, err
		}
		all, ids := scope.SQLArgs()
		like := "%" + strings.ToLower(trim(f.Query)) + "%"
		rows, err := q.Query(ctx, selectAsset+`
			WHERE `+assetScope+`
			  AND ($3::text = '%%' OR lower(a.name) LIKE $3 OR lower(coalesce(a.brand, '')) LIKE $3
			       OR lower(coalesce(a.model, '')) LIKE $3 OR lower(coalesce(a.serial, '')) LIKE $3
			       OR lower(coalesce(a.location, '')) LIKE $3 OR lower(coalesce(p.code, '')) LIKE $3)
			  AND ($4::uuid IS NULL OR a.property_id = $4
			       OR a.building_id = (SELECT building_id FROM properties WHERE id = $4))
			  AND ($5::uuid IS NULL OR coalesce(a.building_id, p.building_id) = $5)
			  AND ($6::uuid IS NULL OR a.vendor_id = $6)
			  AND ($7::text = '' OR a.category = $7)
			  AND ($8::text = '' OR a.status = $8)
			  AND ($9::bool IS NULL OR a.is_active = $9)
			  AND ($10::text IS NULL OR (lower(a.name), a.id) > ($10::text, $11::uuid))
			ORDER BY lower(a.name), a.id
			LIMIT $12`,
			all, ids, like, f.PropertyID, f.BuildingID, f.VendorID, f.Category, f.Status, f.Active,
			afterName, afterID, limit+1)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[AssetRead])
	})
	if err != nil {
		return nil, nil, err
	}
	items, next := paging.Page(items, limit, func(a AssetRead) []string {
		return []string{strings.ToLower(a.Name), a.ID.String()}
	})
	return items, next, nil
}

// Asset devuelve la ficha del activo con planes, documentos e incidencias.
func (s *Service) Asset(ctx context.Context, actor authz.Actor, id uuid.UUID) (AssetDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (AssetDetail, error) {
		a, err := s.visibleAsset(ctx, q, actor, id)
		if err != nil {
			return AssetDetail{}, err
		}
		today, err := s.today(ctx, q)
		if err != nil {
			return AssetDetail{}, err
		}
		plans, err := listPlans(ctx, q, today, `mp.asset_id = $2`, id)
		if err != nil {
			return AssetDetail{}, err
		}
		docs, err := s.documents(ctx, q, actor.TenantID, id)
		if err != nil {
			return AssetDetail{}, err
		}
		rows, err := q.Query(ctx, selectTicket+` WHERE t.asset_id = $1 ORDER BY t.created_at DESC LIMIT 20`, id)
		if err != nil {
			return AssetDetail{}, err
		}
		tickets, err := pgx.CollectRows(rows, pgx.RowToStructByName[TicketRead])
		if err != nil {
			return AssetDetail{}, err
		}
		return AssetDetail{AssetRead: a, Plans: plans, Documents: docs, Tickets: tickets}, nil
	})
}

// CreateAsset da de alta un activo.
func (s *Service) CreateAsset(ctx context.Context, actor authz.Actor, in AssetCreate) (AssetRead, error) {
	d := assetData{
		PropertyID: in.PropertyID, BuildingID: in.BuildingID, Category: in.Category, Name: trim(in.Name),
		Brand: trimmed(in.Brand), Model: trimmed(in.Model), Serial: trimmed(in.Serial), Location: trimmed(in.Location),
		InstalledAt: in.InstalledAt, WarrantyUntil: in.WarrantyUntil, VendorID: in.VendorID,
		Status: in.Status, Notes: trimmed(in.Notes), IsActive: true,
	}
	if d.Status == "" {
		d.Status = "operational"
	}
	if err := validAsset(d); err != nil {
		return AssetRead{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (AssetRead, error) {
		if err := s.checkAssetRefs(ctx, q, actor, d); err != nil {
			return AssetRead{}, err
		}
		var id uuid.UUID
		err := q.QueryRow(ctx, `
			INSERT INTO assets (property_id, building_id, category, name, brand, model, serial, location,
			                    installed_at, warranty_until, vendor_id, status, notes)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::date, $10::date, $11, $12, $13) RETURNING id`,
			d.PropertyID, d.BuildingID, d.Category, d.Name, d.Brand, d.Model, d.Serial, d.Location,
			d.InstalledAt, d.WarrantyUntil, d.VendorID, d.Status, d.Notes).Scan(&id)
		if err != nil {
			return AssetRead{}, err
		}
		a, err := assetByID(ctx, q, id)
		if err != nil {
			return AssetRead{}, err
		}
		if err := s.syncAssetAlert(ctx, q, actor, a); err != nil {
			return AssetRead{}, err
		}
		return a, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "asset.created", Entity: "asset", EntityID: &id, PropertyID: a.PropertyID, After: d,
		})
	})
}

// UpdateAsset cambia los datos que vienen. Un cambio de estado sincroniza la
// alerta asset_failure.
func (s *Service) UpdateAsset(ctx context.Context, actor authz.Actor, id uuid.UUID, in AssetUpdate) (AssetRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (AssetRead, error) {
		cur, err := s.visibleAsset(ctx, q, actor, id)
		if err != nil {
			return AssetRead{}, err
		}
		before := assetDataOf(cur)
		d := mergeAsset(before, in)
		if err := validAsset(d); err != nil {
			return AssetRead{}, err
		}
		if err := s.checkAssetRefs(ctx, q, actor, d); err != nil {
			return AssetRead{}, err
		}
		_, err = q.Exec(ctx, `
			UPDATE assets SET property_id = $2, building_id = $3, category = $4, name = $5, brand = $6, model = $7,
			       serial = $8, location = $9, installed_at = $10::date, warranty_until = $11::date, vendor_id = $12,
			       status = $13, notes = $14, is_active = $15, updated_at = now()
			WHERE id = $1`,
			id, d.PropertyID, d.BuildingID, d.Category, d.Name, d.Brand, d.Model, d.Serial, d.Location,
			d.InstalledAt, d.WarrantyUntil, d.VendorID, d.Status, d.Notes, d.IsActive)
		if err != nil {
			return AssetRead{}, err
		}
		a, err := assetByID(ctx, q, id)
		if err != nil {
			return AssetRead{}, err
		}
		if a.Status != cur.Status || a.IsActive != cur.IsActive {
			if err := s.syncAssetAlert(ctx, q, actor, a); err != nil {
				return AssetRead{}, err
			}
		}
		action := "asset.updated"
		if a.Status != cur.Status {
			action = "asset.status_changed"
		}
		return a, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: action, Entity: "asset", EntityID: &id, PropertyID: a.PropertyID,
			Before: before, After: d,
		})
	})
}

// syncAssetAlert levanta o resuelve la alerta asset_failure segun el estado.
func (s *Service) syncAssetAlert(ctx context.Context, q database.Querier, actor authz.Actor, a AssetRead) error {
	failing := a.IsActive && a.Status != "operational"
	if !failing {
		_, err := alerts.ResolveFor(ctx, q, alerts.TypeAssetFailure, "asset", a.ID, nil)
		return err
	}
	severity := alerts.SeverityMedium
	if a.Status == "out_of_service" {
		severity = alerts.SeverityHigh
	}
	_, _, err := s.alerts.Raise(ctx, q, actor.TenantID, alerts.Raise{
		Type: alerts.TypeAssetFailure, Severity: severity, EntityType: "asset", EntityID: a.ID,
		PropertyID: a.PropertyID, Message: fmt.Sprintf("%s %s: %s", a.Name, placeOfAsset(a), statusPhrase[a.Status]),
	})
	return err
}

var statusPhrase = map[string]string{
	"needs_attention": "necesita atención",
	"in_repair":       "en reparación",
	"out_of_service":  "fuera de servicio",
}

func placeOfAsset(a AssetRead) string {
	switch {
	case a.PropertyCode != nil:
		return "en " + *a.PropertyCode
	case a.BuildingName != nil:
		return "en " + *a.BuildingName
	default:
		return ""
	}
}

// --- Documentos ----------------------------------------------------------------

// AttachDocument vincula un documento ya subido al activo.
func (s *Service) AttachDocument(ctx context.Context, actor authz.Actor, assetID uuid.UUID, in DocumentAttach) (DocumentRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (DocumentRead, error) {
		a, err := s.visibleAsset(ctx, q, actor, assetID)
		if err != nil {
			return DocumentRead{}, err
		}
		n, err := attachFiles(ctx, q, []uuid.UUID{in.FileID}, "asset_document", "asset", assetID, actor.UserID)
		if err != nil {
			return DocumentRead{}, err
		}
		if n != 1 {
			return DocumentRead{}, ErrInvalidDocument.WithField("body.file_id")
		}
		docs, err := s.documents(ctx, q, actor.TenantID, assetID)
		if err != nil {
			return DocumentRead{}, err
		}
		for _, d := range docs {
			if d.FileID == in.FileID {
				return d, audit.Record(ctx, q, audit.Entry{
					Actor: &actor.UserID, Action: "asset.document_added", Entity: "asset", EntityID: &assetID,
					PropertyID: a.PropertyID, After: map[string]any{"file_id": d.FileID, "name": d.Name},
				})
			}
		}
		return DocumentRead{}, ErrInvalidDocument
	})
}

// RemoveDocument descarta un documento del activo (storage_cleanup lo borra de S3).
func (s *Service) RemoveDocument(ctx context.Context, actor authz.Actor, assetID, fileID uuid.UUID) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		a, err := s.visibleAsset(ctx, q, actor, assetID)
		if err != nil {
			return err
		}
		tag, err := q.Exec(ctx, `
			UPDATE files SET status = 'deleted', updated_at = now()
			WHERE id = $1 AND entity_type = 'asset' AND entity_id = $2 AND status = 'ready'`, fileID, assetID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrFileNotFound
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "asset.document_removed", Entity: "asset", EntityID: &assetID,
			PropertyID: a.PropertyID, Before: map[string]any{"file_id": fileID},
		})
	})
}

func (s *Service) documents(ctx context.Context, q database.Querier, tenantID, assetID uuid.UUID) ([]DocumentRead, error) {
	rows, err := q.Query(ctx, `
		SELECT id, s3_key, coalesce(original_name, 'Documento'), content_type, size_bytes, created_at
		FROM files
		WHERE entity_type = 'asset' AND entity_id = $1 AND module = 'maintenance' AND kind = 'asset_document'
		  AND status = 'ready' AND deleted_at IS NULL
		ORDER BY created_at DESC`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DocumentRead{}
	for rows.Next() {
		var (
			d   DocumentRead
			key string
		)
		if err := rows.Scan(&d.FileID, &key, &d.Name, &d.ContentType, &d.SizeBytes, &d.CreatedAt); err != nil {
			return nil, err
		}
		d.URL = s.signed(ctx, tenantID, key)
		out = append(out, d)
	}
	return out, rows.Err()
}

// signed firma una descarga de una hora; si falla (S3 caido) deja la URL vacia.
func (s *Service) signed(ctx context.Context, tenantID uuid.UUID, key string) string {
	u, err := s.storage.PresignDownload(ctx, tenantID, key, time.Hour)
	if err != nil {
		s.log.WarnContext(ctx, "No se pudo firmar la descarga", "error", err)
		return ""
	}
	return u
}

// attachFiles vincula archivos listos del modulo maintenance subidos por
// uploader. Devuelve cuantos vinculo.
func attachFiles(ctx context.Context, q database.Querier, ids []uuid.UUID, kind, entityType string, entityID, uploader uuid.UUID) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := q.Exec(ctx, `
		UPDATE files SET entity_type = $3, entity_id = $4, updated_at = now()
		WHERE id = ANY($1::uuid[]) AND module = 'maintenance' AND kind = $2 AND status = 'ready'
		  AND deleted_at IS NULL AND uploaded_by_user_id = $5 AND (entity_id IS NULL OR entity_id = $4)`,
		ids, kind, entityType, entityID, uploader)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// --- Datos y validacion ----------------------------------------------------------

type assetData struct {
	PropertyID    *uuid.UUID `json:"property_id"`
	BuildingID    *uuid.UUID `json:"building_id"`
	Category      string     `json:"category"`
	Name          string     `json:"name"`
	Brand         *string    `json:"brand"`
	Model         *string    `json:"model"`
	Serial        *string    `json:"serial"`
	Location      *string    `json:"location"`
	InstalledAt   *string    `json:"installed_at"`
	WarrantyUntil *string    `json:"warranty_until"`
	VendorID      *uuid.UUID `json:"vendor_id"`
	Status        string     `json:"status"`
	Notes         *string    `json:"notes"`
	IsActive      bool       `json:"is_active"`
}

func assetDataOf(a AssetRead) assetData {
	d := assetData{
		PropertyID: a.PropertyID, Category: a.Category, Name: a.Name, Brand: a.Brand, Model: a.Model,
		Serial: a.Serial, Location: a.Location, InstalledAt: a.InstalledAt, WarrantyUntil: a.WarrantyUntil,
		VendorID: a.VendorID, Status: a.Status, Notes: a.Notes, IsActive: a.IsActive,
	}
	if a.PropertyID == nil {
		d.BuildingID = a.BuildingID
	}
	return d
}

func mergeAsset(d assetData, in AssetUpdate) assetData {
	d.PropertyID = optional.Apply(d.PropertyID, in.PropertyID)
	d.BuildingID = optional.Apply(d.BuildingID, in.BuildingID)
	// Mover el activo a una propiedad lo saca del edificio y viceversa.
	if in.PropertyID.Sent && !in.PropertyID.Null && !in.BuildingID.Sent {
		d.BuildingID = nil
	}
	if in.BuildingID.Sent && !in.BuildingID.Null && !in.PropertyID.Sent {
		d.PropertyID = nil
	}
	if in.Category != nil {
		d.Category = *in.Category
	}
	if in.Name != nil {
		d.Name = trim(*in.Name)
	}
	d.Brand = trimmed(optional.Apply(d.Brand, in.Brand))
	d.Model = trimmed(optional.Apply(d.Model, in.Model))
	d.Serial = trimmed(optional.Apply(d.Serial, in.Serial))
	d.Location = trimmed(optional.Apply(d.Location, in.Location))
	d.InstalledAt = optional.Apply(d.InstalledAt, in.InstalledAt)
	d.WarrantyUntil = optional.Apply(d.WarrantyUntil, in.WarrantyUntil)
	d.VendorID = optional.Apply(d.VendorID, in.VendorID)
	if in.Status != nil {
		d.Status = *in.Status
	}
	d.Notes = trimmed(optional.Apply(d.Notes, in.Notes))
	if in.IsActive != nil {
		d.IsActive = *in.IsActive
	}
	return d
}

func validAsset(d assetData) error {
	if (d.PropertyID == nil) == (d.BuildingID == nil) {
		return ErrAssetOwner.WithField("body.property_id")
	}
	if err := validDate(d.InstalledAt, "body.installed_at"); err != nil {
		return err
	}
	if err := validDate(d.WarrantyUntil, "body.warranty_until"); err != nil {
		return err
	}
	if d.InstalledAt != nil && d.WarrantyUntil != nil && *d.WarrantyUntil < *d.InstalledAt {
		return ErrWarrantyBefore.WithField("body.warranty_until")
	}
	return nil
}

// checkAssetRefs valida propiedad (del alcance), edificio y proveedor.
func (s *Service) checkAssetRefs(ctx context.Context, q database.Querier, actor authz.Actor, d assetData) error {
	if d.PropertyID != nil {
		ok, err := s.propertyInScope(ctx, q, actor, *d.PropertyID)
		if err != nil {
			return err
		}
		if !ok {
			return ErrUnknownProperty.WithField("body.property_id")
		}
	}
	if d.BuildingID != nil {
		var ok bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM buildings WHERE id = $1)`, *d.BuildingID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return ErrUnknownBuilding.WithField("body.building_id")
		}
	}
	return checkVendor(ctx, q, d.VendorID, "body.vendor_id")
}

// propertyInScope indica si la propiedad existe (no borrada) y esta en el alcance.
func (s *Service) propertyInScope(ctx context.Context, q database.Querier, actor authz.Actor, id uuid.UUID) (bool, error) {
	scope, err := scopeOf(ctx, q, actor)
	if err != nil {
		return false, err
	}
	if !scope.Allows(id) {
		return false, nil
	}
	var ok bool
	err = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM properties WHERE id = $1 AND deleted_at IS NULL)`, id).Scan(&ok)
	return ok, err
}

// visibleAsset devuelve el activo si esta en el alcance (si no, 404).
func (s *Service) visibleAsset(ctx context.Context, q database.Querier, actor authz.Actor, id uuid.UUID) (AssetRead, error) {
	scope, err := scopeOf(ctx, q, actor)
	if err != nil {
		return AssetRead{}, err
	}
	all, ids := scope.SQLArgs()
	rows, err := q.Query(ctx, selectAsset+` WHERE `+assetScope+` AND a.id = $3`, all, ids, id)
	if err != nil {
		return AssetRead{}, err
	}
	a, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[AssetRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return AssetRead{}, ErrAssetNotFound
	}
	return a, err
}

func assetByID(ctx context.Context, q database.Querier, id uuid.UUID) (AssetRead, error) {
	rows, err := q.Query(ctx, selectAsset+` WHERE a.id = $1`, id)
	if err != nil {
		return AssetRead{}, err
	}
	a, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[AssetRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return AssetRead{}, ErrAssetNotFound
	}
	return a, err
}

// AssetRef es un activo para elegir al reportar una incidencia (app B12,
// bootstrap sin conexion).
type AssetRef struct {
	ID         uuid.UUID  `db:"id" json:"id" format:"uuid"`
	PropertyID *uuid.UUID `db:"property_id" json:"property_id" format:"uuid" nullable:"true"`
	BuildingID *uuid.UUID `db:"building_id" json:"building_id" format:"uuid" nullable:"true" doc:"Solo en activos del edificio (elevadores): se ofrecen en todas sus propiedades."`
	Category   string     `db:"category" json:"category"`
	Name       string     `db:"name" json:"name"`
	Location   *string    `db:"location" json:"location" nullable:"true"`
	Status     string     `db:"status" json:"status" enum:"operational,needs_attention,in_repair,out_of_service"`
}

// AssetRefs lista los activos activos del alcance, por nombre.
func AssetRefs(ctx context.Context, q database.Querier, scope authz.PropertyScope) ([]AssetRef, error) {
	all, ids := scope.SQLArgs()
	rows, err := q.Query(ctx, `
		SELECT a.id, a.property_id, a.building_id, a.category, a.name, a.location, a.status
		FROM assets a
		WHERE a.is_active AND `+assetScope+`
		ORDER BY lower(a.name), a.id`, all, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[AssetRef])
}
