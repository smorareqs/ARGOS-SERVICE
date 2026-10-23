# ARGOS — Backend Service

API del sistema de gestión de personal para propiedades de alquiler de corto
plazo (Airbnb). Multi-tenant, construido con **Clean Architecture + DDD (Lite)**
según [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

## Stack

| Área            | Tecnología                                              |
| --------------- | ------------------------------------------------------- |
| Lenguaje        | Go 1.27                                                 |
| API             | huma v2 (OpenAPI 3.1 + validación) sobre chi v5         |
| Base de datos   | PostgreSQL 16 con pgx v5 (SQL explícito, sin ORM)       |
| Migraciones     | SQL embebido en el binario (`argos migrate`)            |
| Autenticación   | JWT HS256 (golang-jwt v5) + bcrypt                      |
| Dependencias    | Go modules (`go.mod` / `go.sum`)                        |
| Lint / Format   | golangci-lint v2 (gofumpt + goimports)                  |
| Testing         | `go test` + Testcontainers                              |

Un único binario, `argos`, con subcomandos: `serve` (API), `migrate` y `seed`.

## Multi-tenancy: schema-per-tenant

- Las tablas **globales** (`tenants`, `users`) viven en el schema `shared`.
- Cada cliente tiene un schema dedicado `tenant_<slug>`, creado y migrado al dar de
  alta el tenant (`tenants.Service.CreateTenant`) en una sola transacción.
- En cada petición autenticada, el middleware de autenticación
  (`internal/api/dependencies.go`) marca el contexto con el schema del tenant del
  usuario; toda transacción posterior fija `SET LOCAL search_path` a ese schema,
  aislando sus datos.

## Estructura

```
cmd/argos/          # Binario: serve, migrate, seed
internal/
├── server/         # Composición HTTP: router, CORS, logs, OpenAPI y /docs
├── app/            # Construcción de servicios (inyección de dependencias)
├── api/            # Capa de interfaz (HTTP): middlewares, errores, endpoints v1
├── core/           # Núcleo transversal: config, database, migrate, security, logging
├── modules/        # Dominio y negocio
│   ├── tenants/    # model, schemas, repository, service, errors
│   ├── auth/       # usuarios, roles, JWT
│   └── health/     # health check
└── testutil/       # PostgreSQL efímero para pruebas de integración
migrations/         # SQL de migraciones (shared/ y tenant/), embebido en el binario
docker/             # Dockerfiles (prod y dev)
```

Cada módulo de negocio sigue el flujo unidireccional:
`endpoint → service → repository → SQL`.

## Configuración (variables de entorno)

Todas las variables usan el prefijo `ARGOS_`. **No existe `.env.example`**
(prohibido por la arquitectura): en local se admite un `.env` (no versionado);
en despliegue las variables vienen del entorno y tienen prioridad sobre el `.env`.

| Variable                              | Por defecto | Descripción                              |
| ------------------------------------- | ----------- | ---------------------------------------- |
| `ARGOS_ENVIRONMENT`                   | `local`     | local/testing/staging/production         |
| `ARGOS_DEBUG`                         | `false`     | Logs en nivel DEBUG                      |
| `ARGOS_HTTP_PORT`                     | `8000`      | Puerto de la API                         |
| `ARGOS_POSTGRES_HOST`                 | `localhost` | Host de PostgreSQL                       |
| `ARGOS_POSTGRES_PORT`                 | `5432`      | Puerto                                   |
| `ARGOS_POSTGRES_USER`                 | —           | Usuario (requerida)                      |
| `ARGOS_POSTGRES_PASSWORD`             | —           | Contraseña (requerida)                   |
| `ARGOS_POSTGRES_DB`                   | —           | Base de datos (requerida)                |
| `ARGOS_DB_MAX_CONNS`                  | `30`        | Tamaño máximo del pool                   |
| `ARGOS_DB_ECHO`                       | `false`     | Registra cada sentencia SQL (con DEBUG)  |
| `ARGOS_SECRET_KEY`                    | —           | Clave para firmar JWT (requerida)        |
| `ARGOS_JWT_ALGORITHM`                 | `HS256`     | HS256, HS384 o HS512                     |
| `ARGOS_ACCESS_TOKEN_EXPIRE_MINUTES`   | `30`        | Vigencia del token de acceso             |
| `ARGOS_REFRESH_TOKEN_EXPIRE_DAYS`     | `7`         | Vigencia del token de refresco           |
| `ARGOS_CORS_ORIGINS`                  | `*`         | Orígenes permitidos, separados por comas |

## Puesta en marcha

Requisito: Go 1.27+ (`winget install GoLang.Go` en Windows) y Docker.

### Opción A — Docker Compose (recomendada)

```bash
docker compose up --build
# API:   http://localhost:8000
# Docs:  http://localhost:8000/docs   (Swagger UI)
```

Compose levanta PostgreSQL, ejecuta `argos migrate up` y arranca la API con
hot-reload ([air](https://github.com/air-verse/air)): cada cambio en un `.go` o `.sql`
recompila y reinicia.

### Opción B — Local con Go

```bash
# Levanta solo PostgreSQL con Docker (queda en localhost:5433; ver .env):
docker compose up -d db
# Aplica las migraciones (shared y todos los tenants):
go run ./cmd/argos migrate up
# Arranca la API:
go run ./cmd/argos serve
```

### Migraciones

```bash
go run ./cmd/argos migrate up                         # shared + todos los tenants
go run ./cmd/argos migrate shared status
go run ./cmd/argos migrate tenant up -tenant garrent  # un solo tenant
```

Formato de archivos y reglas en [`migrations/README.md`](migrations/README.md). La API
se niega a arrancar si `shared` tiene migraciones pendientes.

> Una base creada con el backend anterior (Alembic) se adopta sin cambios: la
> migración `0001_initial` usa `IF NOT EXISTS` con los mismos nombres de tablas,
> índices y constraints. La tabla `shared.alembic_version` queda huérfana y se puede
> borrar.

### Crear el primer platform_admin

```bash
go run ./cmd/argos seed platform-admin admin@argos.io "TuPassword" "Admin ARGOS"
```

### Imagen de producción

```bash
docker build -f docker/Dockerfile -t argos-backend .
docker run --env-file ... argos-backend migrate up   # migraciones
docker run --env-file ... argos-backend              # API (argos serve)
```

Binario estático sobre `distroless/static` (sin shell, usuario sin privilegios).

## Calidad

```bash
golangci-lint run ./...                       # lint (incluye reglas de capas, ver ARCHITECTURE.md)
golangci-lint fmt                             # formato (gofumpt + goimports)
go test ./...                                 # pruebas unitarias
go test -tags=integration -p 1 ./...          # + integración (levanta PostgreSQL con Docker)
```

## Endpoints iniciales

| Método | Ruta                     | Descripción                       |
| ------ | ------------------------ | --------------------------------- |
| GET    | `/api/v1/health`         | Estado del servicio y la BD       |
| POST   | `/api/v1/auth/login`     | Login (OAuth2 password flow, formulario) |
| POST   | `/api/v1/auth/register`  | Registro de usuario (platform_admin) |
| GET    | `/api/v1/auth/me`        | Perfil del usuario autenticado    |
| POST   | `/api/v1/tenants`        | Crear tenant (platform_admin)     |
| GET    | `/api/v1/tenants`        | Listar tenants (platform_admin)   |
| GET    | `/api/v1/tenants/{id}`   | Obtener tenant (platform_admin)   |

Contrato completo: `/api/v1/openapi.json` (o `.yaml`), generado desde el código.

Los errores siguen RFC 9457 (`application/problem+json`): el mensaje está en
`detail` y, en errores de validación (422), cada campo en `errors[]` con su
`location` (p. ej. `body.slug`).
