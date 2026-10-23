package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeEmail(t *testing.T) {
	valid := map[string]string{
		"ana@example.com":          "ana@example.com",
		"Ana.Lopez@Example.COM":    "Ana.Lopez@example.com",
		"ana+argos@sub.garrent.gt": "ana+argos@sub.garrent.gt",
	}
	for in, want := range valid {
		got, err := normalizeEmail(in)
		if err != nil || got != want {
			t.Errorf("normalizeEmail(%q) = %q, %v; se esperaba %q", in, got, err, want)
		}
	}

	invalid := []string{
		"", "ana", "ana@", "@example.com", "ana@localhost", "ana@example.",
		"Ana <ana@example.com>", " ana@example.com", "ana@@example.com",
		strings.Repeat("a", 65) + "@example.com",
	}
	for _, in := range invalid {
		if _, err := normalizeEmail(in); !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("normalizeEmail(%q) deberia fallar", in)
		}
	}
}

func TestUserCreateNormalized(t *testing.T) {
	base := UserCreate{Email: "ana@Example.com", FullName: "Ana", Password: "12345678"}

	got, err := base.normalized()
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "ana@example.com" || got.Role != RoleEmployee {
		t.Errorf("normalizacion inesperada: %+v", got)
	}

	cases := map[string]struct {
		mutate func(*UserCreate)
		want   error
	}{
		"contrasena corta": {func(u *UserCreate) { u.Password = "1234567" }, ErrInvalidPassword},
		"contrasena larga": {func(u *UserCreate) { u.Password = strings.Repeat("x", 129) }, ErrInvalidPassword},
		"nombre vacio":     {func(u *UserCreate) { u.FullName = "" }, ErrInvalidFullName},
		"rol invalido":     {func(u *UserCreate) { u.Role = "root" }, ErrInvalidRole},
		"correo invalido":  {func(u *UserCreate) { u.Email = "no-es-correo" }, ErrInvalidEmail},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in := base
			c.mutate(&in)
			if _, err := in.normalized(); !errors.Is(err, c.want) {
				t.Errorf("se esperaba %v, se obtuvo %v", c.want, err)
			}
		})
	}

	// Los limites cuentan caracteres, no bytes (como Pydantic).
	accents := base
	accents.Password = strings.Repeat("ñ", 128)
	if _, err := accents.normalized(); err != nil {
		t.Errorf("128 caracteres multibyte deberian aceptarse: %v", err)
	}
}
