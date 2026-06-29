"""Endpoints de autenticacion (login, registro, perfil)."""

from typing import Annotated

from fastapi import APIRouter, Depends, status
from fastapi.security import OAuth2PasswordRequestForm

from src.api.dependencies import AuthServiceDep, CurrentUser
from src.modules.auth.schemas import Token, UserCreate, UserRead

router = APIRouter(prefix="/auth", tags=["auth"])


@router.post("/login", response_model=Token, summary="Iniciar sesion")
async def login(
    form_data: Annotated[OAuth2PasswordRequestForm, Depends()],
    service: AuthServiceDep,
) -> Token:
    # OAuth2PasswordRequestForm usa `username`; aqui representa el correo.
    return await service.login(form_data.username, form_data.password)


@router.post(
    "/register",
    response_model=UserRead,
    status_code=status.HTTP_201_CREATED,
    summary="Registrar usuario",
)
async def register(data: UserCreate, service: AuthServiceDep) -> UserRead:
    user = await service.register(data)
    return UserRead.model_validate(user)


@router.get("/me", response_model=UserRead, summary="Perfil del usuario actual")
async def me(current_user: CurrentUser) -> UserRead:
    return UserRead.model_validate(current_user)
