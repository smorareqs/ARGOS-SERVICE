package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/smorareqs/argos-service/internal/core/apperr"
)

func TestHTTPErrorMapsDomainKinds(t *testing.T) {
	cases := map[apperr.Kind]int{
		apperr.KindNotFound:         http.StatusNotFound,
		apperr.KindAlreadyExists:    http.StatusConflict,
		apperr.KindValidation:       http.StatusUnprocessableEntity,
		apperr.KindAuthentication:   http.StatusUnauthorized,
		apperr.KindPermissionDenied: http.StatusForbidden,
		apperr.KindInternal:         http.StatusInternalServerError,
	}
	for kind, want := range cases {
		domainErr := apperr.New(kind, "mensaje de dominio")
		// Tambien envuelto: la traduccion debe atravesar fmt.Errorf("%w").
		for _, err := range []error{domainErr, fmt.Errorf("contexto: %w", domainErr)} {
			got := HTTPError(context.Background(), err)
			if got.GetStatus() != want || got.Error() != "mensaje de dominio" {
				t.Errorf("kind %d: status %d %q, se esperaba %d", kind, got.GetStatus(), got.Error(), want)
			}
		}
	}
}

func TestHTTPErrorHidesUnexpectedErrors(t *testing.T) {
	got := HTTPError(context.Background(), errors.New(`ERROR: relation "shared.users" does not exist`))
	if got.GetStatus() != http.StatusInternalServerError || got.Error() != internalErrorMessage {
		t.Errorf("un error inesperado no debe filtrarse: %d %q", got.GetStatus(), got.Error())
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":   "abc",
		"bearer abc":   "abc",
		"BEARER  abc ": "abc",
	}
	for header, want := range cases {
		if got, ok := bearerToken(header); !ok || got != want {
			t.Errorf("bearerToken(%q) = %q, %v", header, got, ok)
		}
	}
	for _, header := range []string{"", "Bearer", "Bearer ", "Basic abc", "abc"} {
		if _, ok := bearerToken(header); ok {
			t.Errorf("bearerToken(%q) deberia fallar", header)
		}
	}
}
