// Package locks son las cerraduras inteligentes de las propiedades y los
// codigos de acceso de cada reserva (fase06.md §4.2, Persona B, bandera
// smart_locks).
//
//   - Al confirmar una reserva en una propiedad con cerradura se encola
//     lock_provision: crea un codigo de 6 digitos valido desde la hora de
//     entrada hasta 2 h despues de la salida (hora de la empresa) y lo manda a
//     la cerradura. Si cambian las fechas, el codigo viejo se revoca y se crea
//     otro. Al cancelar o hacer check-out se encola lock_revoke.
//   - El codigo se guarda cifrado; la web solo ve los dos ultimos digitos. El
//     huesped lo ve en "Mi reserva" desde 24 h antes de su entrada.
//   - La cerradura se maneja por la interfaz Provider. Mientras no haya acceso
//     a la API de Nexxt (decision pendiente, avance.md) toda cerradura usa el
//     proveedor simulado (FakeProvider), que acepta cualquier codigo salvo en
//     cerraduras cuyo external_id empieza con "fail" (para probar fallas).
//   - Si la cerradura no acepta el codigo, el trabajo reintenta y se avisa a
//     quien administra reservas (lock_failed) la primera vez.
package locks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/smorareqs/argos-service/internal/core/apperr"
	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/events"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/core/security"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/notifications"
	"github.com/smorareqs/argos-service/internal/modules/reservations"
)

// Trabajos del modulo.
const (
	JobProvision = "lock_provision"
	JobRevoke    = "lock_revoke"
)

// GraceAfterCheckout es cuanto sigue sirviendo el codigo despues de la salida.
const GraceAfterCheckout = 2 * time.Hour

// ShowBefore es desde cuando el huesped ve su codigo antes de entrar.
const ShowBefore = 24 * time.Hour

// Errores del modulo.
var (
	ErrDeviceNotFound      = apperr.New(apperr.KindNotFound, "La cerradura no existe.")
	ErrReservationNotFound = apperr.New(apperr.KindNotFound, "La reserva no existe.")
	ErrPropertyNotFound    = apperr.New(apperr.KindValidation, "La propiedad no existe.").WithField("body.property_id")
	ErrExternalTaken       = apperr.New(apperr.KindAlreadyExists, "Esa cerradura ya está registrada.")
	ErrNotLive             = apperr.New(apperr.KindValidation, "Solo se generan códigos para reservas confirmadas o con el huésped adentro.")
	ErrNoDevices           = apperr.New(apperr.KindValidation, "La propiedad no tiene cerraduras activas.")
)

// Device es una cerradura para el proveedor.
type Device struct {
	ID          uuid.UUID
	Provider    string
	ExternalID  string
	Name        string
	Credentials string // descifradas
}

// Provider habla con la cerradura.
type Provider interface {
	// Provision programa el codigo en la cerradura y devuelve su referencia.
	Provision(ctx context.Context, d Device, code string, from, to time.Time) (string, error)
	// Revoke quita el codigo de la cerradura.
	Revoke(ctx context.Context, d Device, ref string) error
}

// FakeProvider simula la cerradura (local, pruebas y mientras no haya API).
type FakeProvider struct{ Log *slog.Logger }

// Provision acepta el codigo salvo en cerraduras "fail...".
func (f FakeProvider) Provision(ctx context.Context, d Device, code string, from, to time.Time) (string, error) {
	if strings.HasPrefix(d.ExternalID, "fail") {
		return "", errors.New("la cerradura no respondió")
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	ref := "sim-" + hex.EncodeToString(b)
	if f.Log != nil {
		f.Log.InfoContext(ctx, "Cerradura simulada: codigo programado", "device", d.ExternalID, "ref", ref, "from", from, "to", to)
	}
	return ref, nil
}

// Revoke siempre funciona.
func (f FakeProvider) Revoke(ctx context.Context, d Device, ref string) error {
	if f.Log != nil {
		f.Log.InfoContext(ctx, "Cerradura simulada: codigo revocado", "device", d.ExternalID, "ref", ref)
	}
	return nil
}

// DeviceRead es una cerradura (GET /locks/devices).
type DeviceRead struct {
	ID             uuid.UUID `db:"id" json:"id" format:"uuid"`
	PropertyID     uuid.UUID `db:"property_id" json:"property_id" format:"uuid"`
	PropertyCode   string    `db:"property_code" json:"property_code"`
	PropertyName   string    `db:"property_name" json:"property_name"`
	Provider       string    `db:"provider" json:"provider" enum:"nexxt,fake"`
	ExternalID     string    `db:"external_id" json:"external_id" doc:"Id de la cerradura en el proveedor."`
	Name           string    `db:"name" json:"name"`
	IsActive       bool      `db:"is_active" json:"is_active"`
	HasCredentials bool      `db:"has_credentials" json:"has_credentials" doc:"Las credenciales nunca se devuelven."`
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
}

// DeviceCreate es el cuerpo de POST /locks/devices.
type DeviceCreate struct {
	_           struct{}  `additionalProperties:"true"`
	PropertyID  uuid.UUID `json:"property_id" format:"uuid"`
	Name        string    `json:"name" minLength:"1" maxLength:"128" example:"Puerta principal"`
	ExternalID  string    `json:"external_id" minLength:"1" maxLength:"128"`
	Provider    string    `json:"provider,omitempty" enum:"nexxt,fake" doc:"Por defecto nexxt."`
	Credentials string    `json:"credentials,omitempty" maxLength:"4000" doc:"Se guardan cifradas."`
}

// DeviceUpdate es el cuerpo de PATCH /locks/devices/{id}.
type DeviceUpdate struct {
	_           struct{} `additionalProperties:"true"`
	Name        *string  `json:"name,omitempty" minLength:"1" maxLength:"128"`
	ExternalID  *string  `json:"external_id,omitempty" minLength:"1" maxLength:"128"`
	IsActive    *bool    `json:"is_active,omitempty"`
	Credentials *string  `json:"credentials,omitempty" maxLength:"4000" doc:"Vacio las borra."`
}

// CodeRead es un codigo de acceso (sin el codigo completo).
type CodeRead struct {
	ID              uuid.UUID `db:"id" json:"id" format:"uuid"`
	DeviceID        uuid.UUID `db:"device_id" json:"device_id" format:"uuid"`
	DeviceName      string    `db:"device_name" json:"device_name"`
	ReservationID   uuid.UUID `db:"reservation_id" json:"reservation_id" format:"uuid"`
	ReservationCode string    `db:"reservation_code" json:"reservation_code"`
	PropertyName    string    `db:"property_name" json:"property_name"`
	CodeHint        string    `db:"code_hint" json:"code_hint" doc:"••••12"`
	ValidFrom       time.Time `db:"valid_from" json:"valid_from"`
	ValidTo         time.Time `db:"valid_to" json:"valid_to"`
	Status          string    `db:"status" json:"status" enum:"scheduled,active,revoked,failed"`
	LastError       *string   `db:"last_error" json:"last_error" nullable:"true"`
	Attempts        int       `db:"attempts" json:"attempts"`
	UpdatedAt       time.Time `db:"updated_at" json:"updated_at"`
}

// GuestLock es GET /guest/lock: el codigo de la puerta para el huesped.
type GuestLock struct {
	Available  bool       `json:"available" doc:"true: hay codigo y ya se puede mostrar."`
	Code       *string    `json:"code" nullable:"true"`
	DeviceName *string    `json:"device_name" nullable:"true"`
	ValidFrom  *time.Time `json:"valid_from" nullable:"true"`
	ValidTo    *time.Time `json:"valid_to" nullable:"true"`
	ShowsAt    *time.Time `json:"shows_at" nullable:"true" doc:"Desde cuando se vera el codigo."`
	Message    string     `json:"message"`
	HasLock    bool       `json:"has_lock" doc:"La propiedad tiene cerradura inteligente."`
}

// Service son las cerraduras.
type Service struct {
	db        *database.DB
	cipher    *security.FieldCipher
	providers map[string]Provider
	notif     *notifications.Service
	log       *slog.Logger
	now       func() time.Time
}

// NewService crea el servicio con el proveedor simulado para todo.
func NewService(db *database.DB, cipher *security.FieldCipher, notif *notifications.Service, log *slog.Logger) *Service {
	fake := FakeProvider{Log: log}
	return &Service{
		db: db, cipher: cipher, notif: notif, log: log, now: time.Now,
		providers: map[string]Provider{"fake": fake, "nexxt": fake},
	}
}

// SetClock cambia el reloj (pruebas).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// SetProvider cambia el proveedor de un tipo de cerradura.
func (s *Service) SetProvider(name string, p Provider) { s.providers[name] = p }

func (s *Service) provider(name string) Provider {
	if p, ok := s.providers[name]; ok {
		return p
	}
	return s.providers["fake"]
}

const tenantTZ = `coalesce((SELECT timezone FROM shared.tenants WHERE schema_name = current_schema()), 'America/Guatemala')`

// ── Cerraduras ────────────────────────────────────────────

const deviceSelect = `
	SELECT d.id, d.property_id, p.code AS property_code, p.name AS property_name, d.provider, d.external_id,
	       d.name, d.is_active, (d.credentials IS NOT NULL) AS has_credentials, d.created_at
	FROM lock_devices d JOIN properties p ON p.id = d.property_id`

// Devices lista las cerraduras (activas primero).
func (s *Service) Devices(ctx context.Context) ([]DeviceRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]DeviceRead, error) {
		rows, err := q.Query(ctx, deviceSelect+` ORDER BY d.is_active DESC, p.code, d.name`)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[DeviceRead])
	})
}

func deviceByID(ctx context.Context, q database.Querier, id uuid.UUID) (DeviceRead, error) {
	rows, err := q.Query(ctx, deviceSelect+` WHERE d.id = $1`, id)
	if err != nil {
		return DeviceRead{}, err
	}
	d, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[DeviceRead])
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceRead{}, ErrDeviceNotFound
	}
	return d, err
}

func (s *Service) seal(v string) (*string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	enc, err := s.cipher.Encrypt(v)
	if err != nil {
		return nil, err
	}
	return &enc, nil
}

// CreateDevice registra una cerradura.
func (s *Service) CreateDevice(ctx context.Context, actor authz.Actor, in DeviceCreate) (DeviceRead, error) {
	provider := in.Provider
	if provider == "" {
		provider = "nexxt"
	}
	creds, err := s.seal(in.Credentials)
	if err != nil {
		return DeviceRead{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (DeviceRead, error) {
		var ok bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM properties WHERE id = $1 AND deleted_at IS NULL)`, in.PropertyID).Scan(&ok); err != nil {
			return DeviceRead{}, err
		}
		if !ok {
			return DeviceRead{}, ErrPropertyNotFound
		}
		var id uuid.UUID
		err := q.QueryRow(ctx, `
			INSERT INTO lock_devices (property_id, provider, external_id, name, credentials)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			in.PropertyID, provider, strings.TrimSpace(in.ExternalID), strings.TrimSpace(in.Name), creds).Scan(&id)
		if uniqueViolation(err) {
			return DeviceRead{}, ErrExternalTaken
		}
		if err != nil {
			return DeviceRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "lock.device_created", Entity: "lock_device", EntityID: &id, PropertyID: &in.PropertyID,
		}); err != nil {
			return DeviceRead{}, err
		}
		return deviceByID(ctx, q, id)
	})
}

// UpdateDevice cambia nombre, id externo, credenciales o la apaga.
func (s *Service) UpdateDevice(ctx context.Context, actor authz.Actor, id uuid.UUID, in DeviceUpdate) (DeviceRead, error) {
	var creds *string
	clear := false
	if in.Credentials != nil {
		c, err := s.seal(*in.Credentials)
		if err != nil {
			return DeviceRead{}, err
		}
		creds, clear = c, c == nil
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (DeviceRead, error) {
		d, err := deviceByID(ctx, q, id)
		if err != nil {
			return DeviceRead{}, err
		}
		_, err = q.Exec(ctx, `
			UPDATE lock_devices SET
				name = coalesce($2, name), external_id = coalesce($3, external_id), is_active = coalesce($4, is_active),
				credentials = CASE WHEN $6::bool THEN NULL ELSE coalesce($5, credentials) END, updated_at = now()
			WHERE id = $1`, id, trimmed(in.Name), trimmed(in.ExternalID), in.IsActive, creds, clear)
		if uniqueViolation(err) {
			return DeviceRead{}, ErrExternalTaken
		}
		if err != nil {
			return DeviceRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &actor.UserID, Action: "lock.device_updated", Entity: "lock_device", EntityID: &id, PropertyID: &d.PropertyID,
		}); err != nil {
			return DeviceRead{}, err
		}
		return deviceByID(ctx, q, id)
	})
}

// ── Codigos ───────────────────────────────────────────────

const codeSelect = `
	SELECT c.id, c.device_id, d.name AS device_name, c.reservation_id, r.code AS reservation_code, p.name AS property_name,
	       c.code_hint, c.valid_from, c.valid_to, c.status, c.last_error, c.attempts, c.updated_at
	FROM lock_access_codes c
	JOIN lock_devices d ON d.id = c.device_id
	JOIN reservations r ON r.id = c.reservation_id
	JOIN properties p ON p.id = r.property_id`

// Codes lista los codigos (de una reserva o los 200 mas recientes).
func (s *Service) Codes(ctx context.Context, reservationID *uuid.UUID) ([]CodeRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) ([]CodeRead, error) {
		rows, err := q.Query(ctx, codeSelect+`
			WHERE ($1::uuid IS NULL OR c.reservation_id = $1)
			ORDER BY c.created_at DESC LIMIT 200`, reservationID)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowToStructByName[CodeRead])
	})
}

type stay struct {
	PropertyID uuid.UUID
	Code       string
	Status     string
	From, To   time.Time
}

func loadStay(ctx context.Context, q database.Querier, reservationID uuid.UUID) (stay, error) {
	var st stay
	err := q.QueryRow(ctx, `
		SELECT r.property_id, r.code, r.status,
		       ((r.check_in_date + r.check_in_time) AT TIME ZONE `+tenantTZ+`),
		       ((r.check_out_date + r.check_out_time) AT TIME ZONE `+tenantTZ+`)
		FROM reservations r WHERE r.id = $1`, reservationID).Scan(&st.PropertyID, &st.Code, &st.Status, &st.From, &st.To)
	if errors.Is(err, pgx.ErrNoRows) {
		return stay{}, ErrReservationNotFound
	}
	st.To = st.To.Add(GraceAfterCheckout)
	return st, err
}

type codeRow struct {
	ID        uuid.UUID
	DeviceID  uuid.UUID
	Status    string
	Ref       *string
	Code      string
	From, To  time.Time
	Attempts  int
	HasDevice bool
}

type plan struct {
	device Device
	row    *codeRow // el ultimo codigo vivo o fallido de esa cerradura
}

// Provision crea o renueva los codigos de una reserva en las cerraduras de su
// propiedad. Devuelve los codigos y un error si alguna cerradura fallo (el
// trabajo reintenta).
func (s *Service) Provision(ctx context.Context, tenantID, reservationID uuid.UUID) ([]CodeRead, error) {
	var st stay
	var plans []plan
	err := s.db.InTx(ctx, func(q database.Querier) error {
		var err error
		if st, err = loadStay(ctx, q, reservationID); err != nil {
			return err
		}
		if st.Status != "confirmed" && st.Status != "checked_in" {
			return ErrNotLive
		}
		rows, err := q.Query(ctx, `
			SELECT d.id, d.provider, d.external_id, d.name, coalesce(d.credentials, ''),
			       c.id, c.status, c.provider_ref, c.code, c.valid_from, c.valid_to, c.attempts
			FROM lock_devices d
			LEFT JOIN LATERAL (
				SELECT * FROM lock_access_codes x
				WHERE x.device_id = d.id AND x.reservation_id = $2 AND x.status <> 'revoked'
				ORDER BY x.created_at DESC LIMIT 1) c ON true
			WHERE d.property_id = $1 AND d.is_active
			ORDER BY d.name`, st.PropertyID, reservationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p plan
			var creds string
			var cid *uuid.UUID
			var status, code *string
			var ref *string
			var from, to *time.Time
			var attempts *int
			if err := rows.Scan(&p.device.ID, &p.device.Provider, &p.device.ExternalID, &p.device.Name, &creds,
				&cid, &status, &ref, &code, &from, &to, &attempts); err != nil {
				return err
			}
			if creds != "" {
				if p.device.Credentials, err = s.cipher.Decrypt(creds); err != nil {
					return err
				}
			}
			if cid != nil {
				plain, err := s.cipher.Decrypt(*code)
				if err != nil {
					return err
				}
				p.row = &codeRow{ID: *cid, DeviceID: p.device.ID, Status: *status, Ref: ref, Code: plain, From: *from, To: *to, Attempts: *attempts}
			}
			plans = append(plans, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if len(plans) == 0 {
		return []CodeRead{}, nil
	}
	var failures []string
	for _, p := range plans {
		r := p.row
		sameWindow := r != nil && r.From.Equal(st.From) && r.To.Equal(st.To)
		if r != nil && r.Status == "active" && sameWindow {
			continue
		}
		if r != nil && r.Status == "active" {
			// Cambiaron las fechas: se quita el viejo y se crea otro.
			if err := s.revokeOne(ctx, p.device, *r); err != nil {
				failures = append(failures, p.device.Name)
				continue
			}
			r = nil
		}
		if r == nil {
			code, err := newCode()
			if err != nil {
				return nil, err
			}
			enc, err := s.cipher.Encrypt(code)
			if err != nil {
				return nil, err
			}
			var id uuid.UUID
			if err := s.db.InTx(ctx, func(q database.Querier) error {
				return q.QueryRow(ctx, `
					INSERT INTO lock_access_codes (device_id, reservation_id, code, code_hint, valid_from, valid_to)
					VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
					p.device.ID, reservationID, enc, code[len(code)-2:], st.From, st.To).Scan(&id)
			}); err != nil {
				return nil, err
			}
			r = &codeRow{ID: id, DeviceID: p.device.ID, Status: "scheduled", Code: code, From: st.From, To: st.To}
		}
		ref, perr := s.provider(p.device.Provider).Provision(ctx, p.device, r.Code, st.From, st.To)
		if err := s.db.InTx(ctx, func(q database.Querier) error {
			if perr == nil {
				_, err := q.Exec(ctx, `
					UPDATE lock_access_codes SET status = 'active', provider_ref = $2, last_error = NULL,
						valid_from = $3, valid_to = $4, attempts = attempts + 1, updated_at = now()
					WHERE id = $1`, r.ID, ref, st.From, st.To)
				return err
			}
			var attempts int
			if err := q.QueryRow(ctx, `
				UPDATE lock_access_codes SET status = 'failed', last_error = $2, valid_from = $3, valid_to = $4,
					attempts = attempts + 1, updated_at = now()
				WHERE id = $1 RETURNING attempts`, r.ID, perr.Error(), st.From, st.To).Scan(&attempts); err != nil {
				return err
			}
			if attempts == 1 && s.notif != nil {
				staff, err := notifications.Holders(ctx, q, tenantID, &st.PropertyID, authz.ReservationsManage)
				if err != nil {
					return err
				}
				return s.notif.Notify(ctx, q, tenantID, notifications.Notice{
					Recipients: staff, Type: notifications.TypeLockFailed,
					Title: "La cerradura no aceptó el código",
					Body:  fmt.Sprintf("%s no recibió el código de la reserva %s. Lo seguimos intentando; si no se resuelve, entrega la llave a mano.", p.device.Name, st.Code),
					Route: "/reservations/" + reservationID.String(), AppRoute: "/reservations/" + reservationID.String(),
					Data:     map[string]any{"reservation_id": reservationID.String()},
					Channels: []string{notifications.ChannelPush, notifications.ChannelEmail},
				})
			}
			return nil
		}); err != nil {
			return nil, err
		}
		if perr != nil {
			failures = append(failures, p.device.Name)
		}
	}
	codes, err := s.Codes(ctx, &reservationID)
	if err != nil {
		return nil, err
	}
	if len(failures) > 0 {
		return codes, fmt.Errorf("cerraduras sin codigo: %s", strings.Join(failures, ", "))
	}
	return codes, nil
}

func (s *Service) revokeOne(ctx context.Context, d Device, r codeRow) error {
	if r.Status == "active" && r.Ref != nil {
		if err := s.provider(d.Provider).Revoke(ctx, d, *r.Ref); err != nil {
			return err
		}
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		_, err := q.Exec(ctx, `UPDATE lock_access_codes SET status = 'revoked', updated_at = now() WHERE id = $1`, r.ID)
		return err
	})
}

// Revoke quita todos los codigos vivos de la reserva.
func (s *Service) Revoke(ctx context.Context, reservationID uuid.UUID) error {
	type item struct {
		device Device
		row    codeRow
	}
	var list []item
	err := s.db.InTx(ctx, func(q database.Querier) error {
		rows, err := q.Query(ctx, `
			SELECT d.id, d.provider, d.external_id, d.name, coalesce(d.credentials, ''), c.id, c.status, c.provider_ref
			FROM lock_access_codes c JOIN lock_devices d ON d.id = c.device_id
			WHERE c.reservation_id = $1 AND c.status <> 'revoked'`, reservationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it item
			var creds string
			if err := rows.Scan(&it.device.ID, &it.device.Provider, &it.device.ExternalID, &it.device.Name, &creds,
				&it.row.ID, &it.row.Status, &it.row.Ref); err != nil {
				return err
			}
			if creds != "" {
				if it.device.Credentials, err = s.cipher.Decrypt(creds); err != nil {
					return err
				}
			}
			list = append(list, it)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	var failed []string
	for _, it := range list {
		if err := s.revokeOne(ctx, it.device, it.row); err != nil {
			failed = append(failed, it.device.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("no se pudo revocar en: %s", strings.Join(failed, ", "))
	}
	return nil
}

// Reprovision corre Provision desde la web (el error de una cerradura no
// es 500: queda en el codigo con status failed).
func (s *Service) Reprovision(ctx context.Context, actor authz.Actor, reservationID uuid.UUID) ([]CodeRead, error) {
	var has bool
	if err := s.db.InTx(ctx, func(q database.Querier) error {
		return q.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM lock_devices d JOIN reservations r ON r.property_id = d.property_id
			               WHERE r.id = $1 AND d.is_active)`, reservationID).Scan(&has)
	}); err != nil {
		return nil, err
	}
	if !has {
		return nil, ErrNoDevices
	}
	codes, err := s.Provision(ctx, actor.TenantID, reservationID)
	var ae *apperr.Error
	if err != nil && !errors.As(err, &ae) {
		// Una cerradura fallo: queda en su codigo (failed) y no es un 500.
		s.log.WarnContext(ctx, "Cerraduras con falla al reprogramar", "reservation_id", reservationID, "error", err)
		return codes, nil
	}
	return codes, err
}

// ForGuest es el codigo que ve el huesped de su reserva.
func (s *Service) ForGuest(ctx context.Context, tenantID, reservationID uuid.UUID) (GuestLock, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (GuestLock, error) {
		var on bool
		if err := q.QueryRow(ctx, `SELECT coalesce((features->>'smart_locks')::bool, false) FROM shared.tenants WHERE id = $1`, tenantID).Scan(&on); err != nil {
			return GuestLock{}, err
		}
		st, err := loadStay(ctx, q, reservationID)
		if err != nil {
			return GuestLock{}, err
		}
		var hasLock bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM lock_devices WHERE property_id = $1 AND is_active)`, st.PropertyID).Scan(&hasLock); err != nil {
			return GuestLock{}, err
		}
		if !on || !hasLock {
			return GuestLock{Message: "Esta propiedad se abre con llave. La recepción te la entrega al llegar."}, nil
		}
		shows := st.From.Add(-ShowBefore)
		out := GuestLock{HasLock: true, ShowsAt: &shows}
		var name, enc string
		var from, to time.Time
		err = q.QueryRow(ctx, `
			SELECT d.name, c.code, c.valid_from, c.valid_to FROM lock_access_codes c JOIN lock_devices d ON d.id = c.device_id
			WHERE c.reservation_id = $1 AND c.status = 'active' ORDER BY d.name LIMIT 1`, reservationID).Scan(&name, &enc, &from, &to)
		now := s.now()
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if now.Before(shows) {
				out.Message = "Tu código de la puerta aparece aquí 24 horas antes de tu llegada."
			} else {
				out.Message = "Estamos preparando tu código. Si llegas y no aparece, escríbenos por el chat."
			}
			return out, nil
		case err != nil:
			return GuestLock{}, err
		}
		out.DeviceName, out.ValidFrom, out.ValidTo = &name, &from, &to
		if now.Before(shows) {
			out.Message = "Tu código de la puerta aparece aquí 24 horas antes de tu llegada."
			return out, nil
		}
		if now.After(to) {
			out.Message = "Tu código ya venció."
			return out, nil
		}
		code, err := s.cipher.Decrypt(enc)
		if err != nil {
			return GuestLock{}, err
		}
		out.Available, out.Code = true, &code
		out.Message = "Marca el código en el teclado de la puerta y luego la tecla ✓."
		return out, nil
	})
}

// ── Reservas y trabajos ───────────────────────────────────

type jobPayload struct {
	ReservationID uuid.UUID `json:"reservation_id"`
}

// Subscribe engancha las reservas: confirmar o mover fechas programa el
// codigo; cancelar o salir lo revoca.
func (s *Service) Subscribe(bus *events.Bus) {
	bus.Subscribe(reservations.EventConfirmed, s.onLive)
	bus.Subscribe(reservations.EventUpdated, s.onUpdated)
	bus.Subscribe(reservations.EventCancelled, s.onEnd)
	bus.Subscribe(reservations.EventCheckedOut, s.onEnd)
}

func reservationOf(e events.Event) (reservations.ReservationEvent, bool) {
	switch d := e.Data.(type) {
	case reservations.ReservationEvent:
		return d, true
	case *reservations.ReservationEvent:
		if d != nil {
			return *d, true
		}
	}
	return reservations.ReservationEvent{}, false
}

func enqueueIfLocks(ctx context.Context, q database.Querier, e events.Event, typ string, r reservations.ReservationRead) error {
	var ok bool
	if err := q.QueryRow(ctx, `
		SELECT coalesce((SELECT (features->>'smart_locks')::bool FROM shared.tenants WHERE id = $1), false)
		   AND EXISTS (SELECT 1 FROM lock_devices WHERE property_id = $2 AND is_active)`, e.TenantID, r.PropertyID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return jobs.Enqueue(ctx, q, jobs.Job{
		Type: typ, TenantID: &e.TenantID, Payload: jobPayload{ReservationID: r.ID}, MaxAttempts: 5,
		DedupeKey: typ + ":" + r.ID.String(),
	})
}

func (s *Service) onLive(ctx context.Context, q database.Querier, e events.Event) error {
	ev, ok := reservationOf(e)
	if !ok {
		return nil
	}
	return enqueueIfLocks(ctx, q, e, JobProvision, ev.Reservation)
}

func (s *Service) onUpdated(ctx context.Context, q database.Querier, e events.Event) error {
	ev, ok := reservationOf(e)
	if !ok {
		return nil
	}
	r := ev.Reservation
	if r.Status != "confirmed" && r.Status != "checked_in" {
		return nil
	}
	if p := ev.Previous; p != nil && p.CheckInDate == r.CheckInDate && p.CheckOutDate == r.CheckOutDate &&
		p.CheckInTime == r.CheckInTime && p.CheckOutTime == r.CheckOutTime && p.PropertyID == r.PropertyID {
		return nil
	}
	return enqueueIfLocks(ctx, q, e, JobProvision, r)
}

func (s *Service) onEnd(ctx context.Context, q database.Querier, e events.Event) error {
	ev, ok := reservationOf(e)
	if !ok {
		return nil
	}
	var live bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM lock_access_codes WHERE reservation_id = $1 AND status <> 'revoked')`,
		ev.Reservation.ID).Scan(&live); err != nil || !live {
		return err
	}
	return jobs.Enqueue(ctx, q, jobs.Job{
		Type: JobRevoke, TenantID: &e.TenantID, Payload: jobPayload{ReservationID: ev.Reservation.ID}, MaxAttempts: 5,
		DedupeKey: JobRevoke + ":" + ev.Reservation.ID.String(),
	})
}

// HandleJob corre lock_provision y lock_revoke (con el schema de la empresa).
func (s *Service) HandleJob(ctx context.Context, job jobs.Running) error {
	var p jobPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return fmt.Errorf("payload de cerradura invalido: %w", err)
	}
	if job.TenantID == nil {
		return errors.New("trabajo de cerradura sin empresa")
	}
	switch job.Type {
	case JobProvision:
		_, err := s.Provision(ctx, *job.TenantID, p.ReservationID)
		if errors.Is(err, ErrNotLive) || errors.Is(err, ErrReservationNotFound) {
			return nil // ya se cancelo: no hay nada que programar
		}
		return err
	case JobRevoke:
		return s.Revoke(ctx, p.ReservationID)
	}
	return fmt.Errorf("trabajo desconocido %s", job.Type)
}

// ── Utilidades ────────────────────────────────────────────

func newCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func trimmed(v *string) *string {
	if v == nil {
		return nil
	}
	t := strings.TrimSpace(*v)
	return &t
}

func uniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
