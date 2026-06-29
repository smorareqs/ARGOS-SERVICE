"""Excepciones especificas del modulo de tenants."""

from src.core.exceptions import AlreadyExistsError, NotFoundError


class TenantNotFoundError(NotFoundError):
    message = "El tenant solicitado no existe."


class TenantSlugAlreadyExistsError(AlreadyExistsError):
    message = "Ya existe un tenant con ese slug."
