"""Logica del modulo de health: verifica el estado de la aplicacion y la BD."""

from src.core.config import settings
from src.core.database import ping
from src.modules.health.schemas import HealthStatus


class HealthService:
    async def check(self) -> HealthStatus:
        """Devuelve el estado del servicio y la conectividad con PostgreSQL."""
        try:
            await ping()
            db_status = "up"
        except Exception:
            db_status = "down"

        return HealthStatus(
            status="ok" if db_status == "up" else "degraded",
            environment=settings.ENVIRONMENT.value,
            database=db_status,
        )
