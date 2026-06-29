"""Punto de entrada de la aplicacion FastAPI (ARGOS).

Registra el router v1, la configuracion CORS y los manejadores que traducen las
excepciones de dominio (agnosticas a HTTP) en respuestas HTTP coherentes.
"""

from collections.abc import AsyncIterator, Awaitable, Callable
from contextlib import asynccontextmanager

from fastapi import FastAPI, Request, Response, status
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse

from src.api.v1.router import api_router
from src.core.config import settings
from src.core.exceptions import (
    AlreadyExistsError,
    ArgosError,
    AuthenticationError,
    NotFoundError,
    PermissionDeniedError,
    ValidationError,
)
from src.core.logging import configure_logging, get_logger
from src.modules import models_registry  # noqa: F401 - registra todos los modelos ORM

logger = get_logger(__name__)

# Mapea cada excepcion de dominio a su codigo HTTP correspondiente.
_EXCEPTION_STATUS: list[tuple[type[ArgosError], int]] = [
    (NotFoundError, status.HTTP_404_NOT_FOUND),
    (AlreadyExistsError, status.HTTP_409_CONFLICT),
    (ValidationError, status.HTTP_422_UNPROCESSABLE_ENTITY),
    (AuthenticationError, status.HTTP_401_UNAUTHORIZED),
    (PermissionDeniedError, status.HTTP_403_FORBIDDEN),
]


@asynccontextmanager
async def lifespan(_: FastAPI) -> AsyncIterator[None]:
    configure_logging()
    logger.info("Iniciando %s en entorno '%s'", settings.PROJECT_NAME, settings.ENVIRONMENT)
    yield
    logger.info("Deteniendo %s", settings.PROJECT_NAME)


def create_app() -> FastAPI:
    app = FastAPI(
        title=settings.PROJECT_NAME,
        version="0.1.0",
        debug=settings.DEBUG,
        lifespan=lifespan,
        openapi_url=f"{settings.API_V1_PREFIX}/openapi.json",
        docs_url="/docs",
    )

    app.add_middleware(
        CORSMiddleware,
        allow_origins=settings.CORS_ORIGINS,
        allow_credentials=True,
        allow_methods=["*"],
        allow_headers=["*"],
    )

    _register_exception_handlers(app)
    app.include_router(api_router, prefix=settings.API_V1_PREFIX)
    return app


# Starlette tipa los handlers con `Exception`; las funciones internas reciben
# en runtime la subclase de ArgosError que se registro para ese tipo.
ExceptionHandler = Callable[[Request, Exception], Awaitable[Response]]


def _register_exception_handlers(app: FastAPI) -> None:
    def _make_handler(http_status: int) -> ExceptionHandler:
        async def handler(_: Request, exc: Exception) -> Response:
            return JSONResponse(
                status_code=http_status,
                content={"detail": getattr(exc, "message", str(exc))},
            )

        return handler

    for exc_type, http_status in _EXCEPTION_STATUS:
        app.add_exception_handler(exc_type, _make_handler(http_status))

    # Cualquier ArgosError no mapeada se trata como error interno controlado.
    async def generic_handler(_: Request, exc: Exception) -> Response:
        logger.exception("Error de dominio no mapeado: %s", exc)
        return JSONResponse(
            status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
            content={"detail": getattr(exc, "message", str(exc))},
        )

    app.add_exception_handler(ArgosError, generic_handler)


app = create_app()
