package maintenance

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/optional"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

var trim = strings.TrimSpace

// Los proveedores no dependen del alcance: son de la empresa. Sus contadores
// si cuentan todo (quien los ve tiene maintenance.read).
const selectVendor = `
	SELECT v.id, v.name, v.service_type, v.contact_name, v.phone, v.email, v.notes, v.is_active,
	       (SELECT count(*)::int FROM assets a WHERE a.vendor_id = v.id AND a.is_active) AS assets_count,
	       (SELECT count(*)::int FROM maintenance_tickets t
	         WHERE t.vendor_id = v.id AND t.status IN ('open', 'assigned', 'in_progress')) AS open_tickets,
	       (SELECT count(*)::int FROM maintenance_tickets t
	         WHERE t.vendor_id = v.id AND t.status IN ('resolved', 'closed')) AS tickets_done,
	       (SELECT coalesce(sum(t.cost), 0)::float8 FROM maintenance_tickets t
	         WHERE t.vendor_id = v.id AND t.created_at >= date_trunc('year', now())) AS spend_year,
	       (SELECT avg(extract(epoch FROM t.resolved_at - t.created_at) / 3600)::float8 FROM maintenance_tickets t
	         WHERE t.vendor_id = v.id AND t.resolved_at IS NOT NULL) AS avg_hours,
	       v.created_at, v.updated_at
	FROM vendors v`

// Vendors lista los proveedores por nombre (activos primero).
func (s *Service) Vendors(ctx context.Context, active *bool) ([]VendorRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]VendorRead, error) {
		rows, err := q.Query(ctx, selectVendor+`
			WHERE ($1::bool IS NULL OR v.is_active = $1)
			ORDER BY v.is_active DESC, lower(v.name)`, active)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[VendorRead])
	})
}

// Vendor devuelve un proveedor.
func (s *Service) Vendor(ctx context.Context, id uuid.UUID) (VendorRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (VendorRead, error) {
		return vendorByID(ctx, q, id)
	})
}

// CreateVendor da de alta un proveedor.
func (s *Service) CreateVendor(ctx context.Context, actor authz.Actor, in VendorCreate) (VendorRead, error) {
	d := vendorData{
		Name: trim(in.Name), ServiceType: trimmed(in.ServiceType), ContactName: trimmed(in.ContactName),
		Phone: trimmed(in.Phone), Email: trimmed(in.Email), Notes: trimmed(in.Notes), IsActive: true,
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (VendorRead, error) {
		var id uuid.UUID
		err := q.QueryRow(ctx, `
			INSERT INTO vendors (name, service_type, contact_name, phone, email, notes)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			d.Name, d.ServiceType, d.ContactName, d.Phone, d.Email, d.Notes).Scan(&id)
		if database.IsUniqueViolation(err) {
			return VendorRead{}, ErrVendorNameTaken.WithField("body.name")
		}
		if err != nil {
			return VendorRead{}, err
		}
		v, err := vendorByID(ctx, q, id)
		if err != nil {
			return VendorRead{}, err
		}
		return v, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "vendor.created", Entity: "vendor", EntityID: &id, After: d,
		})
	})
}

// UpdateVendor cambia los datos que vienen.
func (s *Service) UpdateVendor(ctx context.Context, actor authz.Actor, id uuid.UUID, in VendorUpdate) (VendorRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (VendorRead, error) {
		cur, err := vendorByID(ctx, q, id)
		if err != nil {
			return VendorRead{}, err
		}
		before := vendorData{cur.Name, cur.ServiceType, cur.ContactName, cur.Phone, cur.Email, cur.Notes, cur.IsActive}
		d := before
		if in.Name != nil {
			d.Name = trim(*in.Name)
		}
		d.ServiceType = trimmed(optional.Apply(d.ServiceType, in.ServiceType))
		d.ContactName = trimmed(optional.Apply(d.ContactName, in.ContactName))
		d.Phone = trimmed(optional.Apply(d.Phone, in.Phone))
		d.Email = trimmed(optional.Apply(d.Email, in.Email))
		d.Notes = trimmed(optional.Apply(d.Notes, in.Notes))
		if in.IsActive != nil {
			d.IsActive = *in.IsActive
		}
		_, err = q.Exec(ctx, `
			UPDATE vendors SET name = $2, service_type = $3, contact_name = $4, phone = $5, email = $6,
			       notes = $7, is_active = $8, updated_at = now()
			WHERE id = $1`, id, d.Name, d.ServiceType, d.ContactName, d.Phone, d.Email, d.Notes, d.IsActive)
		if database.IsUniqueViolation(err) {
			return VendorRead{}, ErrVendorNameTaken.WithField("body.name")
		}
		if err != nil {
			return VendorRead{}, err
		}
		v, err := vendorByID(ctx, q, id)
		if err != nil {
			return VendorRead{}, err
		}
		return v, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "vendor.updated", Entity: "vendor", EntityID: &id, Before: before, After: d,
		})
	})
}

type vendorData struct {
	Name        string  `json:"name"`
	ServiceType *string `json:"service_type"`
	ContactName *string `json:"contact_name"`
	Phone       *string `json:"phone"`
	Email       *string `json:"email"`
	Notes       *string `json:"notes"`
	IsActive    bool    `json:"is_active"`
}

func vendorByID(ctx context.Context, q database.Querier, id uuid.UUID) (VendorRead, error) {
	rows, err := q.Query(ctx, selectVendor+` WHERE v.id = $1`, id)
	if err != nil {
		return VendorRead{}, err
	}
	v, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[VendorRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return VendorRead{}, ErrVendorNotFound
	}
	return v, err
}

// checkVendor valida que el proveedor exista y este activo.
func checkVendor(ctx context.Context, q database.Querier, id *uuid.UUID, field string) error {
	if id == nil {
		return nil
	}
	var ok bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM vendors WHERE id = $1 AND is_active)`, *id).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrUnknownVendor.WithField(field)
	}
	return nil
}
