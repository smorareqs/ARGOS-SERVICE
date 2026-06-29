"""Clases de excepciones base del sistema.

Estas excepciones son **agnosticas al framework**: la capa de dominio (services)
las lanza sin conocer HTTP. La capa API las traduce a respuestas HTTP mediante
manejadores registrados en `src/main.py`.
"""


class ArgosError(Exception):
    """Excepcion base de todo el dominio ARGOS."""

    message: str = "Ha ocurrido un error en el sistema."

    def __init__(self, message: str | None = None) -> None:
        self.message = message or self.message
        super().__init__(self.message)


class NotFoundError(ArgosError):
    """La entidad solicitada no existe."""

    message = "Recurso no encontrado."


class AlreadyExistsError(ArgosError):
    """Conflicto: la entidad ya existe (violacion de unicidad de negocio)."""

    message = "El recurso ya existe."


class ValidationError(ArgosError):
    """Violacion de una regla de validacion de negocio."""

    message = "Datos invalidos."


class AuthenticationError(ArgosError):
    """Credenciales invalidas o token no valido."""

    message = "No autenticado."


class PermissionDeniedError(ArgosError):
    """El usuario autenticado no tiene permisos para la operacion."""

    message = "No tiene permisos para realizar esta accion."


class TenantError(ArgosError):
    """Errores relacionados con la resolucion o aislamiento de tenants."""

    message = "Error de tenant."
