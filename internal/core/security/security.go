// Package security contiene la logica criptografica: hashing de contrasenas
// y JWT.
//
// No conoce HTTP ni la base de datos; expone funciones reutilizables por la
// capa de servicios de autenticacion.
package security

import (
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

const (
	// bcrypt limita la contrasena a 72 bytes. Se trunca explicitamente para
	// que las contrasenas largas no fallen y para mantener compatibilidad con
	// los hashes generados por el backend anterior (Python), que truncaba igual.
	bcryptMaxBytes = 72
	// Mismo costo que bcrypt.gensalt() en Python (12 rondas).
	bcryptCost = 12
)

// HashPassword genera el hash bcrypt de una contrasena en texto plano.
func HashPassword(plain string) (string, error) {
	hashed, err := bcrypt.GenerateFromPassword(truncate(plain), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("generando hash bcrypt: %w", err)
	}
	return string(hashed), nil
}

// VerifyPassword compara una contrasena en texto plano contra su hash.
func VerifyPassword(plain, hashed string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hashed), truncate(plain)) == nil
}

func truncate(plain string) []byte {
	b := []byte(plain)
	if len(b) > bcryptMaxBytes {
		b = b[:bcryptMaxBytes]
	}
	return b
}

// TokenType distingue los tokens de acceso de los de refresco.
type TokenType string

// Tipos de token emitidos.
const (
	TokenAccess  TokenType = "access"
	TokenRefresh TokenType = "refresh"
	// TokenSelection permite elegir empresa tras un login con varias
	// membresias; dura minutos y no da acceso a la API.
	TokenSelection TokenType = "selection"
)

// ErrInvalidToken se devuelve cuando un JWT es invalido o expiro.
var ErrInvalidToken = errors.New("token invalido o expirado")

// TokenManager firma y valida JWT con una clave simetrica (HMAC).
type TokenManager struct {
	secret     []byte
	method     jwt.SigningMethod
	accessTTL  time.Duration
	refreshTTL time.Duration
	now        func() time.Time
}

// NewTokenManager crea un TokenManager. algorithm debe ser HS256, HS384 o HS512.
func NewTokenManager(secret, algorithm string, accessTTL, refreshTTL time.Duration) (*TokenManager, error) {
	method, ok := jwt.GetSigningMethod(algorithm).(*jwt.SigningMethodHMAC)
	if !ok {
		return nil, fmt.Errorf("algoritmo JWT no soportado: %q", algorithm)
	}
	if secret == "" {
		return nil, errors.New("la clave secreta JWT esta vacia")
	}
	return &TokenManager{
		secret:     []byte(secret),
		method:     method,
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
		now:        time.Now,
	}, nil
}

// CreateAccessToken crea un token de acceso de corta duracion.
//
// extraClaims permite incrustar el tenant_id y el role para que la API los
// conozca sin un viaje extra a la BD.
func (m *TokenManager) CreateAccessToken(subject string, extraClaims map[string]any) (string, error) {
	return m.create(subject, TokenAccess, m.accessTTL, extraClaims)
}

// CreateToken crea un JWT del tipo y duracion indicados.
func (m *TokenManager) CreateToken(subject string, tokenType TokenType, ttl time.Duration, extraClaims map[string]any) (string, error) {
	return m.create(subject, tokenType, ttl, extraClaims)
}

// AccessTTL es la duracion de los tokens de acceso.
func (m *TokenManager) AccessTTL() time.Duration {
	return m.accessTTL
}

// CreateRefreshToken crea un token de refresco de larga duracion.
func (m *TokenManager) CreateRefreshToken(subject string, extraClaims map[string]any) (string, error) {
	return m.create(subject, TokenRefresh, m.refreshTTL, extraClaims)
}

func (m *TokenManager) create(subject string, tokenType TokenType, ttl time.Duration, extraClaims map[string]any) (string, error) {
	now := m.now()
	claims := jwt.MapClaims{}
	maps.Copy(claims, extraClaims)
	// Los claims estandar se escriben al final para que extraClaims no pueda
	// sobrescribirlos.
	claims["sub"] = subject
	claims["type"] = string(tokenType)
	claims["iat"] = now.Unix()
	claims["exp"] = now.Add(ttl).Unix()
	claims["jti"] = uuid.NewString()

	signed, err := jwt.NewWithClaims(m.method, claims).SignedString(m.secret)
	if err != nil {
		return "", fmt.Errorf("firmando JWT: %w", err)
	}
	return signed, nil
}

// Decode valida firma, algoritmo y expiracion de un JWT y devuelve sus claims.
func (m *TokenManager) Decode(token string) (map[string]any, error) {
	parsed, err := jwt.Parse(
		token,
		func(*jwt.Token) (any, error) { return m.secret, nil },
		jwt.WithValidMethods([]string{m.method.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(m.now),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return nil, ErrInvalidToken
	}
	return claims, nil
}
