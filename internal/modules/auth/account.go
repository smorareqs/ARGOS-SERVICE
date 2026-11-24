package auth

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/core/mail"
	"github.com/smorareqs/argos-service/internal/core/security"
)

// Vigencias y limites de los enlaces por correo (fase01.md §3.3).
const (
	resetTTL      = time.Hour
	resetCooldown = 2 * time.Minute
	totpIssuer    = "ARGOS"
	defaultZone   = "America/Guatemala"
)

// --- Contrasenas -----------------------------------------------------------

// ForgotPassword envia el enlace para restablecer la contrasena. Responde
// igual exista o no el correo (no revela cuentas) y no reenvia si ya mando uno
// hace menos de dos minutos.
func (s *Service) ForgotPassword(ctx context.Context, email string) {
	err := s.db.InTx(ctx, func(q database.Querier) error {
		repo := NewRepository(q)
		user, err := repo.GetByEmail(ctx, strings.TrimSpace(email))
		if err != nil || user == nil || !user.IsActive {
			return err
		}
		recent, err := repo.TokenIssuedSince(ctx, user.ID, PurposePasswordReset, s.now().Add(-resetCooldown))
		if err != nil || recent {
			return err
		}
		if err := repo.ExpireUserTokens(ctx, user.ID, PurposePasswordReset); err != nil {
			return err
		}
		token, err := newOpaqueToken()
		if err != nil {
			return err
		}
		if _, err := repo.AddOneTimeToken(ctx, OneTimeToken{
			UserID: &user.ID, Purpose: PurposePasswordReset, TokenHash: hashToken(token), ExpiresAt: s.now().Add(resetTTL),
		}); err != nil {
			return err
		}
		return jobs.EnqueueEmail(ctx, q, nil, jobs.Email{To: user.Email, Template: mail.TemplatePasswordReset, Data: map[string]any{
			"Name": firstName(user.FullName), "Link": s.link("/restablecer", token), "Minutes": int(resetTTL.Minutes()),
		}})
	})
	if err != nil {
		s.log.ErrorContext(ctx, "No se pudo preparar la recuperacion de contrasena", "email_domain", domainOf(email), "error", err)
	}
}

// ResetPassword cambia la contrasena con el enlace del correo y cierra todas
// las sesiones de la persona.
func (s *Service) ResetPassword(ctx context.Context, in ResetPasswordRequest) error {
	if err := validatePassword(in.NewPassword); err != nil {
		return ErrInvalidPassword.WithField("body.new_password")
	}
	hashed, err := security.HashPassword(in.NewPassword)
	if err != nil {
		return err
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		repo := NewRepository(q)
		token, user, err := s.resetTarget(ctx, repo, in.Token)
		if err != nil {
			return err
		}
		if err := repo.SetPassword(ctx, user.ID, hashed); err != nil {
			return err
		}
		if err := repo.UseOneTimeToken(ctx, token.ID); err != nil {
			return err
		}
		if err := repo.RevokeUserSessions(ctx, user.ID, nil); err != nil {
			return err
		}
		return s.enqueuePasswordChanged(ctx, q, *user)
	})
}

// resetTarget valida el enlace de restablecimiento y devuelve su usuario.
func (s *Service) resetTarget(ctx context.Context, repo *Repository, raw string) (*OneTimeToken, *User, error) {
	token, err := repo.OneTimeTokenByHash(ctx, hashToken(strings.TrimSpace(raw)), PurposePasswordReset)
	if err != nil {
		return nil, nil, err
	}
	if token == nil || !token.Usable(s.now()) || token.UserID == nil {
		return nil, nil, ErrResetLinkInvalid
	}
	user, err := repo.GetByID(ctx, *token.UserID)
	if err != nil {
		return nil, nil, err
	}
	if user == nil || !user.IsActive {
		return nil, nil, ErrResetLinkInvalid
	}
	return token, user, nil
}

// ChangePassword cambia la contrasena desde el perfil y cierra las demas
// sesiones (la actual sigue abierta).
func (s *Service) ChangePassword(ctx context.Context, p Principal, in ChangePasswordRequest) error {
	if err := validatePassword(in.NewPassword); err != nil {
		return ErrInvalidPassword.WithField("body.new_password")
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		repo := NewRepository(q)
		user, err := repo.GetByID(ctx, p.User.ID)
		if err != nil {
			return err
		}
		if user == nil || user.HashedPassword == nil || !security.VerifyPassword(in.CurrentPassword, *user.HashedPassword) {
			return ErrWrongPassword.WithField("body.current_password")
		}
		hashed, err := security.HashPassword(in.NewPassword)
		if err != nil {
			return err
		}
		if err := repo.SetPassword(ctx, user.ID, hashed); err != nil {
			return err
		}
		if err := repo.RevokeUserSessions(ctx, user.ID, &p.SessionID); err != nil {
			return err
		}
		return s.enqueuePasswordChanged(ctx, q, *user)
	})
}

func (s *Service) enqueuePasswordChanged(ctx context.Context, q database.Querier, user User) error {
	return jobs.EnqueueEmail(ctx, q, nil, jobs.Email{To: user.Email, Template: mail.TemplatePasswordChanged, Data: map[string]any{
		"Name": firstName(user.FullName), "When": dateTimeES(s.now(), defaultZone),
		"Link": s.webBase + "/login?recuperar=1",
	}})
}

// --- Verificacion en dos pasos ----------------------------------------------

// SetupTOTP genera un secreto nuevo (aun inactivo) y devuelve el QR para la
// app autenticadora. Repetirlo reemplaza el secreto pendiente.
func (s *Service) SetupTOTP(ctx context.Context, p Principal) (TOTPSetupResponse, error) {
	if p.User.TOTPEnabled {
		return TOTPSetupResponse{}, ErrTOTPAlreadyEnabled
	}
	secret, err := security.NewTOTPSecret()
	if err != nil {
		return TOTPSetupResponse{}, err
	}
	sealed, err := s.cipher.Encrypt(secret)
	if err != nil {
		return TOTPSetupResponse{}, err
	}
	if err := s.db.InTx(ctx, func(q database.Querier) error {
		return NewRepository(q).SetTOTP(ctx, p.User.ID, &sealed, false)
	}); err != nil {
		return TOTPSetupResponse{}, err
	}
	return TOTPSetupResponse{Secret: secret, OTPAuthURL: security.TOTPURI(totpIssuer, p.User.Email, secret)}, nil
}

// ConfirmTOTP activa la verificacion con el primer codigo correcto.
func (s *Service) ConfirmTOTP(ctx context.Context, p Principal, code string) error {
	if p.User.TOTPEnabled {
		return ErrTOTPAlreadyEnabled
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		repo := NewRepository(q)
		sealed, err := repo.TOTPSecret(ctx, p.User.ID)
		if err != nil {
			return err
		}
		if sealed == nil {
			return ErrTOTPNotSetup
		}
		secret, err := s.cipher.Decrypt(*sealed)
		if err != nil {
			return err
		}
		if !security.VerifyTOTP(secret, code, s.now()) {
			return ErrTOTPCodeIncorrect.WithField("body.code")
		}
		return repo.SetTOTP(ctx, p.User.ID, sealed, true)
	})
}

// DisableTOTP desactiva la verificacion: pide contrasena y un codigo vigente.
func (s *Service) DisableTOTP(ctx context.Context, p Principal, in TOTPDisableRequest) error {
	if !p.User.TOTPEnabled {
		return ErrTOTPNotEnabled
	}
	return s.db.InTx(ctx, func(q database.Querier) error {
		repo := NewRepository(q)
		user, err := repo.GetByID(ctx, p.User.ID)
		if err != nil {
			return err
		}
		if user == nil || user.HashedPassword == nil || !security.VerifyPassword(in.Password, *user.HashedPassword) {
			return ErrWrongPassword.WithField("body.password")
		}
		ok, err := s.verifyTOTPWith(ctx, repo, user.ID, in.Code)
		if err != nil {
			return err
		}
		if !ok {
			return ErrTOTPCodeIncorrect.WithField("body.code")
		}
		return repo.SetTOTP(ctx, user.ID, nil, false)
	})
}

func (s *Service) verifyTOTP(ctx context.Context, userID uuid.UUID, code string) (bool, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (bool, error) {
		return s.verifyTOTPWith(ctx, NewRepository(q), userID, code)
	})
}

func (s *Service) verifyTOTPWith(ctx context.Context, repo *Repository, userID uuid.UUID, code string) (bool, error) {
	sealed, err := repo.TOTPSecret(ctx, userID)
	if err != nil || sealed == nil {
		return false, err
	}
	secret, err := s.cipher.Decrypt(*sealed)
	if err != nil {
		return false, fmt.Errorf("secreto TOTP ilegible: %w", err)
	}
	return security.VerifyTOTP(secret, code, s.now()), nil
}

// --- Sesiones --------------------------------------------------------------

// Sessions lista los dispositivos con sesion abierta; marca la actual.
func (s *Service) Sessions(ctx context.Context, p Principal) ([]SessionRead, error) {
	rows, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]SessionRow, error) {
		return NewRepository(q).ActiveSessions(ctx, p.User.ID)
	})
	if err != nil {
		return nil, err
	}
	out := make([]SessionRead, 0, len(rows))
	for _, r := range rows {
		out = append(out, SessionRead{
			ID: r.ID, DeviceName: r.DeviceName, Platform: r.Platform, IP: r.IP, TenantName: r.TenantName,
			LastUsedAt: r.LastUsedAt, CreatedAt: r.CreatedAt.UTC(), Current: r.ID == p.SessionID,
		})
	}
	return out, nil
}

// RevokeSession cierra una sesion propia (404 si no es de la persona).
func (s *Service) RevokeSession(ctx context.Context, p Principal, id uuid.UUID) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		repo := NewRepository(q)
		ok, err := repo.SessionBelongsTo(ctx, p.User.ID, id)
		if err != nil {
			return err
		}
		if !ok {
			return ErrSessionNotFound
		}
		return repo.RevokeFamily(ctx, id)
	})
}

// RevokeOtherSessions cierra todas las sesiones salvo la actual.
func (s *Service) RevokeOtherSessions(ctx context.Context, p Principal) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		return NewRepository(q).RevokeUserSessions(ctx, p.User.ID, &p.SessionID)
	})
}

// --- Perfil ----------------------------------------------------------------

// UpdateProfile cambia nombre y telefono de la persona.
func (s *Service) UpdateProfile(ctx context.Context, p Principal, in ProfileUpdate) (MeUser, error) {
	name, phone := p.User.FullName, p.User.Phone
	if in.FullName != nil {
		n, err := validateFullName(*in.FullName)
		if err != nil {
			return MeUser{}, err
		}
		name = n
	}
	if in.Phone != nil {
		v := strings.TrimSpace(*in.Phone)
		if utf8.RuneCountInString(v) > 32 {
			return MeUser{}, ErrInvalidPhone.WithField("body.phone")
		}
		phone = nil
		if v != "" {
			phone = &v
		}
	}
	if err := s.db.InTx(ctx, func(q database.Querier) error {
		return NewRepository(q).UpdateProfile(ctx, p.User.ID, name, phone)
	}); err != nil {
		return MeUser{}, err
	}
	return MeUser{
		ID: p.User.ID, Email: p.User.Email, FullName: name, Phone: phone,
		TOTPEnabled: p.User.TOTPEnabled, IsPlatformAdmin: p.User.IsPlatformAdmin,
	}, nil
}

// SetPlatformAdmin marca a la persona como administradora de ARGOS (seed).
func (s *Service) SetPlatformAdmin(ctx context.Context, userID uuid.UUID, admin bool) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		return NewRepository(q).SetPlatformAdmin(ctx, userID, admin)
	})
}

// --- Utilidades ------------------------------------------------------------

func (s *Service) link(path, token string) string {
	return s.webBase + path + "?token=" + token
}

func firstName(full string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(full), " ")
	if name == "" {
		return "hola"
	}
	return name
}

var monthsES = [...]string{
	"enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto",
	"septiembre", "octubre", "noviembre", "diciembre",
}

func inZone(t time.Time, zone string) time.Time {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc, _ = time.LoadLocation(defaultZone)
	}
	if loc == nil {
		return t.UTC()
	}
	return t.In(loc)
}

// dateES es "28 de noviembre de 2026" en la zona indicada.
func dateES(t time.Time, zone string) string {
	t = inZone(t, zone)
	return fmt.Sprintf("%d de %s de %d", t.Day(), monthsES[t.Month()-1], t.Year())
}

// dateTimeES es "21 de noviembre de 2026 a las 14:05".
func dateTimeES(t time.Time, zone string) string {
	local := inZone(t, zone)
	return fmt.Sprintf("%s a las %02d:%02d", dateES(t, zone), local.Hour(), local.Minute())
}
