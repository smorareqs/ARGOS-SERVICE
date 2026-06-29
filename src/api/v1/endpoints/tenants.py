"""Endpoints de gestion de tenants (solo platform_admin)."""

import uuid

from fastapi import APIRouter, Depends, status

from src.api.dependencies import TenantServiceDep, require_role
from src.modules.auth.roles import Role
from src.modules.tenants.schemas import TenantCreate, TenantRead

router = APIRouter(
    prefix="/tenants",
    tags=["tenants"],
    # La administracion de tenants es competencia del proveedor (ARGOS).
    dependencies=[Depends(require_role(Role.PLATFORM_ADMIN))],
)


@router.post(
    "",
    response_model=TenantRead,
    status_code=status.HTTP_201_CREATED,
    summary="Crear tenant",
)
async def create_tenant(data: TenantCreate, service: TenantServiceDep) -> TenantRead:
    tenant = await service.create_tenant(data)
    return TenantRead.model_validate(tenant)


@router.get("", response_model=list[TenantRead], summary="Listar tenants")
async def list_tenants(
    service: TenantServiceDep,
    limit: int = 100,
    offset: int = 0,
) -> list[TenantRead]:
    tenants = await service.list_tenants(limit=limit, offset=offset)
    return [TenantRead.model_validate(t) for t in tenants]


@router.get("/{tenant_id}", response_model=TenantRead, summary="Obtener tenant")
async def get_tenant(tenant_id: uuid.UUID, service: TenantServiceDep) -> TenantRead:
    tenant = await service.get_tenant(tenant_id)
    return TenantRead.model_validate(tenant)
