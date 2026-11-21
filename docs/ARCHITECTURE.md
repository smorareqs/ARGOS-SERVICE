## Documento de Definición de Arquitectura
**Enfoque:** Clean Architecture + Domain-Driven Design (Lite)
**Versión del Estándar:** 2.0.0 (Go). La 1.0.0 describía la implementación en Python/FastAPI.

---

### 1. Blueprint del Sistema de Archivos
Esta estructura de directorios es mandatoria. Todo el código de la aplicación reside en `internal/` (Go impide importarlo desde fuera del módulo) y el único punto de entrada es `cmd/argos`.

    [PROJECT_ROOT]/
    ├── .github/workflows/        # CI de GitHub Actions (Lint, Testing, Build)
    ├── cmd/argos/                # Punto de entrada: binario único con subcomandos (serve, migrate, seed)
    ├── docker/                   # Dockerfiles (Dev, Prod)
    ├── docs/                     # Documentación de arquitectura (ADRs)
    ├── migrations/               # Migraciones SQL embebidas en el binario (cambios en DDL)
    │   ├── shared/               # Schema compartido
    │   └── tenant/               # Se aplican a cada schema tenant_<slug>
    ├── internal/
    │   ├── server/               # Composición HTTP: router, CORS, logs, config OpenAPI, /docs
    │   ├── app/                  # Construcción de servicios (inyección de dependencias)
    │   ├── api/                  # CAPA DE INTERFAZ (Interface Adapters)
    │   │   ├── dependencies.go   # Middlewares globales (autenticación, rol, schema del tenant)
    │   │   ├── errors.go         # Traducción de errores de dominio a HTTP
    │   │   └── v1/               # Versión de la API
    │   │       ├── router.go     # Centralizador de rutas
    │   │       └── entidad.go    # Controladores HTTP (conectan HTTP -> Service)
    │   ├── core/                 # NÚCLEO TRANSVERSAL (Infraestructura Global)
    │   │   ├── apperr/           # Errores base del sistema
    │   │   ├── config/           # Configuración desde variables de entorno
    │   │   ├── database/         # Pool de conexiones y transacciones con search_path
    │   │   ├── logging/          # Configuración del logger (slog)
    │   │   ├── migrate/          # Motor de migraciones shared/tenant
    │   │   └── security/         # Lógica criptográfica (JWT, hashing)
    │   ├── modules/              # CAPA DE DOMINIO Y NEGOCIO (El Corazón)
    │   │   └── [nombre_modulo]/  # Ej: 'tenants', 'auth'
    │   │       ├── model.go      # Entidades de base de datos (structs con tags `db`)
    │   │       ├── schemas.go    # DTOs / contratos de datos con sus reglas de validación
    │   │       ├── repository.go # Patrón Repositorio (SQL)
    │   │       ├── service.go    # Lógica de negocio (casos de uso)
    │   │       ├── errors.go     # Errores específicos del módulo
    │   │       └── *_test.go     # Pruebas unitarias (y *_integration_test.go)
    │   └── testutil/             # PostgreSQL efímero para pruebas de integración
    ├── .golangci.yml             # Configuración de linters y formatters estricta
    └── go.mod / go.sum           # Gestión de dependencias

---

### 2. Tecnologías y Estándares de Soporte
Para garantizar que esta arquitectura se mantenga robusta y escalable, el proyecto debe adherirse a los siguientes estándares:

* **Lenguaje:** Go 1.27.
* **Framework de API:** huma v2 sobre chi v5. huma valida las entradas contra los DTOs y genera el contrato OpenAPI 3.1 desde el código (`/api/v1/openapi.json`, documentación en `/docs`).
* **Base de datos:** PostgreSQL 16 con pgx v5. SQL explícito en los repositorios; sin ORM.
* **Gestión de Dependencias:** Go modules; `go.sum` garantiza dependencias deterministas.
* **Linting y Formatting:** golangci-lint v2 con reglas estrictas, control de importaciones (gofumpt + goimports) y complejidad ciclomática (gocyclo).
* **Gestión de Entorno:** Queda estrictamente prohibido el uso de archivos `.env.example`. La configuración debe gestionarse dinámicamente según el entorno de despliegue.
* **Type Checking:** el compilador de Go más `go vet`; prohibido `any` en contratos públicos salvo donde el dato sea genuinamente dinámico (claims JWT, JSON libre).
* **Testing:** `go test` junto con Testcontainers para ejecutar pruebas de integración reales (build tag `integration`).

---

### 3. Manifiesto de Responsabilidades

**A. Capa API (`internal/api/`)**
Es la puerta de entrada de la aplicación; su única responsabilidad es gestionar el protocolo HTTP.
* **DEBE contener:** Definiciones de rutas de los endpoints, middlewares de autenticación y autorización, structs de entrada/salida de huma (que envuelven los DTOs del módulo), llamadas a la capa de Service y retorno de códigos de estado HTTP.
* **NO DEBE contener:** Lógica de negocio (condicionales complejos), consultas directas a la base de datos o reglas de validación de negocio.
* **Dependencias:** Conoce a las capas Core y Modules (específicamente Service y Schemas). Nunca se comunica de forma directa con el Repository.
* **Errores:** todo handler devuelve sus errores mediante `api.HTTPError`. huma copia el texto de cualquier otro error en la respuesta, lo que filtraría detalles internos.

**B. Capa de Dominio/Módulos (`internal/modules/`)**
Representa el núcleo del sistema y debe ser completamente agnóstica al framework web. golangci-lint (regla `depguard`) prohíbe importar `net/http`, huma, chi o `internal/api` desde aquí.

**Service (`service.go`)**
Actúa como el orquestador principal de la lógica.
* **DEBE contener:** Reglas de negocio core, validaciones lógicas complejas, orquestación de llamadas al Repository y los límites de transacción (`database.InTx` / `database.InTxValue`).
* **NO DEBE contener:** Objetos de petición o respuesta del framework, ni códigos HTTP. Devuelve errores de dominio (`apperr`), nunca errores HTTP.
* **Dependencias:** Conoce a Repository y Schemas.

**Repository (`repository.go`)**
Es el guardián de los datos y la capa de abstracción hacia la base de datos.
* **DEBE contener:** Consultas SQL, filtros y lógicas de paginación a nivel de BD. Traduce los errores del driver que tienen significado de negocio (unicidad, clave foránea) a errores del módulo.
* **NO DEBE contener:** Lógica de negocio. El repositorio es pasivo; su único fin es persistir y recuperar información.
* **Dependencias:** Conoce a los Models. Recibe un `database.Querier` (la transacción que abrió el Service).
* `updated_at`: no hay ORM que lo mantenga; todo `UPDATE` incluye `updated_at = now()`.

**Schemas (`schemas.go`)**
Definen los contratos de transferencia de datos.
* **DEBE contener:** Structs de entrada y salida con sus reglas de validación como tags (`minLength`, `format`, `enum`...), que huma aplica en la capa API y publica en el OpenAPI. Las reglas que protegen invariantes (p. ej. el slug que forma un nombre de schema) se repiten como función de normalización que el Service invoca, para que ningún camino (CLI, seeders) se las salte.
* **NO DEBE contener:** Ningún tipo de lógica de conexión a bases de datos.

**C. Aislamiento multi-tenant**
* Tablas globales en `shared`, siempre calificadas en el SQL (`shared.users`). Tablas de tenant sin calificar.
* `database.InTx` fija `SET LOCAL search_path` al schema del tenant presente en el contexto (lo marca el middleware de autenticación) seguido de `shared`. Al ser `LOCAL`, el valor muere con la transacción.
* Los identificadores (schemas) nunca se interpolan crudos: se citan con `pgx.Identifier`.

---

### 4. Flujo de Trabajo de una Petición
El ciclo de vida de una solicitud HTTP debe ser siempre unidireccional y predecible:

1. **Entrada:** El cliente realiza una petición HTTP hacia un endpoint específico de la API.
2. **Middlewares:** Si el endpoint lo exige, se valida el token, se carga el usuario, se verifica el rol y se marca el contexto con el schema del tenant.
3. **Interfaz (API):** huma valida automáticamente la estructura del cuerpo y los parámetros contra el Schema definido (422 si no cumple).
4. **Delegación:** El endpoint invoca el método correspondiente dentro del Service, pasando los datos validados.
5. **Lógica (Service):** Se ejecutan las validaciones de negocio y cualquier transformación requerida de los datos; se abre la transacción y se invoca al Repository.
6. **Persistencia (Repo):** El Repository ejecuta la operación en la base de datos y retorna la entidad resultante al Service.
7. **Transformación Final:** El Service confirma la transacción y retorna la entidad en un formato útil para la capa de negocio.
8. **Salida:** El Endpoint convierte la entidad al Schema de respuesta y emite el código de estado HTTP adecuado; los errores de dominio se traducen con `api.HTTPError`.

## Autorización (fase01.md §3.6)

* El middleware `Guard.Authenticated` resuelve el `auth.Principal` (usuario, membresía de la
  empresa activa, rol y permisos) y marca el contexto con el schema del tenant.
* Todo endpoint bajo `/api/v1` excepto `auth/*`, `health` y `public/*` declara
  `Middlewares: guard.RequirePermission(...)` (o su grupo lo aplica con `UseMiddleware`).
  La administración de la plataforma usa `guard.RequirePlatformAdmin()`.
* El catálogo de permisos y la matriz de los siete roles de sistema viven solo en
  `internal/modules/authz` (`permissions.go`, `default_roles.go`). `argos migrate up` los
  sincroniza con todos los tenants después de migrar; nunca se escriben en SQL.
* Todo repositorio de tenant recibe `scope` y lo aplica en `WHERE`; un `GetByID` fuera del
  scope devuelve `nil` (el endpoint responde 404).
* Está prohibido construir claves S3 fuera de `storage.Service.BuildKey(tenantID, ...)`.
