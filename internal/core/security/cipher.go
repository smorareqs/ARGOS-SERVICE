package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// cipherPrefix marca los valores cifrados con la version 1 del formato
// (AES-256-GCM, nonce de 12 bytes delante del texto cifrado, base64 URL).
const cipherPrefix = "v1:"

// ErrDecrypt indica un valor cifrado corrupto o con otra clave.
var ErrDecrypt = errors.New("no se pudo descifrar el campo")

// FieldCipher cifra campos sensibles en reposo (secreto TOTP y, desde la fase
// 2, la informacion de acceso a propiedades) con AES-256-GCM.
type FieldCipher struct {
	aead cipher.AEAD
}

// NewFieldCipher crea el cifrador. key es la clave de 32 bytes en base64
// (ARGOS_FIELD_ENCRYPTION_KEY). Si key esta vacia y fallback no, la clave se
// deriva de fallback con SHA-256: solo para local y pruebas.
func NewFieldCipher(key, fallback string) (*FieldCipher, error) {
	var raw []byte
	switch {
	case key != "":
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
		if err != nil || len(decoded) != 32 {
			return nil, errors.New("ARGOS_FIELD_ENCRYPTION_KEY debe ser 32 bytes en base64")
		}
		raw = decoded
	case fallback != "":
		sum := sha256.Sum256([]byte("argos-field-key|" + fallback))
		raw = sum[:]
	default:
		return nil, errors.New("falta la clave de cifrado de campos")
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("creando AES: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creando GCM: %w", err)
	}
	return &FieldCipher{aead: aead}, nil
}

// Encrypt devuelve plain cifrado y codificado para guardarlo como texto.
func (c *FieldCipher) Encrypt(plain string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generando nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plain), nil)
	return cipherPrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Decrypt invierte Encrypt.
func (c *FieldCipher) Decrypt(stored string) (string, error) {
	body, ok := strings.CutPrefix(stored, cipherPrefix)
	if !ok {
		return "", ErrDecrypt
	}
	sealed, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(sealed) < c.aead.NonceSize() {
		return "", ErrDecrypt
	}
	nonce, data := sealed[:c.aead.NonceSize()], sealed[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, data, nil)
	if err != nil {
		return "", ErrDecrypt
	}
	return string(plain), nil
}
