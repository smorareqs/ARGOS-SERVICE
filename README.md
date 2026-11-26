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

Un único binario, `argos`, con subcomandos: `serve` (API), `worker` (cola de trabajos), `migrate` y `seed`.

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
├── core/           # Núcleo transversal: config, database, migrate, security, storage, mail, jobs, events, ical, xlsx, pdf, ratelimit, paging, optional
├── modules/        # Dominio y negocio
│   ├── tenants/    # model, schemas, repository, service, errors
│   ├── auth/       # usuarios, sesiones, 2FA, invitaciones
│   ├── authz/      # permisos, roles, membresías y alcance por propiedades
│   ├── files/      # archivos en S3
│   ├── audit/      # bitácora de empresa y de plataforma
│   ├── properties/ # propiedades, edificios, acceso cifrado, asignados (fase 2)
│   ├── checklists/ # plantillas de checklist (fase 2)
│   ├── employees/  # ficha laboral y horario (fase 2)
│   ├── appsync/    # GET /sync/bootstrap para la app (fase 2)
│   ├── maintenance/ # proveedores, activos, preventivos, incidencias, trabajos (fase 3)
│   ├── alerts/     # panel de alertas: levantar, reconocer, resolver (fase 3)
│   ├── dashboard/  # GET /dashboard, agregado del Inicio (fase 3)
│   ├── reservations/ # canales, iCal, reservas, huéspedes, depósitos, inspecciones (fase 4)
│   ├── payroll/    # períodos, cálculo, Bono 14, aguinaldo, IGSS, cierre, Excel y boletas (fase 5)
│   ├── disciplinary/ # acciones disciplinarias con adjuntos y acuse (fase 5)
│   ├── billing/    # planes, suscripciones, facturas, pasarela, billing_cycle, solo lectura (fase 6)
│   ├── apikeys/    # claves API de cada empresa (fase 6)
│   └── health/     # health check
└── testutil/       # PostgreSQL efímero para pruebas de integración
migrations/         # SQL de migraciones (shared/ y tenant/), embebido en el binario
docker/             # Dockerfiles (prod y dev)
docs/               # ARCHITECTURE.md, operacion.md (guía de operación) y openapi.yaml/json exportados
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
| `ARGOS_FIELD_ENCRYPTION_KEY`          | —           | 32 bytes en base64 (AES-256-GCM: secreto 2FA). Obligatoria fuera de local/testing; en local se deriva de `SECRET_KEY` |
| `ARGOS_WEB_BASE_URL`                  | `http://localhost:4200` | Base de los enlaces de los correos |
| `ARGOS_AWS_REGION`                    | `us-east-1` | Región de S3 y SES                       |
| `ARGOS_S3_BUCKET`                     | —           | Bucket (obligatoria fuera de local/testing; sin ella, almacén en memoria) |
| `ARGOS_S3_ENDPOINT_URL`               | —           | Solo local: emulador S3 (`http://rustfs:9000`) |
| `ARGOS_S3_PUBLIC_ENDPOINT_URL`        | —           | Host con que se firman las URLs que abre el cliente (`http://localhost:9000`) |
| `ARGOS_S3_ACCESS_KEY` / `_SECRET_KEY` | —           | Solo el emulador; en AWS, rol IAM        |
| `ARGOS_MAIL_DRIVER`                   | `log` en local, `ses` en el resto | `log` escribe el correo en el log y en `ARGOS_MAIL_DIR` |
| `ARGOS_MAIL_DIR`                      | `tmp/mail`  | Carpeta de los correos en modo `log`     |
| `ARGOS_SES_FROM`                      | `ARGOS <no-reply@argos.gt>` | Remitente             |
| `ARGOS_WORKER_CONCURRENCY`            | `4`         | Trabajos en paralelo del worker          |
| `ARGOS_RATE_LIMIT_AUTH`               | `60`        | Peticiones por IP y minuto en el acceso (0 = sin límite) |
| `ARGOS_RATE_LIMIT_PUBLIC`             | `300`       | Peticiones por IP y minuto en `/public/*` |
| `ARGOS_RATE_LIMIT_API_KEY`            | `60`        | Reservas por clave API y minuto          |
| `ARGOS_BILLING_PROVIDER`              | `fake`      | Pasarela de pago (hoy solo la simulada)  |
| `ARGOS_BILLING_WEBHOOK_SECRET`        | derivado de `SECRET_KEY` | Secreto HMAC de los webhooks de la pasarela |
| `ARGOS_GOOGLE_MAPS_KEY`               | —           | Clave de navegador de Google Maps (mapa 3D del Inicio). Vacía = mapa ilustrado |
| `ARGOS_FCM_CREDENTIALS`               | —           | Cuenta de servicio de Firebase (ruta al JSON o el JSON). Vacía = el push solo se registra en el log |
| `ARGOS_GOOGLE_MAPS_MAP_ID`            | —           | Estilo de mapa de Google (opcional) |

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
recompila y reinicia. También levanta:

* `worker` (`argos worker`, con su propio hot-reload): envía los correos y limpia
  archivos. Los correos **no salen a internet en local**: quedan en su log y como HTML
  en `tmp/mail/` (ahí están los enlaces de invitación y de restablecer contraseña).
* `rustfs` (emulador S3): API en `localhost:9000` y consola en `localhost:9001` con las
  claves `ARGOS_S3_ACCESS_KEY` / `ARGOS_S3_SECRET_KEY` del `.env`. La API crea el bucket
  `argos-local` al arrancar.

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

### Datos iniciales

```bash
# Administrador de la plataforma ARGOS (sin empresa):
go run ./cmd/argos seed platform-admin admin@argos.io "TuPassword" "Admin ARGOS"
# Empresas de demostración (Garrent y Costa Limpia) con una persona por rol.
# Idempotente; la contraseña de todas las cuentas sale de ARGOS_SEED_PASSWORD.
# Se niega a correr con ARGOS_ENVIRONMENT=production.
ARGOS_SEED_PASSWORD=... go run ./cmd/argos seed demo-tenant
```

En Docker Compose el servicio `seed` ejecuta `seed demo-tenant` después de las migraciones.

**Cuenta personal de desarrollo (opcional).** Si el `.env` local define
`ARGOS_SEED_DEV_EMAIL` y `ARGOS_SEED_DEV_PASSWORD` (y, si quieres, `ARGOS_SEED_DEV_NAME`),
`seed demo-tenant` crea además esa cuenta como Administrador en todas las empresas de
demostración. Como vive en el `.env`, cada quien tiene la suya, no se versiona y vuelve a
crearse aunque se borre la base. Después de cambiarla: `docker compose up --no-deps seed`.
La pila completa (web + API + base) se levanta desde `WEB/` con `npm run docker:up`, que
incluye este compose.

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
# -p 1: los paquetes comparten la base de pruebas. Incluye TestIsolationGenerated, que recorre
# todo el OpenAPI con la sesión de otra empresa (fase06.md §5.1).
```

## Endpoints

Contratos: `APP/docs/plans/fase01.md` §3.7.1 (acceso) y §3.7.2 (cuenta, equipo, archivos y
plataforma), `fase02.md` §4.1 (propiedades, edificios, checklists, fichas laborales, bitácora y
sync), `fase03.md` §5.1 (mantenimiento, alertas e Inicio) y `fase04.md` §4.1 (reservas,
huéspedes, depósitos e inspecciones). Resumen:

| Grupo | Rutas |
| ----- | ----- |
| Salud | `GET /api/v1/health` |
| Acceso | `POST /auth/login`, `/auth/select-tenant` (empresa o `platform: true`), `/auth/refresh`, `/auth/logout`, `GET /auth/me` |
| Cuenta | `PATCH /auth/me`, `GET/DELETE /auth/sessions[/{id}]`, `POST /auth/password/{forgot,reset,change}`, `POST /auth/totp/{setup,confirm,disable}` |
| Invitaciones | `GET /auth/invitations/{token}`, `POST /auth/invitations/accept`, `GET/POST /tenants/{id}/invitations`, `POST …/{inv}/resend`, `DELETE …/{inv}` |
| Empresa | `GET /tenants/{id}/members`, `PATCH …/members/{m}/role`, `POST …/{suspend,reactivate}`, `DELETE …/sessions`, `GET/POST /tenants/{id}/roles`, `PATCH/DELETE …/roles/{r}`, `GET/PATCH /tenants/{id}/settings` |
| Archivos | `POST /files/presign-upload`, `POST /files/{id}/confirm`, `GET /files/{id}/url` |
| Plataforma | `GET/POST /platform/tenants`, `GET/PATCH /platform/tenants/{id}`, `GET /platform/audit` |
| Propiedades (fase 2) | `GET/POST /properties`, `GET/PATCH/DELETE /properties/{id}`, `GET …/access-info`, `PUT …/assignments`, `PUT …/owners`, `PUT /property-assignments/{user_id}`, `PUT /property-owners/{user_id}` |
| Edificios y checklists | `GET/POST /buildings`, `GET/PATCH/DELETE /buildings/{id}`, `GET/POST /checklist-templates`, `GET/PATCH/DELETE /checklist-templates/{id}` |
| Ficha laboral | `GET/POST /employees`, `GET/PATCH /employees/{id}`, `PUT /employees/{id}/schedule` |
| Bitácora y sync | `GET /audit` (cursor), `GET /sync/bootstrap` |
| Mantenimiento (fase 3) | `GET/POST /vendors`, `GET/PATCH /vendors/{id}`, `GET/POST /assets`, `GET/PATCH /assets/{id}`, `POST …/documents`, `DELETE …/documents/{file_id}`, `GET/POST /assets/{id}/plans`, `PATCH …/plans/{plan_id}`, `GET /maintenance/plans`, `/maintenance/metrics`, `/maintenance/assignables` |
| Incidencias | `GET/POST /tickets`, `GET/PATCH /tickets/{id}`, `POST /tickets/{id}/{assign,start,resolve,close,reject,reopen}` |
| Alertas e Inicio | `GET /alerts`, `GET /alerts/summary`, `POST /alerts/{id}/{ack,resolve}`, `GET /dashboard` |
| Canales e iCal (fase 4) | `GET/POST /channels`, `PATCH /channels/{id}`, `GET /ical-feeds`, `GET/POST /properties/{id}/ical-feeds`, `PATCH/DELETE /ical-feeds/{id}`, `POST /ical-feeds/sync`, `POST /ical-feeds/{id}/sync` |
| Reservas | `GET/POST /reservations`, `GET /reservations/today`, `GET/PATCH /reservations/{id}`, `POST /reservations/{id}/{confirm,cancel,no-show,check-in,check-out,resend}`, `POST /reservations/{id}/guests`, `DELETE …/guests/{guest_id}` |
| Huéspedes | `GET/POST /guests`, `GET/PATCH /guests/{id}`, `POST /guests/{id}/documents`, `GET /guests/{id}/documents/{file_id}/url` |
| Depósito e inspecciones | `GET /reservations/{id}/deposit`, `POST …/deposit/transactions`, `POST /reservations/{id}/inspections`, `POST /inspections/{id}/findings`, `DELETE …/findings/{finding_id}` |
| Nómina (fase 5, bandera `payroll`) | `GET/PUT /payroll/settings`, `GET/POST /payroll/periods`, `GET/DELETE /payroll/periods/{id}`, `POST /payroll/periods/{id}/{compute,review,reopen,close}`, `PATCH …/entries/{entry_id}`, `GET …/export.xlsx`, `GET …/payslips.pdf`, `GET /payroll/entries?employee_id=`, `GET /payroll/entries/{id}/payslip.pdf`, `GET /payroll/me` |
| Disciplina | `GET/POST /disciplinary`, `GET /disciplinary/me`, `GET /disciplinary/{id}`, `POST /disciplinary/{id}/acknowledge` |
| Facturación (fase 6) | `GET /billing/subscription`, `GET /billing/invoices`, `GET /billing/invoices/{id}/pdf`, `POST /billing/{checkout,cancel,resume}` |
| Planes y suscripciones (plataforma) | `GET/POST /platform/plans`, `PATCH /platform/plans/{id}`, `GET /platform/subscriptions`, `POST /platform/subscriptions/{id}/reactivate`, `POST /platform/tenants/{id}/trial`, `GET /platform/tenants/{id}/invoices`, `GET /platform/invoices/{id}/pdf` |
| Integraciones | `GET/POST /api-keys`, `POST /api-keys/{id}/revoke`; públicas: `POST /public/reservations` (`X-Api-Key`), `POST /public/billing/webhook/{provider}` |
| Cliente web | `GET /client-config` (sesión iniciada): proveedor del mapa y su clave de navegador |
| Salud | `GET /health`, `GET /health/ready` (BD, S3, migraciones y cola; 503 si no está lista) |

(Todas bajo `/api/v1`.) El access token (15 min) lleva `tid` (empresa), `sid` (sesión) y
`pv` (versión de permisos del rol): deja de valer si cambian los permisos del rol, si se
suspende la membresía o si se desactiva el usuario o la empresa. Toda ruta
`/tenants/{id}` de otra empresa responde 404. Las rutas de la fase 2 filtran por el alcance de
la persona (`authz.ResolvePropertyScope`: limpieza, mantenimiento y propietarios solo ven sus
propiedades) y las listas largas paginan por cursor (`?cursor=&limit=`, `next_cursor`). El worker
corre cada hora `maintenance_due` (incidencias y alertas de preventivos), `low_stock` y
`reservation_status` (entradas y salidas automáticas) por empresa, `ical_sync` cada 30 minutos,
`retention_cleanup` (fotos de documentos vencidas) cada hora y `reservation_reminder` la víspera de
cada llegada. En local, la API sirve calendarios iCal de prueba en `GET /dev/ical/{archivo}` (los usa
el seed; `ARGOS_SEED_ICAL_BASE` cambia la base).

Una empresa suspendida o cancelada queda en solo lectura: toda escritura responde 402 salvo
`/billing/*` y `/auth/*`. El worker corre además `billing_cycle` una vez al día.

Contrato completo: `/api/v1/openapi.json` (o `.yaml`), generado desde el código. Copia exportada en
`docs/openapi.yaml` y `docs/openapi.json`; para actualizarla con la API corriendo:

```bash
curl -s http://localhost:8000/api/v1/openapi.yaml -o docs/openapi.yaml
curl -s http://localhost:8000/api/v1/openapi.json -o docs/openapi.json
```

Operación (procesos, migraciones, alta de empresas, facturación, rotación de secretos, respaldos y
límites): [docs/operacion.md](docs/operacion.md).

Los errores siguen RFC 9457 (`application/problem+json`): el mensaje está en
`detail` y, en errores de validación (422), cada campo en `errors[]` con su
`location` (p. ej. `body.slug`).
