// Package storage guarda archivos en S3 con prefijo por tenant y URLs
// prefirmadas (fase01.md §3.8). Nunca se construye una clave fuera de
// Service.BuildKey, y nunca se firma una descarga sin AssertTenantKey.
package storage

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Vigencias de las URLs prefirmadas.
const (
	UploadTTL   = 15 * time.Minute
	DownloadTTL = 5 * time.Minute
	// LogoTTL es mas largo: los logos se muestran en toda la sesion.
	LogoTTL = 12 * time.Hour
)

// Errores del almacenamiento.
var (
	ErrNotFound    = errors.New("el objeto no existe")
	ErrForeignKey  = errors.New("la clave no pertenece al tenant")
	errInvalidPart = errors.New("modulo o tipo de archivo invalido")
)

// ObjectInfo son los metadatos de un objeto subido.
type ObjectInfo struct {
	Size        int64
	ContentType string
}

// Upload es una subida prefirmada: el cliente hace Method a URL con Headers.
type Upload struct {
	URL       string
	Method    string
	Headers   map[string]string
	ExpiresAt time.Time
}

// Backend es el almacen de objetos (S3 o memoria en pruebas).
type Backend interface {
	PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (Upload, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	Head(ctx context.Context, key string) (ObjectInfo, error)
	Delete(ctx context.Context, key string) error
	// PutObject sube un objeto generado por el servidor (fotos de la semilla,
	// boletas en PDF de la fase 5).
	PutObject(ctx context.Context, key, contentType string, body []byte) error
}

// Service aplica las reglas de claves sobre un Backend.
type Service struct {
	backend Backend
	now     func() time.Time
}

// NewService crea el servicio.
func NewService(b Backend) *Service {
	return &Service{backend: b, now: time.Now}
}

// BuildKey arma "tenants/{tenant_id}/{module}/{yyyy}/{mm}/{uuid}.{ext}".
func (s *Service) BuildKey(tenantID uuid.UUID, module, kind, filename string) (string, error) {
	if !safePart(module) || !safePart(kind) {
		return "", errInvalidPart
	}
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(filename), "."))
	if ext != "" && !safePart(ext) {
		ext = ""
	}
	now := s.now().UTC()
	name := uuid.NewString()
	if ext != "" {
		name += "." + ext
	}
	return fmt.Sprintf("tenants/%s/%s/%04d/%02d/%s-%s", tenantID, module, now.Year(), now.Month(), kind, name), nil
}

// AssertTenantKey falla si la clave no esta bajo el prefijo del tenant.
func AssertTenantKey(key string, tenantID uuid.UUID) error {
	if !strings.HasPrefix(key, "tenants/"+tenantID.String()+"/") || strings.Contains(key, "..") {
		return ErrForeignKey
	}
	return nil
}

// PresignUpload firma la subida de un objeto de tamano y tipo exactos.
func (s *Service) PresignUpload(ctx context.Context, tenantID uuid.UUID, key, contentType string, size int64) (Upload, error) {
	if err := AssertTenantKey(key, tenantID); err != nil {
		return Upload{}, err
	}
	return s.backend.PresignPut(ctx, key, contentType, size, UploadTTL)
}

// PresignDownload firma la descarga (verifica el prefijo del tenant).
func (s *Service) PresignDownload(ctx context.Context, tenantID uuid.UUID, key string, ttl time.Duration) (string, error) {
	if err := AssertTenantKey(key, tenantID); err != nil {
		return "", err
	}
	if ttl <= 0 {
		ttl = DownloadTTL
	}
	return s.backend.PresignGet(ctx, key, ttl)
}

// Head devuelve los metadatos del objeto (ErrNotFound si no se subio).
func (s *Service) Head(ctx context.Context, tenantID uuid.UUID, key string) (ObjectInfo, error) {
	if err := AssertTenantKey(key, tenantID); err != nil {
		return ObjectInfo{}, err
	}
	return s.backend.Head(ctx, key)
}

// PutObject sube un objeto generado por el servidor a la clave de la empresa.
func (s *Service) PutObject(ctx context.Context, tenantID uuid.UUID, key, contentType string, body []byte) error {
	if err := AssertTenantKey(key, tenantID); err != nil {
		return err
	}
	return s.backend.PutObject(ctx, key, contentType, body)
}

// Delete borra el objeto.
func (s *Service) Delete(ctx context.Context, tenantID uuid.UUID, key string) error {
	if err := AssertTenantKey(key, tenantID); err != nil {
		return err
	}
	return s.backend.Delete(ctx, key)
}

// LogoURL firma la URL del logo de una empresa; nil si no tiene o si falla
// (un logo nunca debe romper el login).
func (s *Service) LogoURL(ctx context.Context, tenantID uuid.UUID, key *string) *string {
	if s == nil || key == nil || *key == "" {
		return nil
	}
	url, err := s.PresignDownload(ctx, tenantID, *key, LogoTTL)
	if err != nil {
		return nil
	}
	return &url
}

func safePart(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// Backend devuelve el almacen subyacente (las pruebas simulan subidas con
// Memory.Put).
func (s *Service) Backend() Backend {
	return s.backend
}
