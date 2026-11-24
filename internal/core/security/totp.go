package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 usa HMAC-SHA1; es lo que leen todas las apps autenticadoras.
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Verificacion en dos pasos por codigos de un solo uso basados en tiempo
// (TOTP, RFC 6238): 6 digitos, pasos de 30 segundos, HMAC-SHA1. Es lo que
// esperan Google Authenticator, Microsoft Authenticator, 1Password y Authy.
const (
	totpDigits = 6
	totpPeriod = 30
	// totpSkew acepta el paso anterior y el siguiente por relojes desfasados.
	totpSkew = 1
)

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret genera un secreto de 160 bits codificado en base32.
func NewTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generando secreto TOTP: %w", err)
	}
	return totpEncoding.EncodeToString(b), nil
}

// TOTPURI arma el enlace otpauth:// que se muestra como QR.
func TOTPURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(totpPeriod))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// VerifyTOTP indica si code es valido para secret en el instante now.
func VerifyTOTP(secret, code string, now time.Time) bool {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return false
	}
	key, err := totpEncoding.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return false
	}
	step := now.Unix() / totpPeriod
	ok := 0
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		// Se comparan todos los pasos para no filtrar cual coincidio por tiempo.
		ok |= subtle.ConstantTimeCompare([]byte(hotp(key, step+d)), []byte(code))
	}
	return ok == 1
}

// TOTPCode calcula el codigo vigente (lo usan las pruebas).
func TOTPCode(secret string, now time.Time) (string, error) {
	key, err := totpEncoding.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return "", err
	}
	return hotp(key, now.Unix()/totpPeriod), nil
}

// hotp es el HOTP de RFC 4226 con truncamiento dinamico.
func hotp(key []byte, counter int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter)) //nolint:gosec // el contador de pasos es positivo
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, value%1_000_000)
}
