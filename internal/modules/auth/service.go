package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/smorareqs/argos-service/internal/core/database"
	"github.com/smorareqs/argos-service/internal/core/security"
)

// Service contiene la logica de negocio de autenticacion: registro, login y
// emision/validacion de tokens. Es agnostico a HTTP: devuelve errores de
// dominio que la capa API traduce a respuestas.
type Service struct {
	db     *database.DB
	tokens *security.TokenManager
	log    *slog.Logger
}

// NewService crea el servicio.
func NewService(db *database.DB, tokens *security.TokenManager, log *slog.Logger) *Service {
	return &Service{db: db, tokens: tokens, log: log}
}

// Register registra un usuario validando sus datos y la unicidad del correo.
func (s *Service) Register(ctx context.Context, in UserCreate) (User, error) {
	in, err := in.normalized()
	if err != nil {
		return User{}, err
	}
	hashed, err := security.HashPassword(in.Password)
	if err != nil {
		return User{}, err
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
			Email:          in.Email,
			HashedPassword: hashed,
			FullName:       in.FullName,
			Role:           in.Role,
			IsActive:       true,
			IsOwner:        in.IsOwner,
			TenantID:       in.TenantID,
		})
	})
	if err != nil {
		return User{}, err
	}
	s.log.InfoContext(ctx, "Usuario registrado", "email", user.Email, "role", user.Role)
	return user, nil
}

// Authenticate valida credenciales y devuelve el usuario.
func (s *Service) Authenticate(ctx context.Context, email, password string) (User, error) {
	user, err := database.InTxValue(ctx, s.db, func(q database.Querier) (*User, error) {
		return NewRepository(q).GetByEmail(ctx, email)
	})
	if err != nil {
		return User{}, err
	}
	if user == nil || !security.VerifyPassword(password, user.HashedPassword) {
		return User{}, ErrInvalidCredentials
	}
	if !user.IsActive {
		return User{}, ErrInactiveUser
	}
	return *user, nil
}

// IssueTokens emite el par access/refresh con los claims de tenant y rol.
func (s *Service) IssueTokens(user User) (Token, error) {
	claims := map[string]any{"role": string(user.Role)}
	if user.TenantID != nil {
		claims["tenant_id"] = user.TenantID.String()
	}
	access, err := s.tokens.CreateAccessToken(user.ID.String(), claims)
	if err != nil {
		return Token{}, err
	}
	refresh, err := s.tokens.CreateRefreshToken(user.ID.String(), claims)
	if err != nil {
		return Token{}, err
	}
	return Token{AccessToken: access, RefreshToken: refresh, TokenType: "bearer"}, nil
}

// Login autentica y emite los tokens.
func (s *Service) Login(ctx context.Context, email, password string) (Token, error) {
	user, err := s.Authenticate(ctx, email, password)
	if err != nil {
		return Token{}, err
	}
	return s.IssueTokens(user)
}

// GetCurrentUser carga un usuario activo por id.
func (s *Service) GetCurrentUser(ctx context.Context, id uuid.UUID) (User, error) {
	user, err := database.InTxValue(ctx, s.db, func(q database.Querier) (*User, error) {
		return NewRepository(q).GetByID(ctx, id)
	})
	if err != nil {
		return User{}, err
	}
	if user == nil {
		return User{}, ErrUserNotFound
	}
	if !user.IsActive {
		return User{}, ErrInactiveUser
	}
	return *user, nil
}

// CurrentUserFromToken resuelve el usuario de un token de acceso. Rechaza los
// tokens de refresco y los de usuarios que ya no existen.
func (s *Service) CurrentUserFromToken(ctx context.Context, token string) (User, error) {
	payload, err := s.ParseToken(token)
	if err != nil {
		return User{}, err
	}
	if payload.Type != security.TokenAccess {
		return User{}, ErrInvalidToken
	}
	user, err := s.GetCurrentUser(ctx, payload.Sub)
	if errors.Is(err, ErrUserNotFound) {
		return User{}, ErrInvalidToken
	}
	return user, err
}

// ParseToken decodifica y valida un JWT, devolviendo sus claims tipados.
func (s *Service) ParseToken(token string) (TokenPayload, error) {
	claims, err := s.tokens.Decode(token)
	if err != nil {
		return TokenPayload{}, ErrInvalidToken
	}
	payload, err := payloadFromClaims(claims)
	if err != nil {
		return TokenPayload{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	return payload, nil
}

func payloadFromClaims(claims map[string]any) (TokenPayload, error) {
	sub, _ := claims["sub"].(string)
	id, err := uuid.Parse(sub)
	if err != nil {
		return TokenPayload{}, errors.New("claim sub invalido")
	}
	role, _ := claims["role"].(string)
	if !Role(role).Valid() {
		return TokenPayload{}, errors.New("claim role invalido")
	}
	tokenType, _ := claims["type"].(string)
	if tokenType == "" {
		return TokenPayload{}, errors.New("falta el claim type")
	}
	payload := TokenPayload{Sub: id, Role: Role(role), Type: security.TokenType(tokenType)}

	if raw, ok := claims["tenant_id"]; ok && raw != nil {
		tenant, _ := raw.(string)
		tenantID, err := uuid.Parse(tenant)
		if err != nil {
			return TokenPayload{}, errors.New("claim tenant_id invalido")
		}
		payload.TenantID = &tenantID
	}
	return payload, nil
}
