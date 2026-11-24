// Package apikeys administra las claves API de cada empresa (fase06.md §4.1):
// con ellas el sitio web del cliente manda reservas a POST /public/reservations.
// Solo se guarda el hash SHA-256; la clave completa se muestra una vez.
package apikeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// ScopeReservationsWrite deja crear reservas por la API publica.
const ScopeReservationsWrite = "reservations:write"

// Scopes son los alcances validos.
var Scopes = []string{ScopeReservationsWrite}

// prefix distingue las claves de ARGOS en un registro o un repositorio.
const prefix = "argos_"

// Errores del modulo.
var (
	ErrNotFound = apperr.New(apperr.KindNotFound, "La clave no existe.")
	ErrScope    = apperr.New(apperr.KindValidation, "Hay un alcance que no existe.")
	ErrName     = apperr.New(apperr.KindValidation, "Ponle un nombre que diga dónde se usa (2 a 64 caracteres).")
	ErrInvalid  = apperr.New(apperr.KindAuthentication, "Clave API inválida o revocada.")
	ErrRevoked  = apperr.New(apperr.KindValidation, "La clave ya estaba revocada.")
)

// KeyRead es una clave sin su secreto.
type KeyRead struct {
	ID            uuid.UUID  `json:"id" format:"uuid"`
	Name          string     `json:"name"`
	Prefix        string     `json:"prefix" doc:"Primeros caracteres para reconocerla (argos_XXXXXXXX…)."`
	Scopes        []string   `json:"scopes"`
	LastUsedAt    *time.Time `json:"last_used_at" nullable:"true"`
	RevokedAt     *time.Time `json:"revoked_at" nullable:"true"`
	CreatedByName *string    `json:"created_by_name" nullable:"true"`
	CreatedAt     time.Time  `json:"created_at"`
}

// KeyCreated es la respuesta del alta: la unica vez que viaja la clave.
type KeyCreated struct {
	KeyRead
	Key string `json:"key" doc:"La clave completa. Se muestra solo ahora: guárdala en el sitio web."`
}

// KeyCreate es el cuerpo de POST /api-keys.
type KeyCreate struct {
	Name   string   `json:"name" minLength:"2" maxLength:"64"`
	Scopes []string `json:"scopes,omitempty" doc:"Por defecto [reservations:write]."`
}

// Key es una clave valida resuelta desde la cabecera X-Api-Key.
type Key struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	TenantSchema string
	TenantActive bool
	Features     map[string]bool
	BillingState string
	Name         string
	Scopes       []string
}

// Can indica si la clave tiene el alcance.
func (k Key) Can(scope string) bool {
	return slices.Contains(k.Scopes, scope)
}

// Service administra las claves.
type Service struct {
	db  *database.DB
	log *slog.Logger
}

// NewService crea el servicio.
func NewService(db *database.DB, log *slog.Logger) *Service {
	return &Service{db: db, log: log}
}

func hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

const selectKey = `
	SELECT k.id, k.name, k.key_prefix, k.scopes, k.last_used_at, k.revoked_at, u.full_name, k.created_at
	FROM shared.api_keys k LEFT JOIN shared.users u ON u.id = k.created_by`

func scanKey(row pgx.Row) (KeyRead, error) {
	var k KeyRead
	err := row.Scan(&k.ID, &k.Name, &k.Prefix, &k.Scopes, &k.LastUsedAt, &k.RevokedAt, &k.CreatedByName, &k.CreatedAt)
	k.Prefix = prefix + k.Prefix
	return k, err
}

// List devuelve las claves de la empresa (vigentes primero).
func (s *Service) List(ctx context.Context, tenantID uuid.UUID) ([]KeyRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]KeyRead, error) {
		rows, err := q.Query(ctx, selectKey+` WHERE k.tenant_id = $1 ORDER BY k.revoked_at IS NOT NULL, k.created_at DESC`, tenantID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []KeyRead{}
		for rows.Next() {
			k, err := scanKey(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, k)
		}
		return out, rows.Err()
	})
}

// Create genera una clave nueva y la devuelve completa una sola vez.
func (s *Service) Create(ctx context.Context, actor authz.Actor, in KeyCreate) (KeyCreated, error) {
	name := strings.TrimSpace(in.Name)
	if n := len([]rune(name)); n < 2 || n > 64 {
		return KeyCreated{}, ErrName.WithField("body.name")
	}
	scopes := in.Scopes
	if len(scopes) == 0 {
		scopes = []string{ScopeReservationsWrite}
	}
	for _, sc := range scopes {
		if !slices.Contains(Scopes, sc) {
			return KeyCreated{}, ErrScope.WithField("body.scopes")
		}
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return KeyCreated{}, err
	}
	secret := hex.EncodeToString(raw)
	key := prefix + secret
	return database.InTxValue(ctx, s.db, func(q database.Querier) (KeyCreated, error) {
		var id uuid.UUID
		if err := q.QueryRow(ctx, `
			INSERT INTO shared.api_keys (tenant_id, name, key_prefix, key_hash, scopes, created_by)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			actor.TenantID, name, secret[:8], hash(key), scopes, actor.UserID).Scan(&id); err != nil {
			return KeyCreated{}, err
		}
		k, err := scanKey(q.QueryRow(ctx, selectKey+` WHERE k.id = $1`, id))
		if err != nil {
			return KeyCreated{}, err
		}
		return KeyCreated{KeyRead: k, Key: key}, audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "api_key.created", Entity: "api_key", EntityID: &id,
			After: map[string]any{"name": name, "prefix": k.Prefix, "scopes": scopes},
		})
	})
}

// Revoke revoca una clave de la empresa (ajena: 404).
func (s *Service) Revoke(ctx context.Context, actor authz.Actor, id uuid.UUID) (KeyRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (KeyRead, error) {
		k, err := scanKey(q.QueryRow(ctx, selectKey+` WHERE k.id = $1 AND k.tenant_id = $2`, id, actor.TenantID))
		if errors.Is(err, pgx.ErrNoRows) {
			return KeyRead{}, ErrNotFound
		}
		if err != nil {
			return KeyRead{}, err
		}
		if k.RevokedAt != nil {
			return KeyRead{}, ErrRevoked
		}
		if _, err := q.Exec(ctx, `UPDATE shared.api_keys SET revoked_at = now() WHERE id = $1`, id); err != nil {
			return KeyRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "api_key.revoked", Entity: "api_key", EntityID: &id,
			Before: map[string]any{"name": k.Name, "prefix": k.Prefix},
		}); err != nil {
			return KeyRead{}, err
		}
		return scanKey(q.QueryRow(ctx, selectKey+` WHERE k.id = $1`, id))
	})
}

// Authenticate resuelve la clave de la cabecera y anota su uso. Una clave
// desconocida, revocada o con formato invalido responde ErrInvalid (401).
func (s *Service) Authenticate(ctx context.Context, raw string) (Key, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, prefix) || len(raw) != len(prefix)+48 {
		return Key{}, ErrInvalid
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Key, error) {
		var k Key
		err := q.QueryRow(ctx, `
			UPDATE shared.api_keys k SET last_used_at = now()
			FROM shared.tenants t
			WHERE k.key_hash = $1 AND k.revoked_at IS NULL AND t.id = k.tenant_id
			RETURNING k.id, k.tenant_id, t.schema_name, t.is_active, t.features,
			          coalesce((SELECT s.status FROM shared.subscriptions s WHERE s.tenant_id = t.id), 'active'),
			          k.name, k.scopes`, hash(raw)).
			Scan(&k.ID, &k.TenantID, &k.TenantSchema, &k.TenantActive, &k.Features, &k.BillingState, &k.Name, &k.Scopes)
		if errors.Is(err, pgx.ErrNoRows) {
			return Key{}, ErrInvalid
		}
		return k, err
	})
}
