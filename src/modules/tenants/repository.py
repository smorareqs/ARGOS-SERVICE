"""Patron Repositorio del modulo de tenants.

Capa pasiva: solo persiste y recupera datos. Incluye la creacion fisica del
schema dedicado del tenant (DDL), que es una operacion de infraestructura de
datos y por tanto pertenece a esta capa.
"""

import uuid

from sqlalchemy import select, text
from sqlalchemy.ext.asyncio import AsyncSession

from src.core.config import settings
from src.core.database import Base
from src.modules.tenants.models import Tenant


class TenantRepository:
    def __init__(self, session: AsyncSession) -> None:
        self._session = session

    async def get_by_id(self, tenant_id: uuid.UUID) -> Tenant | None:
        return await self._session.get(Tenant, tenant_id)

    async def get_by_slug(self, slug: str) -> Tenant | None:
        result = await self._session.execute(select(Tenant).where(Tenant.slug == slug))
        return result.scalar_one_or_none()

    async def list_all(self, *, limit: int = 100, offset: int = 0) -> list[Tenant]:
        result = await self._session.execute(
            select(Tenant).order_by(Tenant.created_at).limit(limit).offset(offset)
        )
        return list(result.scalars().all())

    async def add(self, tenant: Tenant) -> Tenant:
        self._session.add(tenant)
        await self._session.flush()
        return tenant

    async def create_schema(self, schema_name: str) -> None:
        """Crea el schema fisico del tenant y replica el DDL de las tablas.

        `schema_name` proviene siempre de un slug validado (`[a-z0-9_]`), por lo
        que es seguro interpolarlo en la sentencia DDL.
        """
        await self._session.execute(text(f'CREATE SCHEMA IF NOT EXISTS "{schema_name}"'))

    async def schema_exists(self, schema_name: str) -> bool:
        result = await self._session.execute(
            text(
                "SELECT EXISTS (SELECT 1 FROM information_schema.schemata "
                "WHERE schema_name = :name)"
            ),
            {"name": schema_name},
        )
        return bool(result.scalar())

    @staticmethod
    def build_schema_name(slug: str) -> str:
        """Construye el nombre de schema a partir del slug del tenant."""
        return settings.tenant_schema(slug)


# Nota: la materializacion de las tablas por-tenant (las que no son globales) se
# resuelve mediante migraciones de Alembic aplicadas por schema. `Base.metadata`
# se importa aqui para que las migraciones tengan visibilidad de los modelos.
_ = Base
