"""Entidades ORM del modulo de tenants (schema compartido)."""

from sqlalchemy import Boolean, String
from sqlalchemy.orm import Mapped, mapped_column

from src.core.config import settings
from src.core.database import Base
from src.core.db_mixins import TimestampMixin, UUIDMixin


class Tenant(UUIDMixin, TimestampMixin, Base):
    """Cliente / organizacion. Cada tenant posee un schema dedicado.

    Vive siempre en el schema compartido (no es multi-tenant en si misma): es
    el catalogo que permite resolver el schema de cada cliente.
    """

    __tablename__ = "tenants"
    # Forma de tupla (inmutable): el dict de opciones es el ultimo elemento.
    __table_args__ = ({"schema": settings.SHARED_SCHEMA},)

    # Slug unico, usado para construir el nombre del schema (`tenant_<slug>`).
    slug: Mapped[str] = mapped_column(String(63), unique=True, index=True, nullable=False)
    name: Mapped[str] = mapped_column(String(255), nullable=False)
    # Nombre real del schema en PostgreSQL donde residen sus datos.
    schema_name: Mapped[str] = mapped_column(String(63), unique=True, nullable=False)
    is_active: Mapped[bool] = mapped_column(Boolean, default=True, nullable=False)
