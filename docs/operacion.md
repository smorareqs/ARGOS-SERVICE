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
* Las migraciones de Persona B (`0003_tasks_inventory`, `0005`, `0007`, `0009`, `0010`) llevan los
  números que les reservó `reparto.md`, que quedan *antes* de las de Persona A ya aplicadas. Una base
  que ya tenía `0004`–`0008` las rechaza ("ya hay una posterior"): en local se recrea
  (`docker compose down -v` y `argos migrate up` + `argos seed demo-tenant`). No había ningún
  ambiente con datos reales cuando se agregaron.

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

## 4.1 Mapa real del Inicio (Google Maps 3D)

La web pide `GET /api/v1/client-config` al iniciar sesión. Si `ARGOS_GOOGLE_MAPS_KEY` tiene una
clave, dibuja el mapa 3D fotorrealista de Google; si no (o si Google rechaza la clave), el mapa
ilustrado. Para activarlo:

1. En Google Cloud, en el proyecto de ARGOS: habilitar **Maps JavaScript API** y **Map Tiles API**
   (los mapas 3D fotorrealistas la usan) y tener la facturación activa.
2. Crear una clave de API, restringida a **sitios web** (`http://localhost:4200/*` en local y el
   dominio del panel en producción) y a esas dos APIs.
3. Ponerla en `ARGOS_GOOGLE_MAPS_KEY` (en local, `BACKEND/.env`) y reiniciar la API
   (`docker compose up -d api` desde `WEB/`). `ARGOS_GOOGLE_MAPS_MAP_ID` es opcional.

Es una clave de navegador: viaja al cliente, por eso la restricción por dominio es obligatoria.

## 5. Rotar claves y secretos

| Secreto | Efecto de rotarlo | Cómo |
| --- | --- | --- |
| `ARGOS_SECRET_KEY` | Invalida todos los access tokens (las personas renuevan con su refresh) y, si no hay secreto propio, cambia el del webhook | Cambiar la variable y reiniciar API y worker |
| `ARGOS_FIELD_ENCRYPTION_KEY` | Los datos cifrados (2FA, acceso a propiedades) dejan de leerse | **No rotar sin migrar**: descifrar con la anterior y cifrar con la nueva en una tarea controlada |
| `ARGOS_BILLING_WEBHOOK_SECRET` | Los webhooks firmados con el anterior responden 401 | Cambiarlo a la vez en la pasarela y en la variable |
| Claves de AWS | — | En AWS se usan roles IAM; no hay claves que rotar |
| `ARGOS_FCM_CREDENTIALS` | Hasta reiniciar el worker, el push sigue con la anterior | Generar otra clave en Firebase, cambiar la variable, reiniciar API y worker y borrar la vieja |
| `ARGOS_GOOGLE_MAPS_KEY` | Hasta reiniciar, la web sigue con la anterior | Crear otra en Google Cloud con las mismas restricciones, cambiar la variable, reiniciar la API y borrar la vieja |
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

## 9. Trabajos de Persona B

| Trabajo | Cada | Qué hace |
| --- | --- | --- |
| `mark_late_tasks` | 5 min, por empresa | Marca `is_late` en la hora local de la empresa (no se inició a la hora de inicio o no se terminó a la de fin), deja el evento `late` y publica `task.late`. |
| `task_reminders` | 5 min, por empresa | Avisa a quien tiene una tarea pendiente que empieza en los próximos 30 min (hora local), una vez por tarea. |
| `send_push` | Al crear un aviso | Manda el push a los teléfonos del usuario (FCM). Los tokens que FCM rechaza se borran. |
| `security_alarms` | 1 min, por empresa con `reservations` | Hace sonar las alarmas de salida que llegaron a su hora (aviso crítico `checkout_alarm` a quien tiene `security.dashboard`) y, si nadie las atiende en 60 min, levanta la alerta `checkout_alarm_unattended` (una vez). |
| `send_guest_push` | Al confirmar un bloque de área común | Push al teléfono del huésped (token de `POST /guest/device`). Si FCM rechaza el token no se reintenta. |
| `attendance_autoclose` | 15 min, por empresa | Cierra a las 16 h los marcajes sin salida (`autoclosed = true`), deja la bitácora y avisa a quien tiene `hr.attendance.manage` (`attendance_autoclosed`) y al colaborador. |
| `performance_snapshot` | 24 h, por empresa | Guarda en `performance_snapshots` las cifras de ayer por colaborador (limpiezas, minutos, checklist, retrasos, devueltas, reportes y diferencias abiertas). Es idempotente. |
| `incentive_evaluation` | 24 h, por empresa | Revisa cada meta activa en la última semana o el último mes cerrado y crea los incentivos `pending` de quien la cumplió (uno por meta, persona y período). Si hubo nuevos, avisa a quien tiene `hr.payroll.manage`. |
| `send_user_push` | Al mandar un mensaje de chat | Push a los teléfonos de un participante sin aviso en la bandeja (respeta la preferencia `chat_message` y las conversaciones silenciadas). |
| `chat_archive` | 1 h, por empresa | Archiva las conversaciones de huéspedes 24 h después del check-out, o al cancelar o no llegar. |
| `lock_provision` | Al confirmar o mover una reserva con cerradura | Crea el código de 6 dígitos (desde la entrada hasta 2 h después de la salida) y lo programa en cada cerradura activa de la propiedad. Si cambian las fechas revoca el anterior. Reintenta 5 veces; a la primera falla avisa `lock_failed` a quien tiene `reservations.manage`. |
| `lock_revoke` | Al cancelar o hacer check-out | Quita de la cerradura los códigos vivos de la reserva. |
| `retention_cleanup` | 1 h | Marca evidencias de más de 24 meses, borra avisos leídos de más de 180 días y trabajos terminados de más de 30 días (fallidos: 90) y luego borra de S3 los archivos vencidos (también las fotos de documento a los 90 días de la salida). |
| `worker_stats` | 5 min | Deja en el log `Worker: cola al dia` con pendientes, corriendo, terminados y fallidos de la última hora por tipo; si hay fallos, algo atorado más de 15 min o un pendiente de más de 10 min el mensaje es `Worker: revisar la cola` en nivel WARN (para una alerta de CloudWatch). |

### Avisos y tiempo real

- Los avisos se guardan en la bandeja (`GET /api/v1/notifications`) y, según las preferencias de
  cada persona, se mandan por push y por correo (plantilla `notification`). Los críticos
  (`task_assigned`, `checkout_alarm`) no se pueden apagar.
- **Push:** `ARGOS_FCM_CREDENTIALS` es la ruta al JSON de la cuenta de servicio de Firebase (o el
  JSON mismo). Sin ella, el push solo se registra en el log. Se saca en Firebase › Configuración
  del proyecto › Cuentas de servicio › Generar nueva clave privada; rotarla es generar otra,
  cambiar la variable, reiniciar el worker y borrar la vieja.
- **Tiempo real:** `GET /api/v1/ws?token=<access_token>` (WebSocket). Cada mensaje es
  `{"type": "...", "data": {...}}`: `hello` al conectar, `notification` (el aviso nuevo) y
  `notification.read`. Viaja por `NOTIFY argos_realtime` de PostgreSQL, así que funciona con
  varias réplicas de la API. El balanceador debe permitir WebSocket (en ALB está activo por
  defecto) con un tiempo de inactividad mayor a 30 s (el servidor hace ping cada 25 s).

### Huéspedes, parqueos, áreas y seguridad (fase 4)

- **Acceso del huésped:** al confirmar (o reenviar) una reserva se crean el enlace
  `argos://guest?token=…` y un código `XXXX-XXXX` que van en el correo; el reenvío revoca los
  anteriores. Solo se guarda el hash. `POST /api/v1/guest/auth` (token, o código y correo; 10
  intentos por minuto por IP) devuelve un JWT `type=guest` que vale hasta 24 h después de la
  salida, sin refresh. Cancelar la reserva lo invalida al momento.
- **Alarmas de salida:** dos por reserva confirmada (la previa, `checkout_alarm_minutes` de la
  empresa, 30 por defecto, y la de la hora exacta), en la zona de la empresa. Se reprograman si
  cambia la salida y se cancelan al cancelar o hacer check-out. Atender una marca las dos.
- **Parqueos:** la BD rechaza dos asignaciones del mismo espacio que se cruzan
  (`ex_parking_assignments_overlap`). Cancelar la reserva libera su espacio; cambiar sus fechas
  lo mueve si sigue libre o lo libera.
- **Áreas comunes:** la capacidad son personas a la vez; reservar bloquea el área
  (`SELECT … FOR UPDATE`) y suma las personas que se cruzan.
- **Datos de prueba:** `seed demo-tenant` deja espacios, áreas, placas, bloques y un huésped en
  casa por empresa con código fijo (`HUESPED-GAR` en Garrent y `HUESPED-CST` en Costa Limpia; el
  correo se imprime al sembrar).

### Asistencia, métricas e incentivos (fase 5)

- **Marcaje:** `POST /attendance/clock-in` y `/clock-out` piden GPS (lat y lng; 0,0 se rechaza) y
  una foto opcional subida con módulo `attendance` y tipo `selfie` (solo la ve RRHH). Un
  colaborador tiene a lo sumo una entrada abierta (`uq_attendance_records_open`).
- **Correcciones:** `POST /attendance` (a mano; cierra la entrada abierta de ese día si la hay) y
  `PATCH /attendance/{id}` exigen motivo, no aceptan horas futuras ni turnos de más de 24 h, no
  se pueden cruzar con otro marcaje y quedan en la bitácora con el antes y el después.
- **Nómina:** al existir `attendance_records`, el cálculo de Persona A toma días y horas de los
  marcajes cerrados (`attendance_source = attendance`) y ya no del horario. Un día cuenta si
  suma al menos `min_minutes_for_day`.
- **Métricas:** salen de las limpiezas en `done` o `validated`, del día en que terminaron (hora
  de la empresa). El propietario solo ve sus propiedades. El ranking compara solo dentro del
  mismo rol.
- **Datos de prueba:** `seed demo-tenant` deja marcajes de los dos últimos meses según el horario
  (con retrasos y ausencias), tres metas, las fotos diarias de 30 días y los incentivos del
  último período cerrado. Corre antes de la nómina de demostración.

### Chat, cerraduras y retención (fase 6)

- **Chat:** internas (1:1 que se reutiliza, o grupos con nombre) para quien tiene `chat.internal`;
  la de cada reserva se crea al confirmarla si la empresa tiene `guest_chat` (o la primera vez que
  el huésped abre el chat), con un saludo. Las de huéspedes las ve todo el que tiene `chat.guest`
  sobre la propiedad. Cada mensaje sale por el socket como `chat.message` (Data el mensaje) y por
  push. El huésped escribe solo texto; el personal puede mandar fotos (`files` módulo `chat`, tipo
  `image`, 10 MB).
- **Cerraduras:** `POST /locks/devices` (permiso `properties.manage`, bandera `smart_locks`) con
  credenciales que se guardan cifradas con `ARGOS_FIELD_ENCRYPTION_KEY`. Los códigos se guardan
  cifrados; la web solo ve los dos últimos dígitos y el huésped ve el código en `GET /guest/lock`
  desde 24 h antes de su entrada. **Mientras no haya acceso a la API de Nexxt, toda cerradura usa el
  proveedor simulado** (acepta todo salvo un `external_id` que empiece con `fail`, para probar
  fallas). Integrar Nexxt es implementar `locks.Provider` y registrarlo con `SetProvider("nexxt", …)`.
  `POST /locks/reservations/{id}/provision` reintenta a mano.
- **Retención:** ver `retention_cleanup` arriba. Los plazos están en
  `internal/modules/retention` (`EvidenceMonths`, `ReadNoticeDays`, `FinishedJobDays`,
  `FailedJobDays`).
- **Datos de prueba:** en Garrent, una 1:1 (Jorge y María), el grupo "Mantenimiento Garrent", la
  conversación del huésped `HUESPED-GAR` con una pregunta y su respuesta, y una cerradura simulada
  en su propiedad con el código ya programado.
