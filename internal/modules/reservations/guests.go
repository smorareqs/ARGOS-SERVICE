package reservations

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/optional"
	"github.com/smorareqs/argos-service/internal/core/paging"
	"github.com/smorareqs/argos-service/internal/core/storage"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// RetentionDays es cuanto se guardan las fotos del documento despues de la
// salida (fase04.md §3). El numero de documento se conserva.
const RetentionDays = 90

const selectGuest = `
	SELECT g.id, g.full_name, g.email, g.phone, g.document_type, g.document_number, g.nationality, g.notes,
	       g.blacklisted, g.blacklist_reason, g.created_at, g.updated_at,
	       (SELECT count(*)::int FROM reservations r WHERE r.primary_guest_id = g.id AND r.status = 'checked_out') AS stays,
	       (SELECT to_char(max(r.check_out_date), 'YYYY-MM-DD') FROM reservations r
	         WHERE r.primary_guest_id = g.id AND r.status = 'checked_out') AS last_stay,
	       (SELECT to_char(min(r.check_in_date), 'YYYY-MM-DD') FROM reservations r
	         WHERE r.primary_guest_id = g.id AND r.status IN ('pending', 'confirmed')) AS next_stay
	FROM guests g`

// Guests lista el directorio (W-H5): busca por nombre, correo o documento.
func (s *Service) Guests(ctx context.Context, _ authz.Actor, f GuestFilter) ([]GuestRead, *string, error) {
	limit := paging.Limit(f.Limit)
	key, err := paging.Decode(f.Cursor, 2)
	if err != nil {
		return nil, nil, err
	}
	var afterName, afterID *string
	if key != nil {
		afterName, afterID = &key[0], &key[1]
	}
	search := ""
	if term := strings.ToLower(strings.TrimSpace(f.Query)); term != "" {
		search = "%" + term + "%"
	}
	items, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]GuestRead, error) {
		rows, err := q.Query(ctx, selectGuest+`
			WHERE ($1::text = '' OR lower(g.full_name) LIKE $1 OR lower(coalesce(g.email, '')) LIKE $1
			       OR lower(coalesce(g.document_number, '')) LIKE $1 OR coalesce(g.phone, '') LIKE $1)
			  AND ($2::bool IS NULL OR g.blacklisted = $2)
			  AND ($3::text IS NULL OR (lower(g.full_name), g.id) > ($3::text, $4::uuid))
			ORDER BY lower(g.full_name), g.id
			LIMIT $5`, search, f.Blacklisted, afterName, afterID, limit+1)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[GuestRead])
	})
	if err != nil {
		return nil, nil, err
	}
	items, next := paging.Page(items, limit, func(g GuestRead) []string {
		return []string{strings.ToLower(g.FullName), g.ID.String()}
	})
	return items, next, nil
}

// Guest devuelve la ficha: estancias y fotos vigentes del documento.
func (s *Service) Guest(ctx context.Context, actor authz.Actor, id uuid.UUID) (GuestDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (GuestDetail, error) {
		g, err := guestByID(ctx, q, id)
		if err != nil {
			return GuestDetail{}, err
		}
		out := GuestDetail{GuestRead: g}
		if out.Documents, err = guestDocuments(ctx, q, id); err != nil {
			return out, err
		}
		t, err := s.tenant(ctx, q)
		if err != nil {
			return out, err
		}
		v, err := s.visibility(ctx, q, actor, t.Today)
		if err != nil {
			return out, err
		}
		rows, err := q.Query(ctx, selectReservation+` WHERE `+visibleSQL+`
			AND (r.primary_guest_id = $5 OR EXISTS (SELECT 1 FROM reservation_guests x WHERE x.reservation_id = r.id AND x.guest_id = $5))
			ORDER BY r.check_in_date DESC LIMIT 50`, append(v.args(), id)...)
		if err != nil {
			return out, err
		}
		list, err := pgx.CollectRows(rows, pgx.RowToStructByName[ReservationRead])
		out.Reservations = redactList(list, true)
		return out, err
	})
}

// CreateGuest da de alta un huesped en el directorio.
func (s *Service) CreateGuest(ctx context.Context, actor authz.Actor, in GuestCreate) (GuestRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (GuestRead, error) {
		id, existing, err := createGuest(ctx, q, in, "body")
		if err != nil {
			return GuestRead{}, err
		}
		if existing {
			return GuestRead{}, ErrDocumentTaken.WithField("body.document_number")
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: actorID(actor), Action: "guest.created", Entity: "guest", EntityID: &id,
			After: map[string]any{"full_name": strings.TrimSpace(in.FullName)},
		}); err != nil {
			return GuestRead{}, err
		}
		return guestByID(ctx, q, id)
	})
}

// UpdateGuest corrige datos o registra el documento. La lista negra pide
// guests.manage (quien solo hace check-ins no la cambia).
func (s *Service) UpdateGuest(ctx context.Context, actor authz.Actor, id uuid.UUID, in GuestUpdate) (GuestRead, error) {
	if (in.Blacklisted != nil || in.BlacklistReason.Sent) && !actor.Permissions[authz.GuestsManage] {
		return GuestRead{}, ErrBlacklistPermission
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (GuestRead, error) {
		cur, err := guestByID(ctx, q, id)
		if err != nil {
			return GuestRead{}, err
		}
		v, fields := mergeGuest(cur, in)
		if len(fields) == 0 {
			return cur, nil
		}
		if err := normalizeDocument(&v.DocumentType, &v.DocumentNumber, "body"); err != nil {
			return GuestRead{}, err
		}
		_, err = q.Exec(ctx, `
			UPDATE guests SET full_name = $2, email = $3, phone = $4, document_type = $5, document_number = $6,
			       nationality = $7, notes = $8, blacklisted = $9, blacklist_reason = $10, updated_at = now()
			WHERE id = $1`, id, v.FullName, v.Email, v.Phone, v.DocumentType, v.DocumentNumber, v.Nationality, v.Notes,
			v.Blacklisted, v.BlacklistReason)
		if uniqueViolation(err, "uq_guests_document") {
			return GuestRead{}, ErrDocumentTaken.WithField("body.document_number")
		}
		if err != nil {
			return GuestRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: actorID(actor), Action: guestAction(fields), Entity: "guest", EntityID: &id,
			After: map[string]any{"fields": fields},
		}); err != nil {
			return GuestRead{}, err
		}
		return guestByID(ctx, q, id)
	})
}

func guestAction(fields []string) string {
	for _, f := range fields {
		if f == "blacklisted" {
			return "guest.blacklist_changed"
		}
	}
	return "guest.updated"
}

func mergeGuest(cur GuestRead, in GuestUpdate) (GuestRead, []string) {
	fields := []string{}
	if in.FullName != nil && strings.TrimSpace(*in.FullName) != cur.FullName {
		cur.FullName, fields = strings.TrimSpace(*in.FullName), append(fields, "full_name")
	}
	for _, f := range []struct {
		name string
		dst  **string
		in   optional.Value[string]
	}{
		{"email", &cur.Email, in.Email},
		{"phone", &cur.Phone, in.Phone},
		{"document_type", &cur.DocumentType, in.DocumentType},
		{"document_number", &cur.DocumentNumber, in.DocumentNumber},
		{"nationality", &cur.Nationality, in.Nationality},
		{"notes", &cur.Notes, in.Notes},
		{"blacklist_reason", &cur.BlacklistReason, in.BlacklistReason},
	} {
		if f.in.Sent {
			*f.dst, fields = trimmed(optional.Apply(*f.dst, f.in)), append(fields, f.name)
		}
	}
	if in.Blacklisted != nil && *in.Blacklisted != cur.Blacklisted {
		cur.Blacklisted, fields = *in.Blacklisted, append(fields, "blacklisted")
	}
	if !cur.Blacklisted {
		cur.BlacklistReason = nil
	}
	if cur.Nationality != nil {
		up := strings.ToUpper(*cur.Nationality)
		cur.Nationality = &up
	}
	return cur, fields
}

// createGuest inserta un huesped. Si su documento ya existe devuelve el
// existente (existing = true) sin cambiarlo.
func createGuest(ctx context.Context, q database.Querier, in GuestCreate, prefix string) (uuid.UUID, bool, error) {
	docType, docNumber := trimmed(in.DocumentType), trimmed(in.DocumentNumber)
	if err := normalizeDocument(&docType, &docNumber, prefix); err != nil {
		return uuid.Nil, false, err
	}
	if docNumber != nil {
		var id uuid.UUID
		err := q.QueryRow(ctx, `SELECT id FROM guests WHERE document_type = $1 AND document_number = $2`, docType, docNumber).Scan(&id)
		if err == nil {
			return id, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, err
		}
	}
	nationality := trimmed(in.Nationality)
	if nationality != nil {
		up := strings.ToUpper(*nationality)
		nationality = &up
	}
	email := trimmed(in.Email)
	if email != nil {
		low := strings.ToLower(*email)
		email = &low
	}
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO guests (full_name, email, phone, document_type, document_number, nationality, notes)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		strings.TrimSpace(in.FullName), email, trimmed(in.Phone), docType, docNumber, nationality, trimmed(in.Notes)).Scan(&id)
	return id, false, err
}

var (
	dpiPattern      = regexp.MustCompile(`^\d{13}$`)
	passportPattern = regexp.MustCompile(`^[A-Z0-9]{5,20}$`)
)

// normalizeDocument valida el par tipo/numero: DPI de 13 digitos (se quitan
// espacios y guiones) o pasaporte de 5 a 20 letras y numeros.
func normalizeDocument(docType, number **string, prefix string) error {
	if (*docType == nil) != (*number == nil) {
		return ErrDocumentPair.WithField(prefix + ".document_number")
	}
	if *number == nil {
		return nil
	}
	n := strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(**number))
	switch **docType {
	case "dpi":
		if !dpiPattern.MatchString(n) {
			return ErrInvalidDocument.WithField(prefix + ".document_number")
		}
	case "passport":
		if !passportPattern.MatchString(n) {
			return ErrInvalidDocument.WithField(prefix + ".document_number")
		}
	default:
		return ErrDocumentPair.WithField(prefix + ".document_type")
	}
	*number = &n
	return nil
}

// resolveGuest devuelve el huesped principal de una reserva nueva: uno
// existente o uno nuevo (que reusa el existente si su documento ya esta).
func (s *Service) resolveGuest(ctx context.Context, q database.Querier, actor authz.Actor, id *uuid.UUID, in *GuestCreate) (*uuid.UUID, error) {
	switch {
	case id != nil:
		g, err := guestByID(ctx, q, *id)
		if errors.Is(err, ErrGuestNotFound) {
			return nil, ErrGuestNotFound.WithField("body.primary_guest_id")
		}
		if err != nil {
			return nil, err
		}
		if g.Blacklisted {
			return nil, blacklisted(g, "body.primary_guest_id")
		}
		return id, nil
	case in != nil:
		gid, existing, err := createGuest(ctx, q, *in, "body.guest")
		if err != nil {
			return nil, err
		}
		if existing {
			g, err := guestByID(ctx, q, gid)
			if err != nil {
				return nil, err
			}
			if g.Blacklisted {
				return nil, blacklisted(g, "body.guest.document_number")
			}
		} else if err := audit.Record(ctx, q, audit.Entry{
			Actor: actorID(actor), Action: "guest.created", Entity: "guest", EntityID: &gid,
			After: map[string]any{"full_name": strings.TrimSpace(in.FullName)},
		}); err != nil {
			return nil, err
		}
		return &gid, nil
	}
	return nil, nil
}

func blacklisted(g GuestRead, field string) error {
	msg := "Este huésped está en lista negra."
	if g.BlacklistReason != nil {
		msg = "Este huésped está en lista negra: " + *g.BlacklistReason
	}
	return ErrGuestBlacklisted.WithMessage(msg).WithField(field)
}

func guestByID(ctx context.Context, q database.Querier, id uuid.UUID) (GuestRead, error) {
	rows, err := q.Query(ctx, selectGuest+` WHERE g.id = $1`, id)
	if err != nil {
		return GuestRead{}, err
	}
	g, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[GuestRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return GuestRead{}, ErrGuestNotFound
	}
	return g, err
}

// --- Huespedes de una reserva --------------------------------------------------------

func reservationGuests(ctx context.Context, q database.Querier, reservationID uuid.UUID, contact bool) ([]ReservationGuest, error) {
	type rowT struct {
		GuestRead
		IsPrimary bool `db:"is_primary"`
		Front     bool `db:"has_front"`
		Back      bool `db:"has_back"`
	}
	rows, err := q.Query(ctx, `
		SELECT x.*, rg.is_primary,
		       EXISTS (SELECT 1 FROM files f WHERE f.entity_type = 'guest' AND f.entity_id = x.id AND f.kind = 'id_front'
		               AND f.status = 'ready' AND f.deleted_at IS NULL) AS has_front,
		       EXISTS (SELECT 1 FROM files f WHERE f.entity_type = 'guest' AND f.entity_id = x.id AND f.kind = 'id_back'
		               AND f.status = 'ready' AND f.deleted_at IS NULL) AS has_back
		FROM reservation_guests rg
		JOIN (`+selectGuest+`) x ON x.id = rg.guest_id
		WHERE rg.reservation_id = $1
		ORDER BY rg.is_primary DESC, rg.created_at`, reservationID)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[rowT])
	if err != nil {
		return nil, err
	}
	out := make([]ReservationGuest, 0, len(list))
	for _, r := range list {
		missing := []string{}
		if r.Email == nil {
			missing = append(missing, "email")
		}
		if r.DocumentNumber == nil {
			missing = append(missing, "document")
		}
		if !r.Front {
			missing = append(missing, "id_front")
		}
		if !r.Back {
			missing = append(missing, "id_back")
		}
		g, docs := r.GuestRead, []GuestDocument{}
		if contact {
			if docs, err = guestDocuments(ctx, q, g.ID); err != nil {
				return nil, err
			}
		} else {
			g = GuestRead{ID: g.ID, FullName: g.FullName, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt}
		}
		out = append(out, ReservationGuest{GuestRead: g, IsPrimary: r.IsPrimary, Registered: len(missing) == 0, Missing: missing, Documents: docs})
	}
	return out, nil
}

// LinkGuest agrega un huesped a la reserva (existente o nuevo). Con
// is_primary pasa a ser el principal.
func (s *Service) LinkGuest(ctx context.Context, actor authz.Actor, id uuid.UUID, in GuestLink) (ReservationDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (ReservationDetail, error) {
		r, v, err := s.visible(ctx, q, actor, id)
		if err != nil {
			return ReservationDetail{}, err
		}
		if r.Status == "cancelled" || r.Status == "no_show" || r.Status == "checked_out" {
			return ReservationDetail{}, ErrTransition
		}
		if in.GuestID == nil && in.Guest == nil {
			return ReservationDetail{}, ErrGuestRequired.WithField("body.guest_id")
		}
		guestID, err := s.resolveGuest(ctx, q, actor, in.GuestID, in.Guest)
		if err != nil {
			return ReservationDetail{}, err
		}
		if err := addGuest(ctx, q, actor, id, *guestID, in.IsPrimary || r.PrimaryGuestID == nil); err != nil {
			return ReservationDetail{}, err
		}
		if r, err = reservationByID(ctx, q, id, false); err != nil {
			return ReservationDetail{}, err
		}
		return s.detail(ctx, q, actor, v, redact(r, v.contact))
	})
}

// addGuest vincula al huesped (como principal si primary) y lo anota.
func addGuest(ctx context.Context, q database.Querier, actor authz.Actor, id, guestID uuid.UUID, primary bool) error {
	if primary {
		if _, err := q.Exec(ctx, `UPDATE reservation_guests SET is_primary = false WHERE reservation_id = $1`, id); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `UPDATE reservations SET primary_guest_id = $2, updated_at = now() WHERE id = $1`, id, guestID); err != nil {
			return err
		}
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO reservation_guests (reservation_id, guest_id, is_primary) VALUES ($1, $2, $3)
		ON CONFLICT (reservation_id, guest_id) DO UPDATE SET is_primary = EXCLUDED.is_primary`, id, guestID, primary); err != nil {
		return err
	}
	if err := refreshDocumentExpiry(ctx, q, guestID); err != nil {
		return err
	}
	g, err := guestByID(ctx, q, guestID)
	if err != nil {
		return err
	}
	return addEvent(ctx, q, id, "guest_added", actorID(actor), map[string]any{"guest_name": g.FullName, "primary": primary})
}

// UnlinkGuest quita un acompanante (el principal no se quita).
func (s *Service) UnlinkGuest(ctx context.Context, actor authz.Actor, id, guestID uuid.UUID) (ReservationDetail, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (ReservationDetail, error) {
		r, v, err := s.visible(ctx, q, actor, id)
		if err != nil {
			return ReservationDetail{}, err
		}
		if r.PrimaryGuestID != nil && *r.PrimaryGuestID == guestID {
			return ReservationDetail{}, ErrPrimaryGuest
		}
		var name string
		err = q.QueryRow(ctx, `
			DELETE FROM reservation_guests rg USING guests g
			WHERE rg.reservation_id = $1 AND rg.guest_id = $2 AND g.id = rg.guest_id RETURNING g.full_name`, id, guestID).Scan(&name)
		if errors.Is(err, pgx.ErrNoRows) {
			return ReservationDetail{}, ErrGuestNotFound
		}
		if err != nil {
			return ReservationDetail{}, err
		}
		if err := addEvent(ctx, q, id, "guest_removed", actorID(actor), map[string]any{"guest_name": name}); err != nil {
			return ReservationDetail{}, err
		}
		return s.detail(ctx, q, actor, v, r)
	})
}

// --- Documento ---------------------------------------------------------------------

// AttachDocument vincula la foto del frente o del reverso del documento. La
// foto anterior del mismo lado se descarta. Se borra sola a los 90 dias de la
// ultima salida del huesped.
func (s *Service) AttachDocument(ctx context.Context, actor authz.Actor, guestID uuid.UUID, in GuestDocumentAttach) (GuestDetail, error) {
	err := s.db.InTx(ctx, func(q database.Querier) error {
		if _, err := guestByID(ctx, q, guestID); err != nil {
			return err
		}
		tag, err := q.Exec(ctx, `
			UPDATE files SET entity_type = 'guest', entity_id = $2, updated_at = now()
			WHERE id = $1 AND module = 'guests' AND kind = $3 AND status = 'ready' AND deleted_at IS NULL
			  AND uploaded_by_user_id = $4 AND (entity_id IS NULL OR entity_id = $2)`,
			in.FileID, guestID, in.Kind, actor.UserID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrInvalidFiles.WithField("body.file_id")
		}
		if _, err := q.Exec(ctx, `
			UPDATE files SET status = 'deleted', updated_at = now()
			WHERE entity_type = 'guest' AND entity_id = $1 AND kind = $2 AND id <> $3 AND status = 'ready'`,
			guestID, in.Kind, in.FileID); err != nil {
			return err
		}
		if err := refreshDocumentExpiry(ctx, q, guestID); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: actorID(actor), Action: "guest.document_added", Entity: "guest", EntityID: &guestID,
			After: map[string]any{"kind": in.Kind},
		})
	})
	if err != nil {
		return GuestDetail{}, err
	}
	return s.Guest(ctx, actor, guestID)
}

// DocumentURL firma la descarga de una foto del documento (guests.documents).
// Cada consulta queda en la bitacora.
func (s *Service) DocumentURL(ctx context.Context, actor authz.Actor, guestID, fileID uuid.UUID) (DocumentURL, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (DocumentURL, error) {
		var key, kind string
		err := q.QueryRow(ctx, `
			SELECT s3_key, kind FROM files
			WHERE id = $1 AND entity_type = 'guest' AND entity_id = $2 AND module = 'guests'
			  AND status = 'ready' AND deleted_at IS NULL`, fileID, guestID).Scan(&key, &kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return DocumentURL{}, ErrDocumentNotFound
		}
		if err != nil {
			return DocumentURL{}, err
		}
		u, err := s.storage.PresignDownload(ctx, actor.TenantID, key, storage.DownloadTTL)
		if err != nil {
			return DocumentURL{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: actorID(actor), Action: "guest.document_viewed", Entity: "guest", EntityID: &guestID,
			After: map[string]any{"kind": kind, "file_id": fileID},
		}); err != nil {
			return DocumentURL{}, err
		}
		return DocumentURL{URL: u, ExpiresAt: s.now().Add(storage.DownloadTTL).UTC()}, nil
	})
}

func guestDocuments(ctx context.Context, q database.Querier, guestID uuid.UUID) ([]GuestDocument, error) {
	rows, err := q.Query(ctx, `
		SELECT id, kind, content_type, size_bytes, created_at, expires_at FROM files
		WHERE entity_type = 'guest' AND entity_id = $1 AND module = 'guests' AND status = 'ready' AND deleted_at IS NULL
		ORDER BY kind DESC, created_at DESC`, guestID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[GuestDocument])
}

// refreshDocumentExpiry fija el borrado de las fotos del documento: la salida
// mas tardia del huesped (en cualquier reserva viva) mas 90 dias.
func refreshDocumentExpiry(ctx context.Context, q database.Querier, guestID uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE files f SET expires_at = (
		       coalesce((SELECT max(coalesce(r.checked_out_at::date, r.check_out_date))
		                   FROM reservations r JOIN reservation_guests rg ON rg.reservation_id = r.id
		                  WHERE rg.guest_id = $1 AND r.status NOT IN ('cancelled', 'no_show')), current_date)
		       + $2::int)::timestamptz, updated_at = now()
		WHERE f.entity_type = 'guest' AND f.entity_id = $1 AND f.module = 'guests' AND f.status = 'ready'`,
		guestID, RetentionDays)
	return err
}
