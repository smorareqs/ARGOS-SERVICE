"""Entidades ORM del modulo de autenticacion.

El usuario vive en el schema compartido y referencia su tenant. El aislamiento
de los datos operativos (propiedades, itinerarios, etc.) se logra con el schema
dedicado del tenant; el catalogo de usuarios es global para permitir el login
antes de resolver el schema.
"""

import uuid

from sqlalchemy import Boolean, ForeignKey, String
from sqlalchemy.dialects.postgresql import UUID as PG_UUID
from sqlalchemy.orm import Mapped, mapped_column

from src.core.config import settings
from src.core.database import Base
from src.core.db_mixins import TimestampMixin, UUIDMixin
from src.modules.auth.roles import Role


class User(UUIDMixin, TimestampMixin, Base):
    """Usuario del sistema, asociado a un tenant y a un rol.

    `is_owner` modela la condicion de propietario, que el PRD define como un
    atributo y no como un rol (seccion 3).
    """

    __tablename__ = "users"
    # Forma de tupla (inmutable): el dict de opciones es el ultimo elemento.
    __table_args__ = ({"schema": settings.SHARED_SCHEMA},)

    email: Mapped[str] = mapped_column(String(320), unique=True, index=True, nullable=False)
    hashed_password: Mapped[str] = mapped_column(String(255), nullable=False)
    full_name: Mapped[str] = mapped_column(String(255), nullable=False)
    role: Mapped[Role] = mapped_column(String(32), nullable=False, default=Role.EMPLOYEE)
    is_active: Mapped[bool] = mapped_column(Boolean, default=True, nullable=False)
    is_owner: Mapped[bool] = mapped_column(Boolean, default=False, nullable=False)

    # Los platform_admin no pertenecen a ningun tenant -> nullable.
    tenant_id: Mapped[uuid.UUID | None] = mapped_column(
        PG_UUID(as_uuid=True),
        ForeignKey(f"{settings.SHARED_SCHEMA}.tenants.id", ondelete="CASCADE"),
        index=True,
        nullable=True,
    )
