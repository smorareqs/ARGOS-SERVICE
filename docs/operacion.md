# ARGOS · Guía de operación

Para quien despliega y mantiene el backend. Los comandos usan el binario `argos`; en contenedores
corren como `docker run --env-file … argos-backend <comando>`. La configuración completa está en el
README (todas las variables con prefijo `ARGOS_`).

## 1. Procesos

| Proceso | Comando | Réplicas |
| --- | --- | --- |
| API | `argos serve` | Las que haga falta detrás del balanceador |
| Worker | `argos worker` | Una o más (toman trabajos con `FOR UPDATE SKIP LOCKED`) |
| Migraciones | `argos migrate up` | Una vez por despliegue, antes de la API |

El balanceador revisa `GET /api/v1/health/ready`. Responde 200 solo si la base responde, S3
responde y no hay migraciones pendientes en `shared` ni en ninguna empresa; si no, 503. También
devuelve el estado de la cola (`pending`, `running`, `failed_24h`, `oldest_pending_seconds`), que
sirve para alertas: un `oldest_pending_seconds` que crece significa que el worker no está corriendo.
`GET /api/v1/health` es la verificación liviana (solo la base).

## 2. Migraciones

```bash
argos migrate up                         # shared y todas las empresas
argos migrate tenant up -tenant garrent  # una sola empresa
argos migrate shared status              # qué falta en shared
argos migrate tenant status -tenant all  # qué falta en cada empresa
argos migrate shared down                # revierte la última (solo en emergencias, con respaldo)
```

* `shared` va primero; la API no arranca si `shared` tiene pendientes.
* Una empresa nueva recibe todas las migraciones de tenant al crearse.
* Nunca se edita una migración publicada: se agrega otra (numeración en `reparto.md` §5).

## 3. Crear una empresa

Desde el panel de la plataforma (Empresas › Nueva empresa) o con la API:
`POST /api/v1/platform/tenants {name, slug, admin_email, features?, plan_key?, trial_days?}`. Crea el
schema, aplica las migraciones, siembra los siete roles, invita a quien administra y deja la
suscripción en prueba (14 días en Completo si no se indica). Todo queda en la bitácora de plataforma.

## 4. Facturación

* `billing_cycle` corre una vez al día en el worker: factura al terminar la prueba o el período, marca
  `past_due` cuando la factura vence (5 días) y `suspended` tras los días de gracia (7). Una empresa
  suspendida o cancelada queda en **solo lectura**: toda escritura responde 402, salvo facturación.
* Para correrlo ya: insertar en `shared.jobs` un trabajo `billing_cycle` sin `tenant_id` con
  `scheduled_at = now()` y un `dedupe_key` único.
* Reactivar a mano: Plataforma › Suscripciones › Reactivar (o
  `POST /api/v1/platform/subscriptions/{id}/reactivate {days}`): vuelve a operar y lo pendiente vence en
  `days` días.
* La pasarela se elige con `ARGOS_BILLING_PROVIDER` (hoy solo `fake`). Su webhook es
  `POST /api/v1/public/billing/webhook/{provider}` firmado con `ARGOS_BILLING_WEBHOOK_SECRET`.

## 5. Rotar claves y secretos

| Secreto | Efecto de rotarlo | Cómo |
| --- | --- | --- |
| `ARGOS_SECRET_KEY` | Invalida todos los access tokens (las personas renuevan con su refresh) y, si no hay secreto propio, cambia el del webhook | Cambiar la variable y reiniciar API y worker |
| `ARGOS_FIELD_ENCRYPTION_KEY` | Los datos cifrados (2FA, acceso a propiedades) dejan de leerse | **No rotar sin migrar**: descifrar con la anterior y cifrar con la nueva en una tarea controlada |
| `ARGOS_BILLING_WEBHOOK_SECRET` | Los webhooks firmados con el anterior responden 401 | Cambiarlo a la vez en la pasarela y en la variable |
| Claves de AWS | — | En AWS se usan roles IAM; no hay claves que rotar |
| Claves API de empresas | La empresa revoca la vieja y crea otra en Configuración › Integraciones | La clave nueva se muestra una sola vez |

## 6. Respaldos y restauración

* La base gestionada (RDS) debe tener instantáneas diarias (recomendado: 14 días de retención) y
  recuperación a un momento dado.
* Una vez al mes se restaura la última instantánea en staging y se verifica:
  1. `argos migrate shared status` y `argos migrate tenant status -tenant all` sin pendientes.
  2. `GET /api/v1/health/ready` en 200.
  3. Entrar con una cuenta de prueba de cada empresa y abrir Inicio, Reservas y Nómina.
* Activar el versionado del bucket. Los objetos viven en `tenants/{tenant_id}/…` y cada fila de `files` apunta a su
  clave, así que restaurar la base y el bucket al mismo momento deja todo consistente.
* Para restaurar una sola empresa: restaurar la instantánea en una base aparte y copiar su schema
  `tenant_<slug>` (y sus filas de `shared` que la mencionan) con `pg_dump --schema`.

## 7. Límites y abuso

* Por IP: 60 peticiones por minuto en el acceso (`ARGOS_RATE_LIMIT_AUTH`) y 300 en las rutas públicas
  (`ARGOS_RATE_LIMIT_PUBLIC`). Por clave API: 60 reservas por minuto (`ARGOS_RATE_LIMIT_API_KEY`).
  Responden 429 con `Retry-After`. 0 desactiva el límite.
* Los contadores viven en la memoria de cada instancia (sin Redis): con N réplicas el límite efectivo
  es N veces el configurado. Si hace falta uno global, el balanceador o un WAF de AWS lo aplican.
* El login además bloquea por cuenta tras varios intentos fallidos.

## 8. Registros

Los registros son JSON. Cada petición deja una línea `HTTP` con `method`, `path`, `status`,
`duration_ms`, `request_id`, `tenant_id` y `user_id`. El worker registra cada trabajo con su tipo,
empresa e intentos. Los errores no controlados se registran con su detalle; al cliente solo llega
"Error interno del servidor."

## 9. Pendiente de Persona B

Retención completa (evidencias, notificaciones leídas, trabajos terminados), cerraduras
(`lock_provision`, `lock_revoke`) y métricas del worker: se documentan aquí cuando estén.
