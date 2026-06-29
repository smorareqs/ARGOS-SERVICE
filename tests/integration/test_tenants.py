"""Pruebas de integracion del modulo de tenants (PostgreSQL real)."""

import pytest
from sqlalchemy.ext.asyncio import AsyncSession

from src.modules.tenants.exceptions import TenantSlugAlreadyExistsError
from src.modules.tenants.repository import TenantRepository
from src.modules.tenants.schemas import TenantCreate
from src.modules.tenants.service import TenantService


@pytest.mark.integration
async def test_create_tenant_provisions_schema(db_session: AsyncSession) -> None:
    service = TenantService(TenantRepository(db_session))

    tenant = await service.create_tenant(TenantCreate(name="Acme Rentals", slug="acme"))

    assert tenant.id is not None
    assert tenant.schema_name == "tenant_acme"

    repo = TenantRepository(db_session)
    assert await repo.schema_exists("tenant_acme") is True


@pytest.mark.integration
async def test_create_tenant_duplicate_slug_fails(db_session: AsyncSession) -> None:
    service = TenantService(TenantRepository(db_session))
    await service.create_tenant(TenantCreate(name="Beta", slug="beta"))

    with pytest.raises(TenantSlugAlreadyExistsError):
        await service.create_tenant(TenantCreate(name="Beta 2", slug="beta"))
