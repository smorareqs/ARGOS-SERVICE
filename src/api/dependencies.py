"""Dependencias inyectables globales de la capa API.

Aqui se construyen las cadenas de dependencias (sesion -> repositorio ->
servicio) y se resuelven el usuario actual y el schema del tenant activo.
"""

from collections.abc import Callable, Coroutine
from typing import Annotated, Any

from fastapi import Depends, HTTPException, status
from fastapi.security import OAuth2PasswordBearer
from sqlalchemy.ext.asyncio import AsyncSession

from src.core.config import settings
from src.core.database import get_session, set_search_path
from src.core.exceptions import AuthenticationError, PermissionDeniedError
from src.modules.auth.models import User
from src.modules.auth.repository import UserRepository
from src.modules.auth.roles import Role, has_at_least
from src.modules.auth.service import AuthService
from src.modules.health.service import HealthService
from src.modules.tenants.repository import TenantRepository
from src.modules.tenants.service import TenantService

oauth2_scheme = OAuth2PasswordBearer(tokenUrl=f"{settings.API_V1_PREFIX}/auth/login")

SessionDep = Annotated[AsyncSession, Depends(get_session)]
TokenDep = Annotated[str, Depends(oauth2_scheme)]


# --- Servicios (inyeccion de dependencias capa por capa) ---


def get_auth_service(session: SessionDep) -> AuthService:
    return AuthService(UserRepository(session))


def get_tenant_service(session: SessionDep) -> TenantService:
    return TenantService(TenantRepository(session))


def get_health_service() -> HealthService:
    return HealthService()


AuthServiceDep = Annotated[AuthService, Depends(get_auth_service)]
TenantServiceDep = Annotated[TenantService, Depends(get_tenant_service)]
HealthServiceDep = Annotated[HealthService, Depends(get_health_service)]


# --- Usuario actual y resolucion de tenant ---


async def get_current_user(
    token: TokenDep,
    session: SessionDep,
    auth_service: AuthServiceDep,
) -> User:
    """Valida el JWT, carga el usuario y fija el search_path al schema del tenant.

    De este modo, cualquier consulta posterior en la peticion opera de forma
    aislada dentro del schema del tenant del usuario (schema-per-tenant).
    """
    try:
        payload = auth_service.parse_token(token)
        user = await auth_service.get_current_user(payload.sub)
    except AuthenticationError as exc:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail=exc.message,
            headers={"WWW-Authenticate": "Bearer"},
        ) from exc

    if user.tenant_id is not None:
        # El schema_name se obtiene del slug validado almacenado por el tenant.
        tenant_service = TenantService(TenantRepository(session))
        tenant = await tenant_service.get_tenant(user.tenant_id)
        await set_search_path(session, tenant.schema_name)

    return user


CurrentUser = Annotated[User, Depends(get_current_user)]


def require_role(
    minimum: Role,
) -> Callable[[User], Coroutine[Any, Any, User]]:
    """Factory de dependencia que exige un rol minimo para acceder al endpoint."""

    async def _checker(current_user: CurrentUser) -> User:
        if not has_at_least(current_user.role, minimum):
            raise HTTPException(
                status_code=status.HTTP_403_FORBIDDEN,
                detail=PermissionDeniedError.message,
            )
        return current_user

    return _checker
