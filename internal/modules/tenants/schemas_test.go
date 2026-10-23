package tenants

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeSlug(t *testing.T) {
	valid := []struct{ in, want string }{
		{"acme", "acme"},
		{"  ACME_Rentals ", "acme_rentals"},
		{"ab", "ab"},
		{"garrent2", "garrent2"},
		{strings.Repeat("a", 56), strings.Repeat("a", 56)},
	}
	for _, c := range valid {
		got, err := NormalizeSlug(c.in)
		if err != nil || got != c.want {
			t.Errorf("NormalizeSlug(%q) = %q, %v; se esperaba %q", c.in, got, err, c.want)
		}
	}

	invalid := []string{
		"", "a", " a ", "_acme", "acme_", "acme-rentals", "acme rentals", "ácme",
		`acme"; DROP SCHEMA shared; --`,
		// "tenant_" + 57 caracteres excede los 63 bytes de un identificador.
		strings.Repeat("a", 57),
	}
	for _, in := range invalid {
		if _, err := NormalizeSlug(in); !errors.Is(err, ErrInvalidSlug) {
			t.Errorf("NormalizeSlug(%q) deberia fallar", in)
		}
	}
}

func TestBuildSchemaName(t *testing.T) {
	if got := BuildSchemaName("acme"); got != "tenant_acme" {
		t.Errorf("BuildSchemaName = %q", got)
	}
}
