"""Centralizador de rutas de la API v1."""

from fastapi import APIRouter

from src.api.v1.endpoints import auth, health, tenants

api_router = APIRouter()
api_router.include_router(health.router)
api_router.include_router(auth.router)
api_router.include_router(tenants.router)
