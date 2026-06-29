"""Fixtures base para las pruebas.

ARCHITECTURE.md seccion 2 exige Testcontainers para las pruebas de integracion:
las pruebas marcadas con `@pytest.mark.integration` levantan un PostgreSQL real
en un contenedor efimero.
"""

from collections.abc import AsyncIterator, Iterator

import pytest
import pytest_asyncio
from sqlalchemy import text
from sqlalchemy.ext.asyncio import (
    AsyncSession,
    async_sessionmaker,
    create_async_engine,
)
from testcontainers.postgres import PostgresContainer

from src.core.config import settings
from src.core.database import Base

# Importa los modelos para que Base.metadata los conozca al crear las tablas.
from src.modules.auth import models as _auth_models  # noqa: F401
from src.modules.tenants import models as _tenant_models  # noqa: F401


@pytest.fixture(scope="session")
def postgres_container() -> Iterator[PostgresContainer]:
    """Levanta un contenedor PostgreSQL para toda la sesion de pruebas."""
    with PostgresContainer("postgres:16-alpine", driver="asyncpg") as container:
        yield container


@pytest_asyncio.fixture(scope="session")
async def db_engine(postgres_container: PostgresContainer) -> AsyncIterator[None]:
    """Crea el schema compartido y las tablas globales una vez por sesion."""
    engine = create_async_engine(postgres_container.get_connection_url(), future=True)
    async with engine.begin() as conn:
        await conn.execute(text("CREATE EXTENSION IF NOT EXISTS pgcrypto"))
        await conn.execute(text(f'CREATE SCHEMA IF NOT EXISTS "{settings.SHARED_SCHEMA}"'))
        await conn.run_sync(Base.metadata.create_all)

    # Reutiliza este engine para las sesiones de prueba.
    global _test_engine
    _test_engine = engine
    yield
    await engine.dispose()


@pytest_asyncio.fixture
async def db_session(db_engine: None) -> AsyncIterator[AsyncSession]:
    """Cede una sesion transaccional que se revierte tras cada prueba."""
    factory = async_sessionmaker(_test_engine, class_=AsyncSession, expire_on_commit=False)
    async with factory() as session:
        await session.execute(text(f'SET search_path TO "{settings.SHARED_SCHEMA}"'))
        yield session
        await session.rollback()


_test_engine = None  # type: ignore[assignment]
