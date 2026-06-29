"""Pruebas unitarias de la logica criptografica (sin BD)."""

import pytest

from src.core.security import (
    TokenType,
    create_access_token,
    decode_token,
    hash_password,
    verify_password,
)
from src.modules.auth.roles import Role, has_at_least


@pytest.mark.unit
def test_password_hashing_roundtrip() -> None:
    hashed = hash_password("super-secret")
    assert hashed != "super-secret"
    assert verify_password("super-secret", hashed) is True
    assert verify_password("wrong", hashed) is False


@pytest.mark.unit
def test_access_token_contains_claims() -> None:
    token = create_access_token("user-123", {"role": Role.ADMIN.value})
    payload = decode_token(token)
    assert payload["sub"] == "user-123"
    assert payload["type"] == TokenType.ACCESS.value
    assert payload["role"] == Role.ADMIN.value


@pytest.mark.unit
def test_role_hierarchy() -> None:
    assert has_at_least(Role.ADMIN, Role.EMPLOYEE) is True
    assert has_at_least(Role.EMPLOYEE, Role.ADMIN) is False
    assert has_at_least(Role.PLATFORM_ADMIN, Role.ADMIN) is True
