"""Logica criptografica: hashing de contrasenas y JWT.

No conoce HTTP ni la base de datos; expone funciones puras reutilizables por la
capa de servicios de autenticacion.
"""

from datetime import UTC, datetime, timedelta
from enum import StrEnum
from typing import Any, cast
import uuid

from jose import JWTError, jwt
from passlib.context import CryptContext

from src.core.config import settings

_pwd_context = CryptContext(schemes=["bcrypt"], deprecated="auto")


class TokenType(StrEnum):
    ACCESS = "access"
    REFRESH = "refresh"


def hash_password(plain_password: str) -> str:
    """Genera el hash bcrypt de una contrasena en texto plano."""
    # passlib carece de stubs; el cast explicito evita el `Any` en modo estricto.
    return str(_pwd_context.hash(plain_password))


def verify_password(plain_password: str, hashed_password: str) -> bool:
    """Compara una contrasena en texto plano contra su hash."""
    return bool(_pwd_context.verify(plain_password, hashed_password))


def _create_token(
    subject: str,
    token_type: TokenType,
    expires_delta: timedelta,
    extra_claims: dict[str, Any] | None = None,
) -> str:
    now = datetime.now(UTC)
    payload: dict[str, Any] = {
        "sub": subject,
        "type": token_type.value,
        "iat": now,
        "exp": now + expires_delta,
        "jti": str(uuid.uuid4()),
    }
    if extra_claims:
        payload.update(extra_claims)
    # python-jose carece de stubs; cast explicito para el modo estricto de mypy.
    return str(jwt.encode(payload, settings.SECRET_KEY, algorithm=settings.JWT_ALGORITHM))


def create_access_token(subject: str, extra_claims: dict[str, Any] | None = None) -> str:
    """Crea un token de acceso de corta duracion.

    `extra_claims` permite incrustar el `tenant_id`/`role` para que la API
    resuelva el schema y los permisos sin un viaje extra a la BD.
    """
    return _create_token(
        subject,
        TokenType.ACCESS,
        timedelta(minutes=settings.ACCESS_TOKEN_EXPIRE_MINUTES),
        extra_claims,
    )


def create_refresh_token(subject: str, extra_claims: dict[str, Any] | None = None) -> str:
    """Crea un token de refresco de larga duracion."""
    return _create_token(
        subject,
        TokenType.REFRESH,
        timedelta(days=settings.REFRESH_TOKEN_EXPIRE_DAYS),
        extra_claims,
    )


def decode_token(token: str) -> dict[str, Any]:
    """Decodifica y valida un JWT. Lanza `JWTError` si es invalido o expiro."""
    decoded = jwt.decode(token, settings.SECRET_KEY, algorithms=[settings.JWT_ALGORITHM])
    return cast("dict[str, Any]", decoded)


__all__ = [
    "JWTError",
    "TokenType",
    "create_access_token",
    "create_refresh_token",
    "decode_token",
    "hash_password",
    "verify_password",
]
