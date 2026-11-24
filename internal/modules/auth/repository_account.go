package auth

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const oneTimeColumns = `id, user_id, purpose, token_hash, payload, expires_at, used_at, created_at, updated_at`

// --- Usuarios ---------------------------------------------------------------

// TOTPSecret devuelve el secreto TOTP cifrado del usuario (nil si no tiene).
func (r *Repository) TOTPSecret(ctx context.Context, id uuid.UUID) (*string, error) {
	var secret *string
	err := r.q.QueryRow(ctx, "SELECT totp_secret FROM shared.users WHERE id = $1", id).Scan(&secret)
	return secret, err
}

// SetTOTP guarda el secreto cifrado y si la verificacion esta activa.
func (r *Repository) SetTOTP(ctx context.Context, id uuid.UUID, secret *string, enabled bool) error {
	_, err := r.q.Exec(ctx, `
		UPDATE shared.users SET totp_secret = $2, totp_enabled = $3, updated_at = now() WHERE id = $1`,
		id, secret, enabled)
	return err
}

// UpdateProfile cambia nombre y telefono.
func (r *Repository) UpdateProfile(ctx context.Context, id uuid.UUID, fullName string, phone *string) error {
	_, err := r.q.Exec(ctx, `
		UPDATE shared.users SET full_name = $2, phone = $3, updated_at = now() WHERE id = $1`, id, fullName, phone)
	return err
}

// SetPlatformAdmin marca o desmarca al usuario como administrador de ARGOS.
func (r *Repository) SetPlatformAdmin(ctx context.Context, id uuid.UUID, admin bool) error {
	_, err := r.q.Exec(ctx, `
		UPDATE shared.users SET is_platform_admin = $2, updated_at = now() WHERE id = $1`, id, admin)
	return err
}

// --- Tokens de un solo uso -------------------------------------------------

// AddOneTimeToken guarda un token (solo su hash) y devuelve su id.
func (r *Repository) AddOneTimeToken(ctx context.Context, t OneTimeToken) (uuid.UUID, error) {
	payload := t.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	var id uuid.UUID
	err := r.q.QueryRow(ctx, `
		INSERT INTO shared.one_time_tokens (user_id, purpose, token_hash, payload, expires_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		t.UserID, t.Purpose, t.TokenHash, payload, t.ExpiresAt).Scan(&id)
	return id, err
}

// OneTimeTokenByHash devuelve el token (bloqueado para la transaccion) o nil.
func (r *Repository) OneTimeTokenByHash(ctx context.Context, hash, purpose string) (*OneTimeToken, error) {
	return oneOrNil[OneTimeToken](ctx, r.q, "SELECT "+oneTimeColumns+`
		FROM shared.one_time_tokens WHERE token_hash = $1 AND purpose = $2 FOR UPDATE`, hash, purpose)
}

// UseOneTimeToken marca el token como usado.
func (r *Repository) UseOneTimeToken(ctx context.Context, id uuid.UUID) error {
	_, err := r.q.Exec(ctx, `
		UPDATE shared.one_time_tokens SET used_at = now(), updated_at = now() WHERE id = $1 AND used_at IS NULL`, id)
	return err
}

// ExpireUserTokens invalida los tokens sin usar del usuario con ese proposito.
func (r *Repository) ExpireUserTokens(ctx context.Context, userID uuid.UUID, purpose string) error {
	_, err := r.q.Exec(ctx, `
		UPDATE shared.one_time_tokens SET used_at = now(), updated_at = now()
		WHERE user_id = $1 AND purpose = $2 AND used_at IS NULL`, userID, purpose)
	return err
}

// TokenIssuedSince indica si el usuario recibio un token con ese proposito
// despues de since (evita reenviar correos en rafaga).
func (r *Repository) TokenIssuedSince(ctx context.Context, userID uuid.UUID, purpose string, since time.Time) (bool, error) {
	var exists bool
	err := r.q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM shared.one_time_tokens
		               WHERE user_id = $1 AND purpose = $2 AND created_at > $3)`, userID, purpose, since).Scan(&exists)
	return exists, err
}

// --- Sesiones --------------------------------------------------------------

// ActiveSessions devuelve las sesiones vigentes del usuario, la mas reciente
// primero. Una sesion es una familia de refresh tokens con alguno vigente.
func (r *Repository) ActiveSessions(ctx context.Context, userID uuid.UUID) ([]SessionRow, error) {
	rows, err := r.q.Query(ctx, `
		SELECT rt.family_id AS id,
		       (array_agg(rt.device_name ORDER BY rt.created_at DESC))[1] AS device_name,
		       (array_agg(rt.platform ORDER BY rt.created_at DESC))[1] AS platform,
		       (array_agg(host(rt.ip) ORDER BY rt.created_at DESC))[1] AS ip,
		       (array_agg(t.name ORDER BY rt.created_at DESC))[1] AS tenant_name,
		       max(rt.last_used_at) AS last_used_at,
		       min(rt.created_at) AS created_at
		FROM shared.refresh_tokens rt
		LEFT JOIN shared.tenants t ON t.id = rt.tenant_id
		WHERE rt.user_id = $1
		GROUP BY rt.family_id
		HAVING bool_or(rt.revoked_at IS NULL AND rt.expires_at > now())
		ORDER BY max(rt.last_used_at) DESC NULLS LAST`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[SessionRow])
}

// SessionBelongsTo indica si la familia es una sesion vigente del usuario.
func (r *Repository) SessionBelongsTo(ctx context.Context, userID, familyID uuid.UUID) (bool, error) {
	var exists bool
	err := r.q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM shared.refresh_tokens
		               WHERE user_id = $1 AND family_id = $2 AND revoked_at IS NULL AND expires_at > now())`,
		userID, familyID).Scan(&exists)
	return exists, err
}

// RevokeUserSessions revoca todas las sesiones del usuario salvo except (si
// no es nil).
func (r *Repository) RevokeUserSessions(ctx context.Context, userID uuid.UUID, except *uuid.UUID) error {
	_, err := r.q.Exec(ctx, `
		UPDATE shared.refresh_tokens SET revoked_at = now(), updated_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL AND ($2::uuid IS NULL OR family_id <> $2)`, userID, except)
	return err
}

// --- Invitaciones ----------------------------------------------------------

const selectInvitation = `SELECT i.id, i.tenant_id, i.email, i.role_id, r.key AS role_key, r.name AS role_name,
       i.invited_by, u.full_name AS inviter_name, i.token_id, i.status, i.accepted_at, i.created_at, i.updated_at,
       ot.expires_at, COALESCE(ot.payload, '{}'::jsonb) AS payload,
       t.name AS tenant_name, t.schema_name AS tenant_schema, t.is_active AS tenant_active,
       t.logo_key AS tenant_logo_key, t.timezone AS tenant_timezone
FROM shared.invitations i
JOIN shared.roles r ON r.id = i.role_id
JOIN shared.tenants t ON t.id = i.tenant_id
LEFT JOIN shared.users u ON u.id = i.invited_by
LEFT JOIN shared.one_time_tokens ot ON ot.id = i.token_id`

// AddInvitation inserta la invitacion pendiente y devuelve su id.
func (r *Repository) AddInvitation(ctx context.Context, tenantID uuid.UUID, email string, roleID uuid.UUID, invitedBy *uuid.UUID, tokenID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.q.QueryRow(ctx, `
		INSERT INTO shared.invitations (tenant_id, email, role_id, invited_by, token_id, status)
		VALUES ($1, $2, $3, $4, $5, 'pending') RETURNING id`,
		tenantID, email, roleID, invitedBy, tokenID).Scan(&id)
	return id, err
}

// InvitationByID devuelve la invitacion del tenant (bloqueada) o nil.
func (r *Repository) InvitationByID(ctx context.Context, tenantID, id uuid.UUID) (*Invitation, error) {
	return oneOrNil[Invitation](ctx, r.q, selectInvitation+` WHERE i.tenant_id = $1 AND i.id = $2 FOR UPDATE OF i`, tenantID, id)
}

// InvitationByToken devuelve la invitacion asociada al token o nil.
func (r *Repository) InvitationByToken(ctx context.Context, tokenID uuid.UUID) (*Invitation, error) {
	return oneOrNil[Invitation](ctx, r.q, selectInvitation+` WHERE i.token_id = $1 FOR UPDATE OF i`, tokenID)
}

// PendingInvitation devuelve la invitacion pendiente del correo en el tenant
// o nil.
func (r *Repository) PendingInvitation(ctx context.Context, tenantID uuid.UUID, email string) (*Invitation, error) {
	return oneOrNil[Invitation](ctx, r.q, selectInvitation+`
		WHERE i.tenant_id = $1 AND lower(i.email) = lower($2) AND i.status = 'pending' FOR UPDATE OF i`, tenantID, email)
}

// Invitations devuelve las invitaciones del tenant con ese estado (vacio =
// todas), lo mas reciente primero. "pending" excluye las vencidas.
func (r *Repository) Invitations(ctx context.Context, tenantID uuid.UUID, status string) ([]Invitation, error) {
	rows, err := r.q.Query(ctx, selectInvitation+`
		WHERE i.tenant_id = $1
		  AND ($2 = '' OR i.status = $2)
		  AND ($2 <> 'pending' OR ot.expires_at > now())
		ORDER BY i.created_at DESC`, tenantID, status)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Invitation])
}

// SetInvitationStatus cambia el estado (y la fecha de aceptacion).
func (r *Repository) SetInvitationStatus(ctx context.Context, id uuid.UUID, status string) error {
	_, err := r.q.Exec(ctx, `
		UPDATE shared.invitations
		SET status = $2::text, accepted_at = CASE WHEN $2::text = 'accepted' THEN now() ELSE accepted_at END, updated_at = now()
		WHERE id = $1`, id, status)
	return err
}

// ReplaceInvitationToken asocia un token nuevo (reenvio) y la deja pendiente.
func (r *Repository) ReplaceInvitationToken(ctx context.Context, id, tokenID uuid.UUID) error {
	_, err := r.q.Exec(ctx, `
		UPDATE shared.invitations SET token_id = $2, status = 'pending', updated_at = now() WHERE id = $1`, id, tokenID)
	return err
}

// HasMembership indica si el usuario ya pertenece al tenant (en cualquier
// estado).
func (r *Repository) HasMembership(ctx context.Context, userID, tenantID uuid.UUID) (bool, error) {
	var exists bool
	err := r.q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM shared.memberships WHERE user_id = $1 AND tenant_id = $2)`,
		userID, tenantID).Scan(&exists)
	return exists, err
}
