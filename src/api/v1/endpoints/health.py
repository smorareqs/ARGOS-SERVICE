"""Endpoint de health check."""

from fastapi import APIRouter

from src.api.dependencies import HealthServiceDep
from src.modules.health.schemas import HealthStatus

router = APIRouter(prefix="/health", tags=["health"])


@router.get("", response_model=HealthStatus, summary="Estado del servicio")
async def healthcheck(service: HealthServiceDep) -> HealthStatus:
    return await service.check()
