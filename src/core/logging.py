"""Configuracion de loggers de la aplicacion."""

import logging
import sys

from src.core.config import Environment, settings


def configure_logging() -> None:
    """Configura el logging raiz segun el entorno.

    En produccion eleva el nivel a INFO y silencia el ruido de librerias;
    en local/testing usa DEBUG si `settings.DEBUG` esta activo.
    """
    level = logging.DEBUG if settings.DEBUG else logging.INFO

    handler = logging.StreamHandler(sys.stdout)
    handler.setFormatter(
        logging.Formatter(
            fmt="%(asctime)s | %(levelname)-8s | %(name)s | %(message)s",
            datefmt="%Y-%m-%d %H:%M:%S",
        )
    )

    root = logging.getLogger()
    root.handlers.clear()
    root.addHandler(handler)
    root.setLevel(level)

    # Reduce verbosidad de librerias de terceros.
    noisy_level = (
        logging.WARNING if settings.ENVIRONMENT == Environment.PRODUCTION else logging.INFO
    )
    for noisy in ("uvicorn.access", "sqlalchemy.engine"):
        logging.getLogger(noisy).setLevel(noisy_level)


def get_logger(name: str) -> logging.Logger:
    """Devuelve un logger con el namespace dado."""
    return logging.getLogger(name)
