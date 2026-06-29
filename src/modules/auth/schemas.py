"""DTOs / contratos de datos del modulo de autenticacion."""

from datetime import datetime
import uuid

from pydantic import BaseModel, ConfigDict, EmailStr, Field

from src.modules.auth.roles import Role


class UserBase(BaseModel):
    email: EmailStr
    full_name: str = Field(min_length=1, max_length=255)


class UserCreate(UserBase):
    """Datos para registrar un usuario dentro de un tenant."""

    password: str = Field(min_length=8, max_length=128)
    role: Role = Role.EMPLOYEE
    is_owner: bool = False
    tenant_id: uuid.UUID | None = None


class UserRead(UserBase):
    """Representacion publica de un usuario (nunca expone el hash)."""

    model_config = ConfigDict(from_attributes=True)

    id: uuid.UUID
    role: Role
    is_active: bool
    is_owner: bool
    tenant_id: uuid.UUID | None
    created_at: datetime
    updated_at: datetime


class LoginRequest(BaseModel):
    email: EmailStr
    password: str


class Token(BaseModel):
    access_token: str
    refresh_token: str
    token_type: str = "bearer"


class TokenPayload(BaseModel):
    """Claims decodificados de un JWT validado."""

    sub: uuid.UUID
    role: Role
    tenant_id: uuid.UUID | None = None
    type: str
