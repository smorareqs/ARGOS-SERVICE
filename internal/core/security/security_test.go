package security

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "test-secret"

func newManager(t *testing.T) *TokenManager {
	t.Helper()
	m, err := NewTokenManager(testSecret, "HS256", 30*time.Minute, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPasswordHashingRoundtrip(t *testing.T) {
	hashed, err := HashPassword("super-secret")
	if err != nil {
		t.Fatal(err)
	}
	if hashed == "super-secret" {
		t.Fatal("el hash no debe ser la contrasena")
	}
	if !VerifyPassword("super-secret", hashed) {
		t.Error("la contrasena correcta no verifica")
	}
	if VerifyPassword("wrong", hashed) {
		t.Error("una contrasena incorrecta verifica")
	}
}

func TestPasswordLongerThan72BytesIsTruncated(t *testing.T) {
	long := strings.Repeat("a", 100)
	hashed, err := HashPassword(long)
	if err != nil {
		t.Fatalf("una contrasena larga no debe fallar: %v", err)
	}
	if !VerifyPassword(strings.Repeat("a", 72)+"distinto", hashed) {
		t.Error("solo deben contar los primeros 72 bytes, como en el backend anterior")
	}
}

// Los usuarios creados por el backend anterior (Python, bcrypt $2b$) deben
// poder seguir iniciando sesion.
func TestVerifyPasswordAcceptsPythonBcryptHash(t *testing.T) {
	const pythonHash = "$2b$12$a8eyCjQ1kJ51d2lxherNZemMCvBde0qhCzPxwQHFo8SX/rkxiWYRe"
	if !VerifyPassword("super-secret", pythonHash) {
		t.Error("no verifica un hash generado por bcrypt de Python")
	}
}

func TestAccessTokenContainsClaims(t *testing.T) {
	m := newManager(t)
	token, err := m.CreateAccessToken("user-123", map[string]any{"role": "admin"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := m.Decode(token)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"sub": "user-123", "type": "access", "role": "admin"} {
		if claims[key] != want {
			t.Errorf("claim %s = %v, se esperaba %v", key, claims[key], want)
		}
	}
	if claims["jti"] == "" || claims["exp"] == nil || claims["iat"] == nil {
		t.Errorf("faltan claims estandar: %v", claims)
	}
}

func TestExtraClaimsCannotOverrideStandardClaims(t *testing.T) {
	m := newManager(t)
	token, err := m.CreateRefreshToken("user-123", map[string]any{"type": "access", "sub": "otro"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := m.Decode(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims["type"] != "refresh" || claims["sub"] != "user-123" {
		t.Errorf("claims sobrescritos: %v", claims)
	}
}

func TestDecodeRejectsInvalidTokens(t *testing.T) {
	m := newManager(t)
	valid, err := m.CreateAccessToken("user-123", nil)
	if err != nil {
		t.Fatal(err)
	}

	expiredManager := newManager(t)
	expiredManager.now = func() time.Time { return time.Now().Add(-time.Hour) }
	expired, err := expiredManager.CreateAccessToken("user-123", nil)
	if err != nil {
		t.Fatal(err)
	}

	other, err := NewTokenManager("otra-clave", "HS256", time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	wrongKey, err := other.CreateAccessToken("user-123", nil)
	if err != nil {
		t.Fatal(err)
	}

	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": "user-123", "type": "access", "exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}

	noExp, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "user-123"}).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"expirado":       expired,
		"otra clave":     wrongKey,
		"alg none":       unsigned,
		"sin exp":        noExp,
		"basura":         "no-es-un-jwt",
		"firma alterada": valid[:len(valid)-2] + "xx",
		"vacio":          "",
		"alg cambiado":   strings.Replace(valid, "eyJhbGciOiJIUzI1NiIs", "eyJhbGciOiJIUzUxMiIs", 1),
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := m.Decode(token); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("se esperaba ErrInvalidToken, se obtuvo %v", err)
			}
		})
	}
}

func TestNewTokenManagerRejectsNonHMACAlgorithms(t *testing.T) {
	for _, alg := range []string{"none", "RS256", "ES256", "nope"} {
		if _, err := NewTokenManager(testSecret, alg, time.Minute, time.Hour); err == nil {
			t.Errorf("%s deberia rechazarse", alg)
		}
	}
	if _, err := NewTokenManager("", "HS256", time.Minute, time.Hour); err == nil {
		t.Error("una clave vacia deberia rechazarse")
	}
}
