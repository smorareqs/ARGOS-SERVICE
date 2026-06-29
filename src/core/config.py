"""Settings de variables de entorno.

ARCHITECTURE.md prohibe el uso de `.env.example`: la configuracion se gestiona
dinamicamente segun el entorno de despliegue. En local se admite un `.env`
(no versionado); en produccion las variables vienen del entorno del contenedor.
"""

from enum import StrEnum
from functools import lru_cache
from typing import Literal

from pydantic import PostgresDsn, computed_field
from pydantic_settings import BaseSettings, SettingsConfigDict


class Environment(StrEnum):
    """Entornos de ejecucion soportados."""

    LOCAL = "local"
    TESTING = "testing"
    STAGING = "staging"
    PRODUCTION = "production"


class Settings(BaseSettings):
    """Configuracion central de la aplicacion.

    Los valores se leen del entorno (o de un `.env` local). No existen defaults
    para credenciales sensibles: deben proveerse explicitamente por entorno.
    """

    model_config = SettingsConfigDict(
        env_file=".env",
        env_file_encoding="utf-8",
        env_prefix="ARGOS_",
        case_sensitive=False,
        extra="ignore",
    )

    # --- Aplicacion ---
    PROJECT_NAME: str = "ARGOS"
    API_V1_PREFIX: str = "/api/v1"
    ENVIRONMENT: Environment = Environment.LOCAL
    DEBUG: bool = False

    # --- Base de datos PostgreSQL ---
    # Host y puerto admiten default (no son secretos). Las credenciales NO:
    # deben proveerse por entorno o `.env`, o la app falla al arrancar.
    POSTGRES_HOST: str = "localhost"
    POSTGRES_PORT: int = 5432
    POSTGRES_USER: str
    POSTGRES_PASSWORD: str
    POSTGRES_DB: str
    DB_ECHO: bool = False
    DB_POOL_SIZE: int = 10
    DB_MAX_OVERFLOW: int = 20

    # --- Multi-tenancy (schema-per-tenant) ---
    # Schema donde viven las tablas globales (tenants, usuarios plataforma).
    SHARED_SCHEMA: str = "shared"
    # Prefijo aplicado al slug del tenant para nombrar su schema dedicado.
    TENANT_SCHEMA_PREFIX: str = "tenant_"

    # --- Seguridad / JWT ---
    # Sin default: es un secreto y debe inyectarse por entorno o `.env`.
    SECRET_KEY: str
    JWT_ALGORITHM: str = "HS256"
    ACCESS_TOKEN_EXPIRE_MINUTES: int = 30
    REFRESH_TOKEN_EXPIRE_DAYS: int = 7

    # --- CORS ---
    CORS_ORIGINS: list[str] = ["*"]

    @computed_field  # type: ignore[prop-decorator]
    @property
    def database_url(self) -> str:
        """URL async (asyncpg) usada por la aplicacion en runtime."""
        return str(
            PostgresDsn.build(
                scheme="postgresql+asyncpg",
                username=self.POSTGRES_USER,
                password=self.POSTGRES_PASSWORD,
                host=self.POSTGRES_HOST,
                port=self.POSTGRES_PORT,
                path=self.POSTGRES_DB,
            )
        )

    @computed_field  # type: ignore[prop-decorator]
    @property
    def sync_database_url(self) -> str:
        """URL sincrona (psycopg/por defecto) para herramientas como Alembic."""
        return str(
            PostgresDsn.build(
                scheme="postgresql+psycopg",
                username=self.POSTGRES_USER,
                password=self.POSTGRES_PASSWORD,
                host=self.POSTGRES_HOST,
                port=self.POSTGRES_PORT,
                path=self.POSTGRES_DB,
            )
        )

    @property
    def is_production(self) -> bool:
        return self.ENVIRONMENT == Environment.PRODUCTION

    def tenant_schema(self, slug: str) -> str:
        """Devuelve el nombre del schema dedicado para un tenant dado su slug."""
        return f"{self.TENANT_SCHEMA_PREFIX}{slug}"


@lru_cache
def get_settings() -> Settings:
    """Devuelve una instancia cacheada de Settings (singleton de proceso)."""
    return Settings()


LogLevel = Literal["DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"]

settings = get_settings()
