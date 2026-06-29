"""Roles del sistema (PRD seccion 3).

Jerarquia: Proveedor (ARGOS) > Cliente/Tenant > Administradores > Supervisores >
Empleados. El nivel numerico permite comparaciones de autorizacion sencillas
(mayor nivel = mas privilegios).
"""

from enum import StrEnum


class Role(StrEnum):
    # Rol de plataforma (proveedor ARGOS), opera por encima de los tenants.
    PLATFORM_ADMIN = "platform_admin"
    # Roles dentro de un tenant.
    ADMIN = "admin"  # Plataforma web: gestiona toda la operacion.
    SUPERVISOR = "supervisor"  # App movil: supervisa equipos y valida tareas.
    EMPLOYEE = "employee"  # App movil: ejecuta limpiezas y reporta incidencias.


# Nivel de privilegio por rol (para checks del tipo "requiere al menos X").
ROLE_LEVEL: dict[Role, int] = {
    Role.EMPLOYEE: 10,
    Role.SUPERVISOR: 20,
    Role.ADMIN: 30,
    Role.PLATFORM_ADMIN: 100,
}


def has_at_least(role: Role, required: Role) -> bool:
    """Indica si `role` tiene un nivel de privilegio >= `required`."""
    return ROLE_LEVEL[role] >= ROLE_LEVEL[required]
