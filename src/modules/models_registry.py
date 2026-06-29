"""Registro central de modelos ORM.

Importar este modulo garantiza que TODAS las entidades queden registradas en
`Base.metadata`. Es indispensable para que SQLAlchemy resuelva las claves
foraneas entre modulos (p.ej. `users.tenant_id -> shared.tenants.id`) sin
importar el orden ni el entrypoint (app, seeders, migraciones).

Cada nuevo modulo con modelos debe anadirse aqui.
"""

from src.modules.auth.models import User
from src.modules.tenants.models import Tenant

__all__ = ["Tenant", "User"]
