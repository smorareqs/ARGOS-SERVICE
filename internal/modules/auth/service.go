package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/security"
	"github.com/smorareqs/argos-service/internal/core/storage"
	"github.com/smorareqs/argos-service/internal/modules/authz"
)

// Tiempos de sesion (fase01.md §3.7). El access token dura lo que indique el
// TokenManager (15 min por defecto).
const (
	selectionTTL  = 5 * time.Minute
	webRefreshTTL = 30 * 24 * time.Hour
)

// Claims propios del access token.
const (
	claimTenant  = "tid" // empresa activa; ausente en modo plataforma
	claimSession = "sid" // familia del refresh: identifica la sesion
	claimVersion = "pv"  // version de permisos del rol
)

// Options ajusta el servicio.
type Options struct {
	// RefreshTTL es la vigencia de la sesion en movil; en web se limita a 30 dias.
	RefreshTTL time.Duration
	// Cipher cifra el secreto TOTP en reposo.
	Cipher *security.FieldCipher
	// Storage firma las URLs de los logos de empresa (puede ser nil).
	Storage *storage.Service
	// WebBaseURL arma los enlaces de los correos (invitaciones, contrasena).
	WebBaseURL string
}

// Service contiene la logica de autenticacion: login multiempresa, sesiones
// rotativas por dispositivo y resolucion del usuario autenticado. Es
// agnostico a HTTP: devuelve errores de dominio que la capa API traduce.
type Service struct {
	db         *database.DB
	tokens     *security.TokenManager
	log        *slog.Logger
	refreshTTL time.Duration
	limiter    *attemptLimiter
	now        func() time.Time
	cipher     *security.FieldCipher
	storage    *storage.Service
	webBase    string
	limits     Limits
}

// Limits verifica los limites del plan antes de crear (fase 6). Lo implementa
// billing.Service; sin el, no hay limites.
type Limits interface {
	CheckLimit(ctx context.Context, q database.Querier, tenantID uuid.UUID, kind, field string) error
}

// SetLimits engancha los limites del plan.
func (s *Service) SetLimits(l Limits) { s.limits = l }

// NewService crea el servicio.
func NewService(db *database.DB, tokens *security.TokenManager, log *slog.Logger, opts Options) *Service {
	if opts.RefreshTTL <= 0 {
		opts.RefreshTTL = 60 * 24 * time.Hour
	}
	if opts.WebBaseURL == "" {
		opts.WebBaseURL = "http://localhost:4200"
	}
	return &Service{
		db: db, tokens: tokens, log: log, refreshTTL: opts.RefreshTTL,
		limiter: newAttemptLimiter(time.Now), now: time.Now,
		cipher: opts.Cipher, storage: opts.Storage, webBase: strings.TrimRight(opts.WebBaseURL, "/"),
	}
}

// CreateUser crea un usuario con contrasena (seeders y alta de plataforma).
func (s *Service) CreateUser(ctx context.Context, in UserCreate) (User, error) {
	in, err := in.normalized()
	if err != nil {
		return User{}, err
	}
	hashed, err := security.HashPassword(in.Password)
	if err != nil {
		return User{}, err
	}
	var phone *string
	if in.Phone != "" {
		phone = &in.Phone
	}
	user, err := database.InTxValue(ctx, s.db, func(q database.Querier) (User, error) {
		repo := NewRepository(q)
		existing, err := repo.GetByEmail(ctx, in.Email)
		if err != nil {
			return User{}, err
		}
		if existing != nil {
			return User{}, ErrEmailAlreadyExists
		}
		return repo.Add(ctx, User{
			Email: in.Email, HashedPassword: &hashed, FullName: in.FullName, Phone: phone,
			IsActive: true, IsPlatformAdmin: in.IsPlatformAdmin,
		})
	})
	if err != nil {
		return User{}, err
	}
	s.log.InfoContext(ctx, "Usuario creado", "user_id", user.ID)
	return user, nil
}

// UserByEmail devuelve el usuario o ErrUserNotFound.
func (s *Service) UserByEmail(ctx context.Context, email string) (User, error) {
	user, err := database.InTxValue(ctx, s.db, func(q database.Querier) (*User, error) {
		return NewRepository(q).GetByEmail(ctx, strings.TrimSpace(email))
	})
	if err != nil {
		return User{}, err
	}
	if user == nil {
		return User{}, ErrUserNotFound
	}
	return *user, nil
}

// SetPassword reemplaza la contrasena del usuario.
func (s *Service) SetPassword(ctx context.Context, userID uuid.UUID, password string) error {
	if err := validatePassword(password); err != nil {
		return err
	}
	hashed, err := security.HashPassword(password)
	if err != nil {
		return err
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		return NewRepository(q).SetPassword(ctx, userID, hashed)
	})
}

// Login valida credenciales y abre la sesion: con una sola empresa entra
// directo; con varias devuelve un selection_token para elegir.
func (s *Service) Login(ctx context.Context, in LoginRequest, ip string) (SessionResponse, error) {
	device, err := in.Device.normalized()
	if err != nil {
		return SessionResponse{}, err
	}
	user, err := s.checkCredentials(ctx, in, ip)
	if err != nil {
		return SessionResponse{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (SessionResponse, error) {
		usable, err := usableMemberships(ctx, q, user.ID)
		if err != nil {
			return SessionResponse{}, err
		}
		if err := NewRepository(q).TouchLogin(ctx, user.ID); err != nil {
			return SessionResponse{}, err
		}
		switch {
		case len(usable) == 1:
			return s.openSession(ctx, q, *user, &usable[0], device, ip, usable)
		case len(usable) > 1:
			selection, err := s.tokens.CreateToken(user.ID.String(), security.TokenSelection, selectionTTL, nil)
			if err != nil {
				return SessionResponse{}, err
			}
			return SessionResponse{
				Status: StatusSelectTenant, SelectionToken: selection, Memberships: s.reads(ctx, usable),
				PlatformAdmin: user.IsPlatformAdmin,
			}, nil
		case user.IsPlatformAdmin:
			return s.openSession(ctx, q, *user, nil, device, ip, usable)
		default:
			return SessionResponse{}, ErrNoActiveMemberships
		}
	})
}

// checkCredentials valida correo, contrasena y segundo factor aplicando el
// bloqueo por intentos fallidos (correo + IP).
func (s *Service) checkCredentials(ctx context.Context, in LoginRequest, ip string) (*User, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	key := email + "|" + ip
	if wait := s.limiter.blockedFor(key); wait > 0 {
		return nil, ErrTooManyAttempts.WithRetryAfter(wait)
	}
	user, err := database.InTxValue(ctx, s.db, func(q database.Querier) (*User, error) {
		return NewRepository(q).GetByEmail(ctx, email)
	})
	if err != nil {
		return nil, err
	}
	if user == nil || user.HashedPassword == nil || !security.VerifyPassword(in.Password, *user.HashedPassword) {
		s.limiter.fail(key)
		return nil, ErrInvalidCredentials
	}
	if !user.IsActive {
		return nil, ErrInactiveUser
	}
	if user.TOTPEnabled {
		if strings.TrimSpace(in.TOTPCode) == "" {
			return nil, ErrTOTPRequired
		}
		ok, err := s.verifyTOTP(ctx, user.ID, in.TOTPCode)
		if err != nil {
			return nil, err
		}
		if !ok {
			s.limiter.fail(key)
			return nil, ErrInvalidTOTP
		}
	}
	s.limiter.reset(key)
	return user, nil
}

// SelectTenant abre (tras el login) o cambia (con sesion abierta) la empresa
// activa.
func (s *Service) SelectTenant(ctx context.Context, in SelectTenantRequest, ip string) (SessionResponse, error) {
	switch {
	case in.TenantID == uuid.Nil && !in.Platform:
		return SessionResponse{}, ErrInvalidSelection
	case in.SelectionToken != "" && in.Device != nil:
		return s.selectAfterLogin(ctx, in, ip)
	case in.RefreshToken != "":
		return s.switchTenant(ctx, in, ip)
	default:
		return SessionResponse{}, ErrInvalidSelection
	}
}

func (s *Service) selectAfterLogin(ctx context.Context, in SelectTenantRequest, ip string) (SessionResponse, error) {
	device, err := in.Device.normalized()
	if err != nil {
		return SessionResponse{}, err
	}
	claims, err := s.tokens.Decode(in.SelectionToken)
	if err != nil || claims["type"] != string(security.TokenSelection) {
		return SessionResponse{}, ErrInvalidToken
	}
	userID, err := subject(claims)
	if err != nil {
		return SessionResponse{}, ErrInvalidToken
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (SessionResponse, error) {
		user, err := activeUser(ctx, q, userID)
		if err != nil {
			return SessionResponse{}, err
		}
		usable, err := usableMemberships(ctx, q, user.ID)
		if err != nil {
			return SessionResponse{}, err
		}
		chosen, err := chooseTarget(user, usable, in)
		if err != nil {
			return SessionResponse{}, err
		}
		return s.openSession(ctx, q, user, chosen, device, ip, usable)
	})
}

func (s *Service) switchTenant(ctx context.Context, in SelectTenantRequest, ip string) (SessionResponse, error) {
	refresh := in.RefreshToken
	out, err := database.InTxValue(ctx, s.db, func(q database.Querier) (SessionResponse, error) {
		repo := NewRepository(q)
		current, err := s.validRefresh(ctx, repo, refresh, "")
		if err != nil {
			return SessionResponse{}, err
		}
		user, err := activeUser(ctx, q, current.UserID)
		if err != nil {
			return SessionResponse{}, err
		}
		usable, err := usableMemberships(ctx, q, user.ID)
		if err != nil {
			return SessionResponse{}, err
		}
		chosen, err := chooseTarget(user, usable, in)
		if err != nil {
			return SessionResponse{}, err
		}
		if err := repo.RevokeRefreshToken(ctx, current.ID); err != nil {
			return SessionResponse{}, err
		}
		access, refreshToken, err := s.issue(ctx, repo, user, chosen, current.FamilyID, deviceOf(*current), ip)
		if err != nil {
			return SessionResponse{}, err
		}
		return s.authenticated(ctx, user, access, refreshToken, usable), nil
	})
	return out, s.revokeIfReused(ctx, err)
}

// chooseTarget resuelve la empresa elegida (o el modo plataforma, que exige
// ser administrador de ARGOS). nil = modo plataforma.
func chooseTarget(user User, usable []authz.MembershipView, in SelectTenantRequest) (*authz.MembershipView, error) {
	if in.Platform {
		if !user.IsPlatformAdmin {
			return nil, ErrPlatformNotAllowed
		}
		return nil, nil
	}
	chosen := findMembership(usable, in.TenantID)
	if chosen == nil {
		return nil, ErrNoMembership
	}
	return chosen, nil
}

// Refresh rota el refresh token: revoca el presentado y emite uno nuevo de la
// misma sesion. Si el presentado ya habia sido rotado (posible robo), revoca
// la sesion entera.
func (s *Service) Refresh(ctx context.Context, in RefreshRequest, ip string) (RefreshResponse, error) {
	out, err := database.InTxValue(ctx, s.db, func(q database.Querier) (RefreshResponse, error) {
		repo := NewRepository(q)
		current, err := s.validRefresh(ctx, repo, in.RefreshToken, strings.TrimSpace(in.DeviceID))
		if err != nil {
			return RefreshResponse{}, err
		}
		user, err := activeUser(ctx, q, current.UserID)
		if err != nil {
			return RefreshResponse{}, err
		}
		var membership *authz.MembershipView
		if current.TenantID != nil {
			m, err := authz.NewRepository(q).MembershipOfUser(ctx, user.ID, *current.TenantID)
			if err != nil {
				return RefreshResponse{}, err
			}
			if m == nil || !m.Usable() {
				return RefreshResponse{}, ErrInvalidToken
			}
			membership = m
		} else if !user.IsPlatformAdmin {
			return RefreshResponse{}, ErrInvalidToken
		}
		if err := repo.RevokeRefreshToken(ctx, current.ID); err != nil {
			return RefreshResponse{}, err
		}
		access, refreshToken, err := s.issue(ctx, repo, user, membership, current.FamilyID, deviceOf(*current), ip)
		if err != nil {
			return RefreshResponse{}, err
		}
		return RefreshResponse{AccessToken: access, RefreshToken: refreshToken, ExpiresIn: s.expiresIn()}, nil
	})
	return out, s.revokeIfReused(ctx, err)
}

// reusedError indica que se presento un refresh ya rotado de una sesion
// vigente. Se presenta al cliente como token invalido (401).
type reusedError struct{ family uuid.UUID }

func (e *reusedError) Error() string { return ErrInvalidToken.Error() }
func (e *reusedError) Unwrap() error { return ErrInvalidToken }

// validRefresh devuelve la sesion vigente del token.
func (s *Service) validRefresh(ctx context.Context, repo *Repository, token, deviceID string) (*RefreshToken, error) {
	current, err := repo.RefreshTokenByHash(ctx, hashToken(token))
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, ErrInvalidToken
	}
	if current.RevokedAt != nil {
		active, err := repo.FamilyActive(ctx, current.FamilyID)
		if err != nil {
			return nil, err
		}
		if active {
			return nil, &reusedError{family: current.FamilyID}
		}
		return nil, ErrInvalidToken
	}
	if !current.ExpiresAt.After(s.now()) || (deviceID != "" && deviceID != current.DeviceID) {
		return nil, ErrInvalidToken
	}
	return current, nil
}

// revokeIfReused revoca la sesion comprometida en su propia transaccion (la de
// la operacion ya se revirtio) y devuelve el error para el cliente.
func (s *Service) revokeIfReused(ctx context.Context, err error) error {
	var reused *reusedError
	if !errors.As(err, &reused) {
		return err
	}
	s.log.WarnContext(ctx, "Refresh token reutilizado: se revoca la sesion", "session_id", reused.family)
	if rerr := s.db.InTx(ctx, func(q database.Querier) error {
		return NewRepository(q).RevokeFamily(ctx, reused.family)
	}); rerr != nil {
		return rerr
	}
	return ErrInvalidToken
}

// Logout revoca la sesion del refresh token. No falla si ya estaba revocada o
// no existe.
func (s *Service) Logout(ctx context.Context, refresh string) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		repo := NewRepository(q)
		current, err := repo.RefreshTokenByHash(ctx, hashToken(refresh))
		if err != nil || current == nil {
			return err
		}
		return repo.RevokeFamily(ctx, current.FamilyID)
	})
}

// Me arma la respuesta de GET /auth/me para el principal.
func (s *Service) Me(ctx context.Context, p Principal) (MeResponse, error) {
	usable, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]authz.MembershipView, error) {
		return usableMemberships(ctx, q, p.User.ID)
	})
	if err != nil {
		return MeResponse{}, err
	}
	out := MeResponse{
		User: MeUser{
			ID: p.User.ID, Email: p.User.Email, FullName: p.User.FullName, Phone: p.User.Phone,
			TOTPEnabled: p.User.TOTPEnabled, IsPlatformAdmin: p.User.IsPlatformAdmin,
		},
		Permissions: make([]authz.Permission, 0, len(p.Permissions)),
		Memberships: s.reads(ctx, usable),
	}
	for _, perm := range authz.All {
		if p.Permissions[perm] {
			out.Permissions = append(out.Permissions, perm)
		}
	}
	if m := p.Membership; m != nil {
		features := make(map[string]bool, len(authz.Features))
		for _, f := range authz.Features {
			features[string(f)] = m.TenantFeatures[string(f)]
		}
		out.Tenant = &MeTenant{
			ID: m.TenantID, Name: m.TenantName, LogoURL: s.storage.LogoURL(ctx, m.TenantID, m.TenantLogoKey),
			Timezone: m.TenantTimezone,
			Currency: strings.TrimSpace(m.TenantCurrency), Features: features,
			BillingStatus: m.TenantBillingStatus, ReadOnly: m.ReadOnly(),
		}
		out.Role = &MeRole{Key: m.RoleKey, Name: m.RoleName}
	}
	return out, nil
}

// ResolvePrincipal valida un access token y carga quien hace la peticion. Un
// token deja de valer si el usuario se desactiva, pierde la membresia, el
// tenant se desactiva o cambian los permisos de su rol (claim pv).
func (s *Service) ResolvePrincipal(ctx context.Context, token string) (Principal, error) {
	claims, err := s.parseAccess(token)
	if err != nil {
		return Principal{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (Principal, error) {
		user, err := activeUser(ctx, q, claims.user)
		if err != nil {
			return Principal{}, err
		}
		p := Principal{User: user, SessionID: claims.session, Permissions: map[authz.Permission]bool{}}
		if claims.tenant == nil {
			if !user.IsPlatformAdmin {
				return Principal{}, ErrInvalidToken
			}
			return p, nil
		}
		repo := authz.NewRepository(q)
		m, err := repo.MembershipOfUser(ctx, user.ID, *claims.tenant)
		if err != nil {
			return Principal{}, err
		}
		if m == nil || !m.Usable() || claims.version != m.PermissionsVersion {
			return Principal{}, ErrInvalidToken
		}
		perms, err := repo.Permissions(ctx, m.RoleID)
		if err != nil {
			return Principal{}, err
		}
		for _, perm := range perms {
			p.Permissions[perm] = true
		}
		p.Membership = m
		return p, nil
	})
}

// accessClaims son los datos de un access token valido.
type accessClaims struct {
	user    uuid.UUID
	session uuid.UUID
	tenant  *uuid.UUID // nil en modo plataforma
	version int
}

func (s *Service) parseAccess(token string) (accessClaims, error) {
	claims, err := s.tokens.Decode(token)
	if err != nil || claims["type"] != string(security.TokenAccess) {
		return accessClaims{}, ErrInvalidToken
	}
	user, err := subject(claims)
	if err != nil {
		return accessClaims{}, ErrInvalidToken
	}
	session, err := uuid.Parse(stringClaim(claims, claimSession))
	if err != nil {
		return accessClaims{}, ErrInvalidToken
	}
	out := accessClaims{user: user, session: session}
	if raw := stringClaim(claims, claimTenant); raw != "" {
		tenant, err := uuid.Parse(raw)
		version, ok := claims[claimVersion].(float64)
		if err != nil || !ok {
			return accessClaims{}, ErrInvalidToken
		}
		out.tenant, out.version = &tenant, int(version)
	}
	return out, nil
}

// openSession crea una sesion nueva (familia nueva) y devuelve sus tokens.
func (s *Service) openSession(
	ctx context.Context, q database.Querier, user User, m *authz.MembershipView,
	device Device, ip string, usable []authz.MembershipView,
) (SessionResponse, error) {
	access, refresh, err := s.issue(ctx, NewRepository(q), user, m, uuid.New(), device, ip)
	if err != nil {
		return SessionResponse{}, err
	}
	return s.authenticated(ctx, user, access, refresh, usable), nil
}

// issue emite el access token y guarda un refresh token nuevo en la familia.
func (s *Service) issue(
	ctx context.Context, repo *Repository, user User, m *authz.MembershipView,
	family uuid.UUID, device Device, ip string,
) (access, refresh string, err error) {
	claims := map[string]any{claimSession: family.String()}
	var tenantID *uuid.UUID
	if m != nil {
		claims[claimTenant] = m.TenantID.String()
		claims[claimVersion] = m.PermissionsVersion
		tenantID = &m.TenantID
	}
	access, err = s.tokens.CreateAccessToken(user.ID.String(), claims)
	if err != nil {
		return "", "", err
	}
	refresh, err = newOpaqueToken()
	if err != nil {
		return "", "", err
	}
	ttl := s.refreshTTL
	if device.Platform == PlatformWeb {
		ttl = min(ttl, webRefreshTTL)
	}
	var name *string
	if device.Name != "" {
		name = &device.Name
	}
	err = repo.AddRefreshToken(ctx, RefreshToken{
		UserID: user.ID, TenantID: tenantID, TokenHash: hashToken(refresh), FamilyID: family,
		DeviceID: device.ID, DeviceName: name, Platform: device.Platform, ExpiresAt: s.now().Add(ttl),
	}, validIP(ip))
	return access, refresh, err
}

func (s *Service) authenticated(ctx context.Context, user User, access, refresh string, usable []authz.MembershipView) SessionResponse {
	return SessionResponse{
		Status: StatusAuthenticated, AccessToken: access, RefreshToken: refresh,
		ExpiresIn: s.expiresIn(), Memberships: s.reads(ctx, usable), PlatformAdmin: user.IsPlatformAdmin,
	}
}

func (s *Service) expiresIn() int {
	return int(s.tokens.AccessTTL().Seconds())
}

func activeUser(ctx context.Context, q database.Querier, id uuid.UUID) (User, error) {
	user, err := NewRepository(q).GetByID(ctx, id)
	if err != nil {
		return User{}, err
	}
	if user == nil || !user.IsActive {
		return User{}, ErrInvalidToken
	}
	return *user, nil
}

func usableMemberships(ctx context.Context, q database.Querier, userID uuid.UUID) ([]authz.MembershipView, error) {
	all, err := authz.NewRepository(q).MembershipsOfUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	usable := make([]authz.MembershipView, 0, len(all))
	for _, m := range all {
		if m.Usable() {
			usable = append(usable, m)
		}
	}
	return usable, nil
}

func findMembership(list []authz.MembershipView, tenantID uuid.UUID) *authz.MembershipView {
	for i := range list {
		if list[i].TenantID == tenantID {
			return &list[i]
		}
	}
	return nil
}

// reads arma las empresas para el cliente, con la URL firmada de cada logo.
func (s *Service) reads(ctx context.Context, list []authz.MembershipView) []MembershipRead {
	out := make([]MembershipRead, 0, len(list))
	for _, m := range list {
		out = append(out, NewMembershipRead(m, s.storage.LogoURL(ctx, m.TenantID, m.TenantLogoKey)))
	}
	return out
}

func deviceOf(t RefreshToken) Device {
	d := Device{ID: t.DeviceID, Platform: t.Platform}
	if t.DeviceName != nil {
		d.Name = *t.DeviceName
	}
	return d
}

func subject(claims map[string]any) (uuid.UUID, error) {
	return uuid.Parse(stringClaim(claims, "sub"))
}

func stringClaim(claims map[string]any, name string) string {
	v, _ := claims[name].(string)
	return v
}

// newOpaqueToken genera un refresh token aleatorio de 256 bits.
func newOpaqueToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generando token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken es el SHA-256 en hexadecimal: lo unico que se guarda del token.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func validIP(raw string) *string {
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return nil
	}
	s := addr.String()
	return &s
}

func domainOf(email string) string {
	if at := strings.LastIndexByte(email, '@'); at >= 0 {
		return strings.ToLower(email[at+1:])
	}
	return ""
}
