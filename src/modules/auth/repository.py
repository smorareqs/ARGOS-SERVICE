"""Patron Repositorio del modulo de autenticacion."""

import uuid

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from src.modules.auth.models import User


class UserRepository:
    def __init__(self, session: AsyncSession) -> None:
        self._session = session

    async def get_by_id(self, user_id: uuid.UUID) -> User | None:
        return await self._session.get(User, user_id)

    async def get_by_email(self, email: str) -> User | None:
        result = await self._session.execute(select(User).where(User.email == email))
        return result.scalar_one_or_none()

    async def add(self, user: User) -> User:
        self._session.add(user)
        await self._session.flush()
        return user

    async def list_by_tenant(
        self, tenant_id: uuid.UUID, *, limit: int = 100, offset: int = 0
    ) -> list[User]:
        result = await self._session.execute(
            select(User)
            .where(User.tenant_id == tenant_id)
            .order_by(User.created_at)
            .limit(limit)
            .offset(offset)
        )
        return list(result.scalars().all())
