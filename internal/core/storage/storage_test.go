package storage

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBuildKeyAlwaysUnderTenant(t *testing.T) {
	s := NewService(NewMemory())
	s.now = func() time.Time { return time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC) }
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	key, err := s.BuildKey(tenant, "tenant", "logo", "Mi Logo.PNG")
	if err != nil {
		t.Fatal(err)
	}
	want := regexp.MustCompile(`^tenants/11111111-1111-1111-1111-111111111111/tenant/2026/11/logo-[0-9a-f-]{36}\.png$`)
	if !want.MatchString(key) {
		t.Errorf("clave inesperada: %s", key)
	}
	if err := AssertTenantKey(key, tenant); err != nil {
		t.Error(err)
	}
	if _, err := s.BuildKey(tenant, "../shared", "logo", "x.png"); err == nil {
		t.Error("un modulo con ruta debe fallar")
	}
	for _, name := range []string{"x.p/../ng", "x../..", "x.ph p"} {
		key, err := s.BuildKey(tenant, "tenant", "logo", name)
		if err != nil || regexp.MustCompile(`\.\.|/\.|\s`).MatchString(key[len("tenants/"):]) {
			t.Errorf("el nombre %q no debe colar rutas: %s", name, key)
		}
	}
}

func TestDownloadRejectsForeignKey(t *testing.T) {
	s := NewService(NewMemory())
	a, b := uuid.New(), uuid.New()
	key, _ := s.BuildKey(b, "tenant", "logo", "x.png")
	if _, err := s.PresignDownload(context.Background(), a, key, 0); !errors.Is(err, ErrForeignKey) {
		t.Errorf("descargar la clave de otro tenant debe fallar: %v", err)
	}
	if _, err := s.PresignDownload(context.Background(), a, "tenants/"+a.String()+"/../"+b.String()+"/x", 0); !errors.Is(err, ErrForeignKey) {
		t.Errorf("las rutas con .. deben fallar: %v", err)
	}
	if url := s.LogoURL(context.Background(), a, &key); url != nil {
		t.Error("el logo de otra empresa no se firma")
	}
}
