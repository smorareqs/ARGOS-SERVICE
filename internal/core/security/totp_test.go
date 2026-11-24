package security

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// Vectores del apendice B de RFC 6238 (SHA1, secreto "12345678901234567890"),
// truncados a 6 digitos.
func TestTOTPRFCVectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	cases := []struct {
		unix int64
		code string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
	}
	for _, c := range cases {
		got, err := TOTPCode(secret, time.Unix(c.unix, 0))
		if err != nil || got != c.code {
			t.Errorf("t=%d: %s, %v; se esperaba %s", c.unix, got, err, c.code)
		}
		if !VerifyTOTP(secret, c.code, time.Unix(c.unix, 0)) {
			t.Errorf("t=%d: el codigo vigente no verifica", c.unix)
		}
	}
}

func TestTOTPWindow(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	code, _ := TOTPCode(secret, now)
	if !VerifyTOTP(secret, code, now.Add(30*time.Second)) {
		t.Error("deberia aceptar el paso anterior")
	}
	if VerifyTOTP(secret, code, now.Add(90*time.Second)) {
		t.Error("no deberia aceptar un codigo de hace tres pasos")
	}
	if VerifyTOTP(secret, "12345", now) || VerifyTOTP(secret, "abcdef", now) {
		t.Error("codigos mal formados no deben verificar")
	}
	if !strings.HasPrefix(TOTPURI("ARGOS", "ana@garrent.gt", secret), "otpauth://totp/ARGOS:ana@garrent.gt?") {
		t.Error("URI otpauth inesperada")
	}
}

func TestFieldCipherRoundTrip(t *testing.T) {
	c, err := NewFieldCipher("", "secreto-local")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := c.Encrypt("JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(enc, "v1:") || strings.Contains(enc, "JBSWY3DP") {
		t.Fatalf("cifrado inesperado: %s", enc)
	}
	plain, err := c.Decrypt(enc)
	if err != nil || plain != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("Decrypt = %q, %v", plain, err)
	}
	other, _ := NewFieldCipher("", "otra-clave")
	if _, err := other.Decrypt(enc); err == nil {
		t.Error("otra clave no debe descifrar")
	}
	if _, err := NewFieldCipher("corta", ""); err == nil {
		t.Error("una clave que no es de 32 bytes debe fallar")
	}
}
