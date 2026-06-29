"""Contratos de datos del modulo de health."""

from pydantic import BaseModel


class HealthStatus(BaseModel):
    status: str
    environment: str
    database: str
