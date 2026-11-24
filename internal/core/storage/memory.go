package storage

import (
	"context"
	"net/url"
	"sync"
	"time"
)

// Memory es un Backend en memoria para pruebas. Put simula la subida que hace
// el cliente con la URL prefirmada.
type Memory struct {
	mu      sync.Mutex
	objects map[string]ObjectInfo
}

// NewMemory crea el almacen vacio.
func NewMemory() *Memory {
	return &Memory{objects: map[string]ObjectInfo{}}
}

// Put guarda los metadatos de un objeto como si se hubiera subido.
func (m *Memory) Put(key string, info ObjectInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = info
}

// PutObject implementa Backend (solo guarda tamano y tipo).
func (m *Memory) PutObject(_ context.Context, key, contentType string, body []byte) error {
	m.Put(key, ObjectInfo{Size: int64(len(body)), ContentType: contentType})
	return nil
}

// PresignPut implementa Backend.
func (m *Memory) PresignPut(_ context.Context, key, contentType string, _ int64, ttl time.Duration) (Upload, error) {
	return Upload{
		URL: "memory://upload/" + url.PathEscape(key), Method: "PUT",
		Headers: map[string]string{"Content-Type": contentType}, ExpiresAt: time.Now().Add(ttl),
	}, nil
}

// PresignGet implementa Backend.
func (m *Memory) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "memory://get/" + url.PathEscape(key), nil
}

// Head implementa Backend.
func (m *Memory) Head(_ context.Context, key string) (ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, ok := m.objects[key]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return info, nil
}

// Delete implementa Backend.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}
