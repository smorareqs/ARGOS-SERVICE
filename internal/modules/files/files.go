// Package files registra los archivos de cada empresa (tabla files del
// tenant) y firma su subida y descarga en S3 (fase01.md §3.8).
//
// Flujo: el cliente pide presign-upload con module, kind, tipo y tamano; el
// backend valida el permiso, crea la fila en pending y devuelve la URL; el
// cliente sube directo a S3; confirm verifica con HEAD y marca ready. Los
// pending de mas de 24 h y los descartados los borra el job storage_cleanup.
package files

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/storage"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Rule define quien sube y quien ve un tipo de archivo. Cada modulo agrega
// las suyas (append-only).
type Rule struct {
	Upload   authz.Permission
	Read     []authz.Permission // cualquiera; vacio = cualquier miembro
	Types    []string
	MaxBytes int64
	// Private: no se descarga por GET /files/{id}/url sino por el endpoint del
	// modulo, que deja registro en la bitacora (documentos de huespedes).
	Private bool
}

var imageTypes = []string{"image/png", "image/jpeg", "image/webp"}

// Rules por "module/kind".
var Rules = map[string]Rule{
	// Logo de la empresa (W-O1): lo ve cualquier miembro.
	"tenant/logo": {Upload: authz.TenantSettings, Types: imageTypes, MaxBytes: 2 << 20},
	// Foto de portada de una propiedad (W-C2, app B1).
	"properties/cover": {
		Upload: authz.PropertiesManage, Read: []authz.Permission{authz.PropertiesRead}, Types: imageTypes, MaxBytes: 5 << 20,
	},
	// Fotos de una incidencia al reportarla (app B12, web W-G1) y al resolverla.
	"maintenance/ticket_photo": {
		Upload: authz.MaintenanceReport, Read: []authz.Permission{authz.MaintenanceRead, authz.MaintenanceReport},
		Types: imageTypes, MaxBytes: 8 << 20,
	},
	"maintenance/ticket_resolution": {
		Upload: authz.MaintenanceExecute, Read: []authz.Permission{authz.MaintenanceRead}, Types: imageTypes, MaxBytes: 8 << 20,
	},
	// Manuales, garantias y facturas de un activo (W-G2).
	"maintenance/asset_document": {
		Upload: authz.MaintenanceManage, Read: []authz.Permission{authz.MaintenanceRead},
		Types: append([]string{"application/pdf"}, imageTypes...), MaxBytes: 15 << 20,
	},
	// Fotos del documento del huesped (fase 4, W-H3 y app C5): las sube quien
	// hace check-ins; se ven solo con guests.documents por GET
	// /guests/{id}/documents/{file_id}/url, que queda en la bitacora. Se borran
	// a los 90 dias de la salida (expires_at, job retention_cleanup).
	"guests/id_front": {
		Upload: authz.ReservationsCheckin, Read: []authz.Permission{authz.GuestsDocuments}, Types: imageTypes, MaxBytes: 8 << 20, Private: true,
	},
	"guests/id_back": {
		Upload: authz.ReservationsCheckin, Read: []authz.Permission{authz.GuestsDocuments}, Types: imageTypes, MaxBytes: 8 << 20, Private: true,
	},
	// Fotos de inspecciones y sus hallazgos (fase 4, W-H3 y app C9).
	"reservations/inspection_photo": {
		Upload: authz.TasksValidate, Read: []authz.Permission{authz.ReservationsRead}, Types: imageTypes, MaxBytes: 8 << 20,
	},
	// Adjuntos de acciones disciplinarias (fase 5, W-J4). El colaborador los ve
	// por GET /disciplinary/me, que firma las URL de las suyas.
	"disciplinary/attachment": {
		Upload: authz.HRDisciplinaryManage, Read: []authz.Permission{authz.HRDisciplinaryManage},
		Types: append([]string{"application/pdf"}, imageTypes...), MaxBytes: 15 << 20,
	},
}

// Errores del modulo.
var (
	ErrFileNotFound   = apperr.New(apperr.KindNotFound, "El archivo no existe.")
	ErrUnsupported    = apperr.New(apperr.KindValidation, "Ese tipo de archivo no se puede subir aquí.")
	ErrContentType    = apperr.New(apperr.KindValidation, "El formato del archivo no está permitido.")
	ErrFileSize       = apperr.New(apperr.KindValidation, "El archivo pesa demasiado.")
	ErrNotUploaded    = apperr.New(apperr.KindValidation, "El archivo todavía no se subió. Intenta de nuevo.")
	ErrUploadMismatch = apperr.New(apperr.KindValidation, "El archivo subido no coincide con el que se anunció.")
	errForbidden      = apperr.New(apperr.KindPermissionDenied, "")
)

// File es una fila de la tabla files.
type File struct {
	ID          uuid.UUID  `db:"id"`
	S3Key       string     `db:"s3_key"`
	Module      string     `db:"module"`
	Kind        string     `db:"kind"`
	EntityType  *string    `db:"entity_type"`
	EntityID    *uuid.UUID `db:"entity_id"`
	ContentType string     `db:"content_type"`
	SizeBytes   int64      `db:"size_bytes"`
	Status      string     `db:"status"`
	CreatedAt   time.Time  `db:"created_at"`
}

const fileColumns = `id, s3_key, module, kind, entity_type, entity_id, content_type, size_bytes, status, created_at`

// PresignRequest es el cuerpo de POST /files/presign-upload.
type PresignRequest struct {
	_           struct{}   `additionalProperties:"true"`
	Module      string     `json:"module" minLength:"1" maxLength:"32"`
	Kind        string     `json:"kind" minLength:"1" maxLength:"32"`
	ContentType string     `json:"content_type" minLength:"1" maxLength:"128"`
	Size        int64      `json:"size" minimum:"1"`
	Filename    string     `json:"filename,omitempty" maxLength:"255"`
	EntityType  *string    `json:"entity_type,omitempty" maxLength:"64"`
	EntityID    *uuid.UUID `json:"entity_id,omitempty" format:"uuid"`
}

// PresignResponse indica como subir el archivo.
type PresignResponse struct {
	FileID    uuid.UUID         `json:"file_id" format:"uuid"`
	UploadURL string            `json:"upload_url"`
	Method    string            `json:"method" enum:"PUT"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// FileRead es un archivo registrado.
type FileRead struct {
	ID          uuid.UUID `json:"id" format:"uuid"`
	Module      string    `json:"module"`
	Kind        string    `json:"kind"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	Status      string    `json:"status" enum:"pending,ready,deleted"`
	CreatedAt   time.Time `json:"created_at"`
}

// URLResponse es una descarga firmada.
type URLResponse struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Caller es quien sube o descarga: su empresa, su usuario y sus permisos.
type Caller struct {
	TenantID    uuid.UUID
	UserID      uuid.UUID
	Permissions map[authz.Permission]bool
}

// Service contiene los casos de uso de archivos.
type Service struct {
	db      *database.DB
	storage *storage.Service
	log     *slog.Logger
	now     func() time.Time
}

// NewService crea el servicio.
func NewService(db *database.DB, st *storage.Service, log *slog.Logger) *Service {
	return &Service{db: db, storage: st, log: log, now: time.Now}
}

// PresignUpload registra el archivo en pending y firma su subida.
func (s *Service) PresignUpload(ctx context.Context, c Caller, in PresignRequest) (PresignResponse, error) {
	module, kind := strings.TrimSpace(in.Module), strings.TrimSpace(in.Kind)
	rule, ok := Rules[module+"/"+kind]
	if !ok {
		return PresignResponse{}, ErrUnsupported.WithField("body.kind")
	}
	if !c.Permissions[rule.Upload] {
		return PresignResponse{}, errForbidden
	}
	contentType := strings.ToLower(strings.TrimSpace(in.ContentType))
	if !slices.Contains(rule.Types, contentType) {
		return PresignResponse{}, ErrContentType.WithField("body.content_type")
	}
	if in.Size <= 0 || in.Size > rule.MaxBytes {
		return PresignResponse{}, ErrFileSize.WithField("body.size")
	}
	key, err := s.storage.BuildKey(c.TenantID, module, kind, in.Filename)
	if err != nil {
		return PresignResponse{}, ErrUnsupported.WithField("body.module")
	}
	upload, err := s.storage.PresignUpload(ctx, c.TenantID, key, contentType, in.Size)
	if err != nil {
		return PresignResponse{}, err
	}
	id, err := database.InTxValue(ctx, s.db, func(q database.Querier) (uuid.UUID, error) {
		var id uuid.UUID
		err := q.QueryRow(ctx, `
			INSERT INTO files (s3_key, module, kind, entity_type, entity_id, content_type, size_bytes,
			                   uploaded_by_user_id, original_name, status)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending') RETURNING id`,
			key, module, kind, in.EntityType, in.EntityID, contentType, in.Size, c.UserID, originalName(in.Filename)).Scan(&id)
		return id, err
	})
	if err != nil {
		return PresignResponse{}, err
	}
	return PresignResponse{
		FileID: id, UploadURL: upload.URL, Method: upload.Method, Headers: upload.Headers, ExpiresAt: upload.ExpiresAt.UTC(),
	}, nil
}

// Confirm verifica en S3 que el archivo se subio tal como se anuncio y lo
// marca listo.
func (s *Service) Confirm(ctx context.Context, c Caller, id uuid.UUID) (FileRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (FileRead, error) {
		f, err := byID(ctx, q, id, true)
		if err != nil {
			return FileRead{}, err
		}
		rule, ok := Rules[f.Module+"/"+f.Kind]
		if !ok || !c.Permissions[rule.Upload] {
			return FileRead{}, ErrFileNotFound
		}
		if f.Status == "ready" {
			return newRead(*f), nil
		}
		if f.Status != "pending" {
			return FileRead{}, ErrFileNotFound
		}
		if err := s.verifyUpload(ctx, c.TenantID, *f); err != nil {
			return FileRead{}, err
		}
		if _, err := q.Exec(ctx, `UPDATE files SET status = 'ready', updated_at = now() WHERE id = $1`, id); err != nil {
			return FileRead{}, err
		}
		f.Status = "ready"
		return newRead(*f), nil
	})
}

// verifyUpload comprueba con HEAD que el objeto existe con el tamano y el
// tipo anunciados.
func (s *Service) verifyUpload(ctx context.Context, tenantID uuid.UUID, f File) error {
	info, err := s.storage.Head(ctx, tenantID, f.S3Key)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return ErrNotUploaded
	case errors.Is(err, storage.ErrForeignKey):
		return ErrFileNotFound
	case err != nil:
		return err
	}
	if info.Size != f.SizeBytes || (info.ContentType != "" && !strings.EqualFold(info.ContentType, f.ContentType)) {
		return ErrUploadMismatch
	}
	return nil
}

// DownloadURL firma la descarga de un archivo listo. Una clave con el prefijo
// de otra empresa responde 404 aunque el id exista (fase01.md §3.10 caso 5).
func (s *Service) DownloadURL(ctx context.Context, c Caller, id uuid.UUID) (URLResponse, error) {
	f, err := database.InTxValue(ctx, s.db, func(q database.Querier) (*File, error) {
		return byID(ctx, q, id, false)
	})
	if err != nil {
		return URLResponse{}, err
	}
	rule, ok := Rules[f.Module+"/"+f.Kind]
	if !ok || rule.Private || f.Status != "ready" || !canRead(rule, c) {
		return URLResponse{}, ErrFileNotFound
	}
	url, err := s.storage.PresignDownload(ctx, c.TenantID, f.S3Key, storage.DownloadTTL)
	if errors.Is(err, storage.ErrForeignKey) {
		return URLResponse{}, ErrFileNotFound
	}
	if err != nil {
		return URLResponse{}, err
	}
	return URLResponse{URL: url, ExpiresAt: s.now().Add(storage.DownloadTTL).UTC()}, nil
}

// Cleanup borra los archivos pending de mas de 24 h y los descartados, en
// todas las empresas (job storage_cleanup).
func (s *Service) Cleanup(ctx context.Context) error {
	type tenantRow struct {
		ID     uuid.UUID `db:"id"`
		Schema string    `db:"schema_name"`
	}
	tenants, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]tenantRow, error) {
		rows, err := q.Query(ctx, "SELECT id, schema_name FROM shared.tenants")
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[tenantRow])
	})
	if err != nil {
		return err
	}
	removed := 0
	for _, t := range tenants {
		tctx := database.WithTenantSchema(ctx, t.Schema)
		stale, err := database.InTxValue(tctx, s.db, func(q database.Querier) ([]File, error) {
			// Los vencidos por retencion (deleted con deleted_at y expires_at) se
			// quedan como registro: su objeto ya lo borro Retention.
			rows, err := q.Query(tctx, "SELECT "+fileColumns+` FROM files
				WHERE (status = 'pending' AND created_at < now() - interval '24 hours')
				   OR (status = 'deleted' AND NOT (deleted_at IS NOT NULL AND expires_at IS NOT NULL))
				LIMIT 500`)
			if err != nil {
				return nil, err
			}
			return pgx.CollectRows(rows, pgx.RowToStructByName[File])
		})
		if err != nil {
			return fmt.Errorf("tenant %s: %w", t.Schema, err)
		}
		for _, f := range stale {
			if err := s.storage.Delete(tctx, t.ID, f.S3Key); err != nil && !errors.Is(err, storage.ErrNotFound) {
				s.log.WarnContext(ctx, "No se pudo borrar el objeto", "file_id", f.ID, "error", err)
				continue
			}
			if err := s.db.InTx(tctx, func(q database.Querier) error {
				_, err := q.Exec(tctx, "DELETE FROM files WHERE id = $1", f.ID)
				return err
			}); err != nil {
				return err
			}
			removed++
		}
	}
	if removed > 0 {
		s.log.InfoContext(ctx, "Archivos limpiados", "count", removed)
	}
	return nil
}

// Retention borra de S3 los archivos cuyo expires_at ya paso y deja el
// registro en deleted (con deleted_at) para la bitacora: el numero de
// documento del huesped se conserva en su ficha (job retention_cleanup,
// fase04.md §3). Devuelve cuantos borro.
func (s *Service) Retention(ctx context.Context) (int, error) {
	tenants, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]uuid.UUID, error) {
		rows, err := q.Query(ctx, "SELECT id FROM shared.tenants ORDER BY slug")
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	})
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, id := range tenants {
		n, err := s.retainTenant(ctx, id)
		if err != nil {
			return removed, err
		}
		removed += n
	}
	if removed > 0 {
		s.log.InfoContext(ctx, "Archivos vencidos por retencion", "count", removed)
	}
	return removed, nil
}

func (s *Service) retainTenant(ctx context.Context, tenantID uuid.UUID) (int, error) {
	var schema string
	if err := s.db.InTx(ctx, func(q database.Querier) error {
		return q.QueryRow(ctx, "SELECT schema_name FROM shared.tenants WHERE id = $1", tenantID).Scan(&schema)
	}); err != nil {
		return 0, err
	}
	tctx := database.WithTenantSchema(ctx, schema)
	expired, err := database.InTxValue(tctx, s.db, func(q database.Querier) ([]File, error) {
		rows, err := q.Query(tctx, "SELECT "+fileColumns+` FROM files
			WHERE status = 'ready' AND deleted_at IS NULL AND expires_at IS NOT NULL AND expires_at <= $1
			LIMIT 500`, s.now())
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[File])
	})
	if err != nil {
		return 0, fmt.Errorf("tenant %s: %w", schema, err)
	}
	n := 0
	for _, f := range expired {
		if err := s.storage.Delete(tctx, tenantID, f.S3Key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			s.log.WarnContext(ctx, "No se pudo borrar el objeto vencido", "file_id", f.ID, "error", err)
			continue
		}
		if err := s.db.InTx(tctx, func(q database.Querier) error {
			_, err := q.Exec(tctx, `UPDATE files SET status = 'deleted', deleted_at = now(), updated_at = now() WHERE id = $1`, f.ID)
			return err
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func byID(ctx context.Context, q database.Querier, id uuid.UUID, lock bool) (*File, error) {
	sql := "SELECT " + fileColumns + " FROM files WHERE id = $1 AND deleted_at IS NULL"
	if lock {
		sql += " FOR UPDATE"
	}
	rows, err := q.Query(ctx, sql, id)
	if err != nil {
		return nil, err
	}
	f, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[File])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrFileNotFound
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

func canRead(r Rule, c Caller) bool {
	if len(r.Read) == 0 {
		return true
	}
	for _, p := range r.Read {
		if c.Permissions[p] {
			return true
		}
	}
	return false
}

func newRead(f File) FileRead {
	return FileRead{
		ID: f.ID, Module: f.Module, Kind: f.Kind, ContentType: f.ContentType, SizeBytes: f.SizeBytes,
		Status: f.Status, CreatedAt: f.CreatedAt.UTC(),
	}
}

// originalName es el nombre que se muestra del archivo: sin ruta y sin
// caracteres de control. Vacio = nil.
func originalName(name string) *string {
	name = strings.TrimSpace(path.Base(strings.ReplaceAll(name, "\\", "/")))
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == "/" {
		return nil
	}
	if r := []rune(name); len(r) > 255 {
		name = string(r[:255])
	}
	return &name
}
