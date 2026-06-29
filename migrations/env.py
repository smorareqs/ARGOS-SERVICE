"""Entorno de migraciones de Alembic.

Usa un engine sincrono (psycopg) y toma la URL dinamicamente desde Settings,
respetando la prohibicion de `.env.example` del ARCHITECTURE.md.

Las tablas globales viven en el schema compartido; las migraciones se restringen
a ese schema con `version_table_schema` + `include_schemas`. La replicacion del
DDL hacia los schemas de cada tenant se gestiona con scripts dedicados (ver
`scripts/`), no con la migracion base.
"""

from logging.config import fileConfig

from alembic import context
from sqlalchemy import engine_from_config, pool, text

from src.core.config import settings
from src.core.database import Base

# Importar el registro asegura que TODOS los modelos esten en Base.metadata.
from src.modules import models_registry  # noqa: F401

config = context.config
config.set_main_option("sqlalchemy.url", settings.sync_database_url)

if config.config_file_name is not None:
    fileConfig(config.config_file_name)

target_metadata = Base.metadata
SHARED_SCHEMA = settings.SHARED_SCHEMA


def _include_object(obj, name, type_, reflected, compare_to) -> bool:
    # Solo gestiona objetos del schema compartido en esta migracion base.
    if type_ == "table":
        return obj.schema in (SHARED_SCHEMA, None)
    return True


def run_migrations_offline() -> None:
    context.configure(
        url=settings.sync_database_url,
        target_metadata=target_metadata,
        literal_binds=True,
        include_schemas=True,
        version_table_schema=SHARED_SCHEMA,
        include_object=_include_object,
        dialect_opts={"paramstyle": "named"},
    )
    with context.begin_transaction():
        context.run_migrations()


def run_migrations_online() -> None:
    connectable = engine_from_config(
        config.get_section(config.config_ini_section, {}),
        prefix="sqlalchemy.",
        poolclass=pool.NullPool,
    )

    with connectable.connect() as connection:
        # Garantiza la existencia del schema compartido antes de migrar.
        connection.execute(text(f'CREATE SCHEMA IF NOT EXISTS "{SHARED_SCHEMA}"'))
        connection.commit()

        context.configure(
            connection=connection,
            target_metadata=target_metadata,
            include_schemas=True,
            version_table_schema=SHARED_SCHEMA,
            include_object=_include_object,
        )
        with context.begin_transaction():
            context.run_migrations()


if context.is_offline_mode():
    run_migrations_offline()
else:
    run_migrations_online()
