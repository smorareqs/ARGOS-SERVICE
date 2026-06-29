"""Excepciones especificas del modulo de autenticacion."""

from src.core.exceptions import (
    AlreadyExistsError,
    AuthenticationError,
    NotFoundError,
)


class UserNotFoundError(NotFoundError):
    message = "El usuario no existe."


class EmailAlreadyExistsError(AlreadyExistsError):
    message = "Ya existe un usuario con ese correo."


class InvalidCredentialsError(AuthenticationError):
    message = "Correo o contrasena incorrectos."


class InactiveUserError(AuthenticationError):
    message = "La cuenta de usuario esta inactiva."


class InvalidTokenError(AuthenticationError):
    message = "Token invalido o expirado."
