"""Logica de negocio (casos de uso) del modulo de tenants.

Orquesta el alta de un tenant: valida unicidad, persiste el registro en el
schema compartido y aprovisiona su schema dedicado (schema-per-tenant).
"""

import uuid

from src.core.logging import get_logger
from src.modules.tenants.exceptions import (
    TenantNotFoundError,
    TenantSlugAlreadyExistsError,
)
from src.modules.tenants.models import Tenant
from src.modules.tenants.repository import TenantRepository
from src.modules.tenants.schemas import TenantCreate

logger = get_logger(__name__)


class TenantService:
    def __init__(self, repository: TenantRepository) -> None:
        self._repo = repository

    async def create_tenant(self, data: TenantCreate) -> Tenant:
        """Crea un tenant y aprovisiona su schema dedicado."""
        if await self._repo.get_by_slug(data.slug) is not None:
            raise TenantSlugAlreadyExistsError

        schema_name = self._repo.build_schema_name(data.slug)
        tenant = Tenant(name=data.name, slug=data.slug, schema_name=schema_name)
        tenant = await self._repo.add(tenant)

        # Aprovisionamiento del schema-per-tenant.
        await self._repo.create_schema(schema_name)
        logger.info("Tenant creado: slug=%s schema=%s", data.slug, schema_name)

        return tenant

    async def get_tenant(self, tenant_id: uuid.UUID) -> Tenant:
        tenant = await self._repo.get_by_id(tenant_id)
        if tenant is None:
            raise TenantNotFoundError
        return tenant

    async def get_tenant_by_slug(self, slug: str) -> Tenant:
        tenant = await self._repo.get_by_slug(slug)
        if tenant is None:
            raise TenantNotFoundError
        return tenant

    async def list_tenants(self, *, limit: int = 100, offset: int = 0) -> list[Tenant]:
        return await self._repo.list_all(limit=limit, offset=offset)
