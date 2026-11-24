package auth

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/jobs"
	"github.com/smorareqs/argos-service/internal/core/mail"
	"github.com/smorareqs/argos-service/internal/core/security"
	"github.com/smorareqs/argos-service/internal/modules/audit"
	"github.com/smorareqs/argos-service/internal/modules/authz"
	"github.com/smorareqs/argos-service/internal/modules/properties"
)

// invitationTTL es la vigencia del enlace de invitacion (fase01.md §3.3).
const invitationTTL = 7 * 24 * time.Hour

// InviteInput son los datos de una invitacion. Inviter nil = la invita la
// plataforma (alta de empresa).
type InviteInput struct {
	TenantID    uuid.UUID
	Inviter     *uuid.UUID
	Email       string
	RoleKey     authz.RoleKey
	PropertyIDs []uuid.UUID
	Message     string
}

// Invite crea la invitacion, envia el correo y lo registra en la bitacora.
func (s *Service) Invite(ctx context.Context, p Principal, tenantID uuid.UUID, in InvitationCreate) (InvitationRead, error) {
	msg := ""
	if in.Message != nil {
		msg = *in.Message
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (InvitationRead, error) {
		// Nadie invita con un rol que tenga permisos que no tiene.
		if ok, err := authz.CanGrantKey(ctx, q, p.Actor(), tenantID, in.RoleKey); err == nil && !ok {
			return InvitationRead{}, authz.ErrCannotGrant.WithField("body.role_key")
		}
		if err := properties.CheckProperties(ctx, q, in.PropertyIDs, "body.property_ids"); err != nil {
			return InvitationRead{}, err
		}
		if s.limits != nil {
			if err := s.limits.CheckLimit(ctx, q, tenantID, "users", "body.email"); err != nil {
				return InvitationRead{}, err
			}
		}
		inv, err := s.InviteTx(ctx, q, InviteInput{
			TenantID: tenantID, Inviter: &p.User.ID, Email: in.Email, RoleKey: in.RoleKey,
			PropertyIDs: in.PropertyIDs, Message: msg,
		})
		if err != nil {
			return InvitationRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &p.User.ID, Action: "invitation.created", Entity: "invitation", EntityID: &inv.ID,
			After: map[string]any{"email": inv.Email, "role_key": inv.RoleKey},
		}); err != nil {
			return InvitationRead{}, err
		}
		return newInvitationRead(inv), nil
	})
}

// InviteTx crea la invitacion dentro de la transaccion q (la usa tambien el
// alta de empresa de la plataforma). Encola el correo con el enlace.
func (s *Service) InviteTx(ctx context.Context, q database.Querier, in InviteInput) (Invitation, error) {
	email, err := normalizeEmail(strings.TrimSpace(in.Email))
	if err != nil {
		return Invitation{}, ErrInvalidEmail.WithField("body.email")
	}
	msg := strings.TrimSpace(in.Message)
	if utf8.RuneCountInString(msg) > 500 {
		return Invitation{}, ErrInvitationMessage.WithField("body.message")
	}
	role, err := authz.NewRepository(q).RoleByKey(ctx, in.TenantID, in.RoleKey)
	if err != nil {
		return Invitation{}, err
	}
	if role == nil || role.Key == authz.RoleGuest {
		return Invitation{}, ErrInvitationRole.WithField("body.role_key")
	}
	repo := NewRepository(q)
	userID, err := s.invitable(ctx, repo, in.TenantID, email)
	if err != nil {
		return Invitation{}, err
	}
	ids := make([]string, 0, len(in.PropertyIDs))
	for _, id := range in.PropertyIDs {
		ids = append(ids, id.String())
	}
	token, tokenID, err := s.newInvitationToken(ctx, repo, userID, map[string]any{"message": msg, "property_ids": ids})
	if err != nil {
		return Invitation{}, err
	}
	id, err := repo.AddInvitation(ctx, in.TenantID, email, role.ID, in.Inviter, tokenID)
	if err != nil {
		return Invitation{}, err
	}
	inv, err := repo.InvitationByID(ctx, in.TenantID, id)
	if err != nil {
		return Invitation{}, err
	}
	return *inv, s.enqueueInvitation(ctx, q, *inv, token)
}

// invitable comprueba que el correo no sea ya miembro ni tenga una invitacion
// vigente (una vencida se marca como tal). Devuelve el usuario si ya existe.
func (s *Service) invitable(ctx context.Context, repo *Repository, tenantID uuid.UUID, email string) (*uuid.UUID, error) {
	user, err := repo.GetByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	var userID *uuid.UUID
	if user != nil {
		member, err := repo.HasMembership(ctx, user.ID, tenantID)
		if err != nil {
			return nil, err
		}
		if member {
			return nil, ErrAlreadyMember.WithField("body.email")
		}
		userID = &user.ID
	}
	pending, err := repo.PendingInvitation(ctx, tenantID, email)
	if err != nil || pending == nil {
		return userID, err
	}
	if pending.ExpiresAt != nil && pending.ExpiresAt.After(s.now()) {
		return nil, ErrAlreadyInvited.WithField("body.email")
	}
	return userID, repo.SetInvitationStatus(ctx, pending.ID, InvitationExpired)
}

// Invitations lista las invitaciones de la empresa (status vacio = todas).
func (s *Service) Invitations(ctx context.Context, tenantID uuid.UUID, status string) ([]InvitationRead, error) {
	list, err := database.InTxValue(ctx, s.db, func(q database.Querier) ([]Invitation, error) {
		return NewRepository(q).Invitations(ctx, tenantID, status)
	})
	if err != nil {
		return nil, err
	}
	out := make([]InvitationRead, 0, len(list))
	for _, i := range list {
		out = append(out, newInvitationRead(i))
	}
	return out, nil
}

// ResendInvitation genera un enlace nuevo (el anterior deja de servir), extiende
// la vigencia y vuelve a enviar el correo.
func (s *Service) ResendInvitation(ctx context.Context, p Principal, tenantID, id uuid.UUID) (InvitationRead, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (InvitationRead, error) {
		repo := NewRepository(q)
		inv, err := repo.InvitationByID(ctx, tenantID, id)
		if err != nil {
			return InvitationRead{}, err
		}
		if inv == nil || inv.Status == InvitationRevoked {
			return InvitationRead{}, ErrInvitationNotFound
		}
		if inv.Status == InvitationAccepted {
			return InvitationRead{}, ErrInvitationAccepted
		}
		if inv.TokenID != nil {
			if err := repo.UseOneTimeToken(ctx, *inv.TokenID); err != nil {
				return InvitationRead{}, err
			}
		}
		token, tokenID, err := s.newInvitationToken(ctx, repo, nil, inv.Payload)
		if err != nil {
			return InvitationRead{}, err
		}
		if err := repo.ReplaceInvitationToken(ctx, inv.ID, tokenID); err != nil {
			return InvitationRead{}, err
		}
		fresh, err := repo.InvitationByID(ctx, tenantID, id)
		if err != nil {
			return InvitationRead{}, err
		}
		if err := s.enqueueInvitation(ctx, q, *fresh, token); err != nil {
			return InvitationRead{}, err
		}
		if err := audit.Record(ctx, q, audit.Entry{
			Actor: &p.User.ID, Action: "invitation.resent", Entity: "invitation", EntityID: &inv.ID,
			After: map[string]any{"email": inv.Email},
		}); err != nil {
			return InvitationRead{}, err
		}
		return newInvitationRead(*fresh), nil
	})
}

// RevokeInvitation cancela una invitacion pendiente: su enlace deja de servir.
func (s *Service) RevokeInvitation(ctx context.Context, p Principal, tenantID, id uuid.UUID) error {
	return s.db.InTx(ctx, func(q database.Querier) error {
		repo := NewRepository(q)
		inv, err := repo.InvitationByID(ctx, tenantID, id)
		if err != nil {
			return err
		}
		if inv == nil {
			return ErrInvitationNotFound
		}
		switch inv.Status {
		case InvitationAccepted:
			return ErrInvitationAccepted
		case InvitationRevoked:
			return nil
		}
		if err := repo.SetInvitationStatus(ctx, inv.ID, InvitationRevoked); err != nil {
			return err
		}
		if inv.TokenID != nil {
			if err := repo.UseOneTimeToken(ctx, *inv.TokenID); err != nil {
				return err
			}
		}
		return audit.Record(ctx, q, audit.Entry{
			Actor: &p.User.ID, Action: "invitation.revoked", Entity: "invitation", EntityID: &inv.ID,
			Before: map[string]any{"email": inv.Email, "role_key": inv.RoleKey},
		})
	})
}

// PreviewInvitation devuelve lo que ve quien abre el enlace.
func (s *Service) PreviewInvitation(ctx context.Context, token string) (InvitationPreview, error) {
	return database.InTxValue(ctx, s.db, func(q database.Querier) (InvitationPreview, error) {
		repo := NewRepository(q)
		_, inv, err := s.usableInvitation(ctx, repo, token)
		if err != nil {
			return InvitationPreview{}, err
		}
		user, err := repo.GetByEmail(ctx, inv.Email)
		if err != nil {
			return InvitationPreview{}, err
		}
		return InvitationPreview{
			TenantName: inv.TenantName, TenantLogoURL: s.storage.LogoURL(ctx, inv.TenantID, inv.TenantLogoKey),
			RoleName: inv.RoleName, Email: inv.Email, InviterName: inv.InviterName,
			UserExists: user != nil && user.HashedPassword != nil, ExpiresAt: inv.ExpiresAt.UTC(),
		}, nil
	})
}

// AcceptInvitation activa la membresia y abre la sesion en esa empresa. Si la
// persona ya tiene cuenta entra con su contrasena (y su codigo si usa 2FA); si
// no, la cuenta se crea con el nombre y la contrasena que escribe.
func (s *Service) AcceptInvitation(ctx context.Context, in AcceptInvitationRequest, ip string) (SessionResponse, error) {
	device, err := in.Device.normalized()
	if err != nil {
		return SessionResponse{}, err
	}
	return database.InTxValue(ctx, s.db, func(q database.Querier) (SessionResponse, error) {
		repo := NewRepository(q)
		token, inv, err := s.usableInvitation(ctx, repo, in.Token)
		if err != nil {
			return SessionResponse{}, err
		}
		user, err := s.invitedUser(ctx, repo, inv.Email, in)
		if err != nil {
			return SessionResponse{}, err
		}
		if err := s.join(ctx, q, *token, *inv, user); err != nil {
			return SessionResponse{}, err
		}
		if err := repo.TouchLogin(ctx, user.ID); err != nil {
			return SessionResponse{}, err
		}
		usable, err := usableMemberships(ctx, q, user.ID)
		if err != nil {
			return SessionResponse{}, err
		}
		chosen := findMembership(usable, inv.TenantID)
		if chosen == nil {
			return SessionResponse{}, ErrInvitationInvalid
		}
		return s.openSession(ctx, q, user, chosen, device, ip, usable)
	})
}

// join activa la membresia, cierra la invitacion y lo registra en la
// bitacora de la empresa (que vive en su schema).
func (s *Service) join(ctx context.Context, q database.Querier, token OneTimeToken, inv Invitation, user User) error {
	now := s.now()
	if _, err := authz.NewRepository(q).UpsertMembership(ctx, authz.Membership{
		UserID: user.ID, TenantID: inv.TenantID, RoleID: inv.RoleID, Status: authz.MembershipActive,
		InvitedBy: inv.InvitedBy, JoinedAt: &now,
	}); err != nil {
		return err
	}
	repo := NewRepository(q)
	if err := repo.SetInvitationStatus(ctx, inv.ID, InvitationAccepted); err != nil {
		return err
	}
	if err := repo.UseOneTimeToken(ctx, token.ID); err != nil {
		return err
	}
	if err := database.SetSearchPath(ctx, q, inv.TenantSchema, database.SharedSchema); err != nil {
		return err
	}
	if err := properties.ApplyInvitation(ctx, q, user.ID, inv.RoleKey == authz.RoleOwner,
		invitationProperties(token.Payload), inv.InvitedBy); err != nil {
		return err
	}
	if err := audit.Record(ctx, q, audit.Entry{
		Actor: &user.ID, Action: "invitation.accepted", Entity: "invitation", EntityID: &inv.ID,
		After: map[string]any{"email": inv.Email, "role_key": inv.RoleKey},
	}); err != nil {
		return err
	}
	return database.SetSearchPath(ctx, q, database.SharedSchema)
}

// invitationProperties lee las propiedades guardadas en el enlace.
func invitationProperties(payload map[string]any) []uuid.UUID {
	raw, _ := payload["property_ids"].([]any)
	ids := make([]uuid.UUID, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		if id, err := uuid.Parse(s); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// invitedUser valida (o crea) la cuenta de quien acepta.
func (s *Service) invitedUser(ctx context.Context, repo *Repository, email string, in AcceptInvitationRequest) (User, error) {
	user, err := repo.GetByEmail(ctx, email)
	if err != nil {
		return User{}, err
	}
	if user != nil && user.HashedPassword != nil {
		return s.existingInvitee(ctx, repo, *user, in)
	}
	name, err := validateFullName(in.FullName)
	if err != nil {
		return User{}, err
	}
	if err := validatePassword(in.Password); err != nil {
		return User{}, ErrInvalidPassword.WithField("body.password")
	}
	hashed, err := security.HashPassword(in.Password)
	if err != nil {
		return User{}, err
	}
	if user == nil {
		return repo.Add(ctx, User{Email: email, HashedPassword: &hashed, FullName: name, IsActive: true})
	}
	if err := repo.SetPassword(ctx, user.ID, hashed); err != nil {
		return User{}, err
	}
	if err := repo.UpdateProfile(ctx, user.ID, name, user.Phone); err != nil {
		return User{}, err
	}
	user.FullName, user.HashedPassword = name, &hashed
	return *user, nil
}

func (s *Service) usableInvitation(ctx context.Context, repo *Repository, raw string) (*OneTimeToken, *Invitation, error) {
	token, err := repo.OneTimeTokenByHash(ctx, hashToken(strings.TrimSpace(raw)), PurposeInvitation)
	if err != nil {
		return nil, nil, err
	}
	if token == nil || !token.Usable(s.now()) {
		return nil, nil, ErrInvitationInvalid
	}
	inv, err := repo.InvitationByToken(ctx, token.ID)
	if err != nil {
		return nil, nil, err
	}
	if inv == nil || inv.Status != InvitationPending || !inv.TenantActive || inv.ExpiresAt == nil {
		return nil, nil, ErrInvitationInvalid
	}
	return token, inv, nil
}

func (s *Service) newInvitationToken(ctx context.Context, repo *Repository, userID *uuid.UUID, payload map[string]any) (string, uuid.UUID, error) {
	token, err := newOpaqueToken()
	if err != nil {
		return "", uuid.Nil, err
	}
	id, err := repo.AddOneTimeToken(ctx, OneTimeToken{
		UserID: userID, Purpose: PurposeInvitation, TokenHash: hashToken(token), Payload: payload,
		ExpiresAt: s.now().Add(invitationTTL),
	})
	return token, id, err
}

func (s *Service) enqueueInvitation(ctx context.Context, q database.Querier, inv Invitation, token string) error {
	inviter := "El equipo de ARGOS"
	if inv.InviterName != nil {
		inviter = *inv.InviterName
	}
	expires := s.now().Add(invitationTTL)
	if inv.ExpiresAt != nil {
		expires = *inv.ExpiresAt
	}
	msg, _ := inv.Payload["message"].(string)
	return jobs.EnqueueEmail(ctx, q, &inv.TenantID, jobs.Email{To: inv.Email, Template: mail.TemplateInvitation, Data: map[string]any{
		"InviterName": inviter, "TenantName": inv.TenantName, "RoleName": inv.RoleName,
		"Link": s.link("/invitacion", token), "ExpiresOn": dateES(expires, inv.TenantTimezone), "Message": msg,
	}})
}

// existingInvitee valida a quien ya tiene cuenta: contrasena y, si usa 2FA,
// su codigo.
func (s *Service) existingInvitee(ctx context.Context, repo *Repository, user User, in AcceptInvitationRequest) (User, error) {
	if !user.IsActive {
		return User{}, ErrInactiveUser
	}
	if !security.VerifyPassword(in.Password, *user.HashedPassword) {
		return User{}, ErrWrongPassword.WithField("body.password")
	}
	if !user.TOTPEnabled {
		return user, nil
	}
	if strings.TrimSpace(in.TOTPCode) == "" {
		return User{}, ErrTOTPRequired
	}
	ok, err := s.verifyTOTPWith(ctx, repo, user.ID, in.TOTPCode)
	if err != nil {
		return User{}, err
	}
	if !ok {
		return User{}, ErrTOTPCodeIncorrect.WithField("body.totp_code")
	}
	return user, nil
}
