-- Script de inicializacion de PostgreSQL (se ejecuta una sola vez al crear el
-- volumen de datos). Habilita las extensiones que usan los modelos.

-- gen_random_uuid() para las claves primarias UUID (PostgreSQL 13+ la incluye
-- de serie via pgcrypto en versiones previas).
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Schema compartido donde viven las tablas globales (tenants, users).
CREATE SCHEMA IF NOT EXISTS shared;
