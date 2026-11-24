package reservations

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Inspecciones (fase04.md §3): la previa (pre_arrival) deja constancia de como
// se entrego la propiedad, con las fotos de la limpieza; la de salida
// (post_departure) de como la dejo el huesped. Sus hallazgos con cargo al
// deposito habilitan retenerlo; cualquiera se puede convertir en incidencia.

// CreateInspection registra una inspeccion con sus fotos.
func (s *Service) CreateInspection(ctx context.Context, actor authz.Actor, id uuid.UUID, in InspectionCreate) (ReservationDetail, error) {
	return s.withReservation(ctx, actor, id, func(q database.Querier, r ReservationRead) error {
		before := r.Status == "pending" || r.Status == "confirmed"
		after := r.Status == "checked_out"
		if (in.Kind == "pre_arrival" && !before) || (in.Kind == "post_departure" && !after) {
			return ErrInspectionTiming
		}
		var inspectionID uuid.UUID
		err := q.QueryRow(ctx, `
			INSERT INTO inspections (reservation_id, property_id, kind, performed_by, task_id, notes)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			id, r.PropertyID, in.Kind, actorID(actor), in.TaskID, trimmed(in.Notes)).Scan(&inspectionID)
		if uniqueViolation(err, "uq_inspections_reservation_id_kind") {
			return ErrInspectionTaken.WithField("body.kind")
		}
		if err != nil {
			return err
		}
		if err := attachPhotos(ctx, q, in.PhotoIDs, "inspection", inspectionID, actor.UserID, "body.photo_file_ids"); err != nil {
			return err
		}
		if err := addEvent(ctx, q, id, "inspection", actorID(actor), map[string]any{"kind": in.Kind, "photos": len(in.PhotoIDs)}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actorID(actor), Action: "inspection.created", Entity: "reservation", EntityID: &id, PropertyID: &r.PropertyID,
			After: map[string]any{"code": r.Code, "kind": in.Kind, "photos": len(in.PhotoIDs)},
		})
	})
}

// AddFinding agrega un hallazgo; con create_ticket abre su incidencia.
func (s *Service) AddFinding(ctx context.Context, actor authz.Actor, inspectionID uuid.UUID, in FindingCreate) (ReservationDetail, error) {
	rid, kind, err := s.inspectionOwner(ctx, inspectionID)
	if err != nil {
		return ReservationDetail{}, err
	}
	return s.withReservation(ctx, actor, rid, func(q database.Querier, r ReservationRead) error {
		if kind == "post_departure" && settled(r.DepositStatus) {
			return ErrDepositSettled
		}
		var ticketID *uuid.UUID
		if in.CreateTicket {
			t, err := s.maintenance.ReportFromInspection(ctx, q, actor, r.PropertyID,
				"Inspección "+r.Code+": "+strings.TrimSpace(in.Area), strings.TrimSpace(in.Description), urgencyOf(in.Severity))
			if err != nil {
				return err
			}
			ticketID = &t.ID
		}
		var findingID uuid.UUID
		if err := q.QueryRow(ctx, `
			INSERT INTO inspection_findings (inspection_id, area, description, severity, estimated_cost, charge_to_deposit, ticket_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
			inspectionID, strings.TrimSpace(in.Area), strings.TrimSpace(in.Description), in.Severity, in.EstimatedCost,
			in.ChargeToDeposit, ticketID, actorID(actor)).Scan(&findingID); err != nil {
			return err
		}
		if err := attachPhotos(ctx, q, in.PhotoIDs, "inspection_finding", findingID, actor.UserID, "body.photo_file_ids"); err != nil {
			return err
		}
		return addEvent(ctx, q, rid, "finding", actorID(actor), map[string]any{
			"area": strings.TrimSpace(in.Area), "severity": in.Severity, "charge_to_deposit": in.ChargeToDeposit,
			"ticket": ticketID != nil,
		})
	})
}

// RemoveFinding quita un hallazgo mientras el deposito no se liquide.
func (s *Service) RemoveFinding(ctx context.Context, actor authz.Actor, inspectionID, findingID uuid.UUID) (ReservationDetail, error) {
	rid, _, err := s.inspectionOwner(ctx, inspectionID)
	if err != nil {
		return ReservationDetail{}, err
	}
	return s.withReservation(ctx, actor, rid, func(q database.Querier, r ReservationRead) error {
		if settled(r.DepositStatus) {
			return ErrDepositSettled
		}
		var area string
		err := q.QueryRow(ctx, `DELETE FROM inspection_findings WHERE id = $1 AND inspection_id = $2 RETURNING area`,
			findingID, inspectionID).Scan(&area)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFindingNotFound
		}
		if err != nil {
			return err
		}
		return addEvent(ctx, q, rid, "finding_removed", actorID(actor), map[string]any{"area": area})
	})
}

func settled(status string) bool {
	return status == "returned" || status == "retained_partial" || status == "retained_full"
}

func urgencyOf(severity string) string {
	if severity == "high" || severity == "low" {
		return severity
	}
	return "medium"
}

// inspectionOwner devuelve la reserva y el tipo de una inspeccion.
func (s *Service) inspectionOwner(ctx context.Context, id uuid.UUID) (uuid.UUID, string, error) {
	type owner struct {
		ReservationID uuid.UUID `db:"reservation_id"`
		Kind          string    `db:"kind"`
	}
	o, err := database.InTxValue(ctx, s.db, func(q database.Querier) (owner, error) {
		rows, err := q.Query(ctx, `SELECT reservation_id, kind FROM inspections WHERE id = $1`, id)
		if err != nil {
			return owner{}, err
		}
		return pgx.CollectOneRow(rows, pgx.RowToStructByName[owner])
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", ErrInspectionNotFound
	}
	return o.ReservationID, o.Kind, err
}

// withReservation corre fn con la reserva del alcance bloqueada y devuelve la
// ficha actualizada.
func (s *Service) withReservation(ctx context.Context, actor authz.Actor, id uuid.UUID, fn func(q database.Querier, r ReservationRead) error) (ReservationDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (ReservationDetail, error) {
		if _, _, err := s.visible(ctx, q, actor, id); err != nil {
			return ReservationDetail{}, err
		}
		r, err := reservationByID(ctx, q, id, true)
		if err != nil {
			return ReservationDetail{}, err
		}
		if err := fn(q, r); err != nil {
			return ReservationDetail{}, err
		}
		r, v, err := s.visible(ctx, q, actor, id)
		if err != nil {
			return ReservationDetail{}, err
		}
		return s.detail(ctx, q, actor, v, r)
	})
}

// attachPhotos vincula fotos reservations/inspection_photo subidas por quien
// registra.
func attachPhotos(ctx context.Context, q database.Querier, ids []uuid.UUID, entityType string, entityID, uploader uuid.UUID, field string) error {
	if len(ids) == 0 {
		return nil
	}
	tag, err := q.Exec(ctx, `
		UPDATE files SET entity_type = $2, entity_id = $3, updated_at = now()
		WHERE id = ANY($1::uuid[]) AND module = 'reservations' AND kind = 'inspection_photo' AND status = 'ready'
		  AND deleted_at IS NULL AND uploaded_by_user_id = $4 AND entity_id IS NULL`,
		ids, entityType, entityID, uploader)
	if err != nil {
		return err
	}
	if int(tag.RowsAffected()) != len(ids) {
		return ErrInvalidFiles.WithField(field)
	}
	return nil
}

// inspections lee las inspecciones de la reserva con fotos y hallazgos.
func (s *Service) inspections(ctx context.Context, q database.Querier, tenantID, reservationID uuid.UUID) ([]InspectionRead, error) {
	rows, err := q.Query(ctx, `
		SELECT i.id, i.kind, i.performed_by, u.full_name AS performed_by_name, i.performed_at, i.task_id, i.notes
		FROM inspections i LEFT JOIN shared.users u ON u.id = i.performed_by
		WHERE i.reservation_id = $1 ORDER BY i.kind DESC`, reservationID)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[InspectionRead])
	if err != nil || len(list) == 0 {
		return []InspectionRead{}, err
	}
	findings, err := findingsOf(ctx, q, reservationID)
	if err != nil {
		return nil, err
	}
	ids := []uuid.UUID{}
	for _, i := range list {
		ids = append(ids, i.ID)
	}
	for _, f := range findings {
		ids = append(ids, f.ID)
	}
	photos, err := s.photosOf(ctx, q, tenantID, ids)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].Photos = orEmpty(photos[list[i].ID])
		list[i].Findings = []FindingRead{}
		for _, f := range findings {
			if f.InspectionID == list[i].ID {
				f.Photos = orEmpty(photos[f.ID])
				list[i].Findings = append(list[i].Findings, f)
			}
		}
	}
	return list, nil
}

func findingsOf(ctx context.Context, q database.Querier, reservationID uuid.UUID) ([]FindingRead, error) {
	rows, err := q.Query(ctx, `
		SELECT f.id, f.inspection_id, f.area, f.description, f.severity, f.estimated_cost::float8 AS estimated_cost,
		       f.charge_to_deposit, f.ticket_id, t.number AS ticket_number, f.created_at
		FROM inspection_findings f
		JOIN inspections i ON i.id = f.inspection_id
		LEFT JOIN maintenance_tickets t ON t.id = f.ticket_id
		WHERE i.reservation_id = $1 ORDER BY f.created_at, f.id`, reservationID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[FindingRead])
}

func (s *Service) photosOf(ctx context.Context, q database.Querier, tenantID uuid.UUID, owners []uuid.UUID) (map[uuid.UUID][]Photo, error) {
	type row struct {
		ID        uuid.UUID `db:"id"`
		Owner     uuid.UUID `db:"entity_id"`
		Key       string    `db:"s3_key"`
		CreatedAt time.Time `db:"created_at"`
	}
	rows, err := q.Query(ctx, `
		SELECT id, entity_id, s3_key, created_at FROM files
		WHERE entity_id = ANY($1::uuid[]) AND entity_type IN ('inspection', 'inspection_finding')
		  AND module = 'reservations' AND status = 'ready' AND deleted_at IS NULL
		ORDER BY created_at`, owners)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[row])
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID][]Photo{}
	for _, p := range list {
		u, err := s.storage.PresignDownload(ctx, tenantID, p.Key, time.Hour)
		if err != nil {
			s.log.WarnContext(ctx, "No se pudo firmar la foto", "file_id", p.ID, "error", err)
			continue
		}
		out[p.Owner] = append(out[p.Owner], Photo{FileID: p.ID, URL: u, CreatedAt: p.CreatedAt})
	}
	return out, nil
}

func orEmpty(p []Photo) []Photo {
	if p == nil {
		return []Photo{}
	}
	return p
}
