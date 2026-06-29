"""Factory de conexion a la base de datos PostgreSQL (SQLAlchemy 2.0 async).

Estrategia multi-tenant: **schema-per-tenant**.

- Un unico engine/pool de conexiones.
- Las tablas globales (tenants, usuarios de plataforma) viven en el schema
  compartido (`settings.SHARED_SCHEMA`).
- Cada tenant tiene un schema dedicado (`tenant_<slug>`). Para operar sobre el
  se ejecuta `SET search_path` por sesion, de modo que el mismo modelo ORM
  apunta al schema del tenant activo sin duplicar definiciones.
"""

from collections.abc import AsyncIterator
from typing import Any

from sqlalchemy import MetaData, text
from sqlalchemy.ext.asyncio import (
    AsyncEngine,
    AsyncSession,
    async_sessionmaker,
    create_async_engine,
)
from sqlalchemy.orm import DeclarativeBase

from src.core.config import settings

# Convencion de nombres para constraints/indices: indispensable para que
# Alembic genere migraciones deterministas y reversibles.
NAMING_CONVENTION = {
    "ix": "ix_%(column_0_label)s",
    "uq": "uq_%(table_name)s_%(column_0_name)s",
    "ck": "ck_%(table_name)s_%(constraint_name)s",
    "fk": "fk_%(table_name)s_%(column_0_name)s_%(referred_table_name)s",
    "pk": "pk_%(table_name)s",
}


class Base(DeclarativeBase):
    """Clase base declarativa para todos los modelos ORM."""

    metadata = MetaData(naming_convention=NAMING_CONVENTION)


def _create_engine() -> AsyncEngine:
    return create_async_engine(
        settings.database_url,
        echo=settings.DB_ECHO,
        pool_size=settings.DB_POOL_SIZE,
        max_overflow=settings.DB_MAX_OVERFLOW,
        pool_pre_ping=True,
        future=True,
    )


engine: AsyncEngine = _create_engine()

async_session_factory: async_sessionmaker[AsyncSession] = async_sessionmaker(
    bind=engine,
    class_=AsyncSession,
    expire_on_commit=False,
    autoflush=False,
)


async def set_search_path(session: AsyncSession, schema: str) -> None:
    """Fija el `search_path` de la sesion a un schema concreto.

    Se incluye siempre el schema compartido como fallback para resolver tablas
    globales referenciadas por claves foraneas.
    """
    # El schema se construye a partir de datos controlados (slug validado del
    # tenant), nunca de input crudo del cliente.
    await session.execute(text(f'SET search_path TO "{schema}", "{settings.SHARED_SCHEMA}"'))


async def get_session() -> AsyncIterator[AsyncSession]:
    """Dependencia FastAPI: cede una sesion async y gestiona commit/rollback.

    Por defecto apunta al schema compartido. La resolucion del schema del
    tenant activo se aplica en `src/api/dependencies.py` segun el token/usuario.
    """
    async with async_session_factory() as session:
        try:
            await set_search_path(session, settings.SHARED_SCHEMA)
            yield session
            await session.commit()
        except Exception:
            await session.rollback()
            raise
        finally:
            await session.close()


async def ping(**kwargs: Any) -> bool:
    """Verifica conectividad con la base de datos (usado por el health check)."""
    async with engine.connect() as conn:
        await conn.execute(text("SELECT 1"))
    return True
