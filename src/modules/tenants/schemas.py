"""DTOs / contratos de datos del modulo de tenants."""

from datetime import datetime
import re
import uuid

from pydantic import BaseModel, ConfigDict, Field, field_validator

_SLUG_PATTERN = re.compile(r"^[a-z0-9](?:[a-z0-9_]{1,61}[a-z0-9])?$")


class TenantBase(BaseModel):
    name: str = Field(min_length=1, max_length=255)
    slug: str = Field(min_length=2, max_length=63)

    @field_validator("slug")
    @classmethod
    def validate_slug(cls, value: str) -> str:
        """El slug se usa para construir un identificador de schema en SQL.

        Se restringe a `[a-z0-9_]` para impedir inyeccion y nombres invalidos.
        """
        value = value.lower().strip()
        if not _SLUG_PATTERN.match(value):
            raise ValueError(
                "El slug debe contener solo minusculas, numeros y guiones bajos "
                "(entre 2 y 63 caracteres)."
            )
        return value


class TenantCreate(TenantBase):
    """Datos para crear un nuevo tenant."""


class TenantRead(TenantBase):
    """Representacion publica de un tenant."""

    model_config = ConfigDict(from_attributes=True)

    id: uuid.UUID
    schema_name: str
    is_active: bool
    created_at: datetime
    updated_at: datetime
