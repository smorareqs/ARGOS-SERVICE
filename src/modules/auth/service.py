"""Logica de negocio del modulo de autenticacion.

Maneja registro, login y emision/validacion de tokens. Es agnostico a HTTP:
lanza excepciones puras que la capa API traduce a respuestas.
"""

import uuid

from src.core.logging import get_logger
from src.core.security import (
    JWTError,
    create_access_token,
    create_refresh_token,
    decode_token,
    hash_password,
    verify_password,
)
from src.modules.auth.exceptions import (
    EmailAlreadyExistsError,
    InactiveUserError,
    InvalidCredentialsError,
    InvalidTokenError,
    UserNotFoundError,
)
from src.modules.auth.models import User
from src.modules.auth.repository import UserRepository
from src.modules.auth.schemas import Token, TokenPayload, UserCreate

logger = get_logger(__name__)


class AuthService:
    def __init__(self, repository: UserRepository) -> None:
        self._repo = repository

    async def register(self, data: UserCreate) -> User:
        """Registra un nuevo usuario validando la unicidad del correo."""
        if await self._repo.get_by_email(data.email) is not None:
            raise EmailAlreadyExistsError

        user = User(
            email=data.email,
            hashed_password=hash_password(data.password),
            full_name=data.full_name,
            role=data.role,
            is_owner=data.is_owner,
            tenant_id=data.tenant_id,
        )
        user = await self._repo.add(user)
        logger.info("Usuario registrado: email=%s role=%s", data.email, data.role)
        return user

    async def authenticate(self, email: str, password: str) -> User:
        """Valida credenciales y devuelve el usuario o lanza excepcion."""
        user = await self._repo.get_by_email(email)
        if user is None or not verify_password(password, user.hashed_password):
            raise InvalidCredentialsError
        if not user.is_active:
            raise InactiveUserError
        return user

    def issue_tokens(self, user: User) -> Token:
        """Emite el par access/refresh con los claims de tenant y rol."""
        claims: dict[str, str] = {"role": user.role}
        if user.tenant_id is not None:
            claims["tenant_id"] = str(user.tenant_id)

        return Token(
            access_token=create_access_token(str(user.id), claims),
            refresh_token=create_refresh_token(str(user.id), claims),
        )

    async def login(self, email: str, password: str) -> Token:
        user = await self.authenticate(email, password)
        return self.issue_tokens(user)

    async def get_current_user(self, user_id: uuid.UUID) -> User:
        user = await self._repo.get_by_id(user_id)
        if user is None:
            raise UserNotFoundError
        if not user.is_active:
            raise InactiveUserError
        return user

    @staticmethod
    def parse_token(token: str) -> TokenPayload:
        """Decodifica y valida un JWT, devolviendo sus claims tipados."""
        try:
            raw = decode_token(token)
            return TokenPayload.model_validate(raw)
        except (JWTError, ValueError) as exc:
            raise InvalidTokenError from exc
