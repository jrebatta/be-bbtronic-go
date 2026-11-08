# BBtronic — Registro de avance de la migración Spring Boot → Go

- Repositorio: `D:\Proyectos\be-bbtronic-go` (git local nuevo), rama **`migration/go-backend`** (sin push).
- Backend Java de referencia: `D:\Proyectos\be-bbtronic`, rev. `f5c0daa` — **no modificado** (solo se añadió
  la sección 14 de hallazgos al `MIGRATION_PLAN.md`, archivo no versionado).
- Última actualización: 2026-11-08.

## Estado por fase

| Fase | Estado | Evidencia |
|---|---|---|
| 0 Preparación/contratos | **Completada** | Plan leído completo; Java auditado (2 controllers, 6 servicios de juego, listener WS, registry, seeders, 9 entidades, DTO, config). SHA = `f5c0daa` (origin/master solo añade un test trivial). Credenciales del `application.properties` **no usadas ni copiadas**. |
| 1 Go + WS en Vercel Preview | **Completada (aprobada con evidencia)** | `spike/vercel-ws/README.md`: 101, eco, 2 clientes, reconexión, Origin, 1009; **límite efectivo medido 300 s** (3 cortes en 16 min). |
| 2 Base Go, config, seguridad | **Completada** | `internal/config`, `auth`, `apierror`, `httpapi`; tests unitarios. |
| 3 PostgreSQL y esquema aditivo | **Completada en local**; pendiente contra Neon | Migración `0001_go_runtime.sql` aditiva e idempotente; baseline Hibernate **inferido** solo para tests; integración contra PG 17.2 local. Esquema real y `reset_game_data` **no inspeccionados** (sin acceso autorizado). |
| 4 Usuarios, salas, host, presencia | **Completada** | N1–N7, N13–N15, A1 (`cmd/admin`). |
| 5 Seis juegos | **Completada** | `internal/game` (93,5 % cobertura) + integración por juego. |
| 6 Tiempo real multi-instancia | **Completada en local** | Hub + watcher PG; 2 instancias httptest compartiendo PG: convergencia ~210–230 ms (sondeo 150–200 ms). |
| 7 Equivalencia/concurrencia/seguridad | **Completada en local** | F01–F37 (ver `backend-go/docs/TRACEABILITY.md`), C1–C14, `-race` (WSL), fuzzing, concurrencia. |
| 8 Validación Preview + frontend | **Parcial** | Backend desplegado en Preview y `cmd/smoke` 12/12 **sin BD**. Pendiente: Preview con BD de pruebas, multi-instancia real, carga. Contratos para frontend documentados. |
| 9 Corte y rollback | **No iniciada (requiere autorización)** | Runbook en `backend-go/docs/DEPLOYMENT.md`. Bloqueos: F10, BD, frontend, backup. |

Distinción solicitada:

| Nivel | Qué |
|---|---|
| Implementado | Todo lo listado en fases 2–6 y `cmd/admin`, `cmd/smoke`. |
| Probado localmente | Unitarios (`go test ./...`), integración PostgreSQL real y E2E HTTP/WS (`-tags integration`), `-race` en Linux (WSL), fuzz, benchmarks. |
| Validado en integración | Contra PostgreSQL 17.2 real local con esquema Java inferido; **no** contra el esquema real de Neon. |
| Validado mediante despliegue real | Spike Go+WS en Vercel Preview (incl. duración 300 s); backend completo en Preview **sin BD** (smoke 12/12). |
| Pendiente de verificación | BD Neon desde Vercel (TLS verify-full, pooler, prepared statements); multi-instancia real; redeploy con sockets abiertos; carga/cuotas; `reset_game_data`; E2E con el frontend Vue migrado. |

## Resultados de pruebas (ejecutadas, 2026-11-08)

| Comando | Resultado |
|---|---|
| `go build ./...`, `go vet ./...`, `go vet -tags integration ./...` | OK |
| `go test ./...` (Windows) | OK — 8 paquetes con tests |
| `go test -tags integration ./...` (PG 17.2 local aislado) | OK — 18 tests coordinador + 7 E2E + unitarios |
| `go test -race -tags integration ./...` (WSL2 Ubuntu 24.04, Go 1.26.4, gcc 13.3) | OK, sin carreras |
| `go test -race` en Windows | **No ejecutable**: sin gcc/mingw (clang MSVC rechaza `-mthreads`) |
| Fuzz `FuzzApply` 20 s / `FuzzEnvelopeDecode` 10 s | ~2,5 M ejecuciones, 0 fallos |
| Cobertura (unit + integración, `-coverpkg=./internal/...`) | **81,2 %** total; game 93,5 · auth 94,4 · config 93,2 · apierror 92,6 · coordinator 78,5 · httpapi 77,1 · realtime 74,8 · store 67,2 |
| `cmd/smoke -full` contra binario local + PG | 17/17 |
| `cmd/smoke` contra Vercel Preview (sin BD) | 12/12 (`…-8zqg9lzym-…vercel.app`) |
| `spike wsprobe` contra Vercel Preview | 12/12 + sesión 16 min con 3 rotaciones de plataforma |

Benchmarks (local, no extrapolables a Neon): snapshot 30 jugadores 14–28 µs / 4–5 KB; `runtime_state` con
catálogo de 1000 preguntas 24,5 KB (enc+dec 0,5 ms); comando completo ~1,28 → **~1,15 ms** tras eliminar
3 round-trips (13 → 10 sentencias por `question.submit`).

## Problemas encontrados y corregidos durante la implementación

1. **Deadlock potencial en logout**: bloqueaba usuario antes que sala (orden inverso al global sala→usuarios).
   Corregido: lectura sin bloqueo + bloqueo sala→miembros + recomprobación.
2. **Orden del protocolo WS**: `auth.ok` podía perder la carrera del `select` contra el snapshot inicial; el
   cliente descartaba el snapshot (test intermitente). Corregido escribiendo `auth.ok` antes de arrancar el writer;
   el helper E2E exige ahora orden estricto (10/10 + 5×suite).
3. **Timeout de lectura WS**: cancelar el contexto de `Read` en coder cierra el socket sin código; reemplazado por
   watchdog que cierra limpio con 4001.
4. Optimización medida (SAVEPOINT solo cuando hay reconciliación; recibo con upsert condicional).

## Decisiones técnicas y cambios respecto al plan

- **Vercel**: la contradicción documental (Fluid sin Go) se resolvió con evidencia: WS Go funciona en Hobby;
  máximo **300 s** → `WS_ROTATE_SECONDS=270`. El primer despliegue de un proyecto Vercel va siempre a su
  producción: el spike creó `bbtronic-ws-spike` (proyecto de pruebas aislado, no BBtronic) y su primera URL quedó
  como "production" de ese proyecto; todo lo demás fue Preview. Quedó además un proyecto vacío
  `bbtronic-backend-preview` sin despliegues (puede borrarse: `npx vercel project rm bbtronic-backend-preview`).
- **Salas legacy**: no se hace backfill; las filas Java (`epoch IS NULL`) quedan intactas e invisibles. Índice
  único parcial de código solo para salas Go abiertas.
- **Borrado de jugadores**: logout, kick y salida definitiva borran el `users` row (paridad Java: libera el
  nombre, invalida el token). Las preguntas conservan nombres en texto.
- **Sala vacía** → cierre lógico (`closed_at`); salas inactivas 2 h → `410 ROOM_EXPIRED` (cierre lógico).
- **Registro huérfano** (nunca unido) libera el nombre tras 15 min; filas Java nunca se reclaman.
- **Probable**: conteos parciales y líder visibles durante la votación (Java permitía leer resultados en cualquier
  momento); ganador al completar. Empate → menor ID (C10).
- **Impostor**: se mantiene que `vote.call` se permita durante la cuenta 3-2-1 (Java no lo impedía). Una salida
  no reinicia el cooldown global (Java lo hacía de forma accidental en `checkEndCondition`).
- **Límites nuevos documentados**: nombre 1–32 caracteres; texto de pregunta ≤255 (varchar Hibernate por defecto);
  200 preguntas/ronda; 30 miembros/sala; 4 conexiones/jugador; rate limit local por instancia.
- **Skill golang-database vs plan**: la skill desaconseja generar esquemas; prevalece el plan (migración aditiva
  versionada), marcada como "requiere revisión humana" y nunca ejecutada en el arranque.
- `room.reset` responde `501 RESET_NOT_CONFIGURED`: **no cumple F10** hasta conocer `reset_game_data`.

## Skills utilizadas (instrucciones leídas)

`golang-how-to` (orquestación), `golang-project-layout`, `golang-database` (+ transactions), `golang-error-handling`,
`golang-concurrency`, `golang-context`, `golang-security`, `golang-testing`, `golang-performance`.

## Bloqueos y pendientes externos

1. **`reset_game_data`**: definición desconocida (SQL local vacío). F10 bloquea el corte.
2. **Esquema real de Neon**: ejecutar `cmd/admin inspect-schema` con acceso autorizado de lectura y revisar la
   migración 0001 contra él (tipos, duplicados de `session_code`, índices existentes).
3. **Rotación de credenciales** PostgreSQL expuestas en `application.properties` (responsable del proyecto).
4. **Preview con BD de pruebas** (rama Neon) para validar TLS/pooler/prepared statements, latencias reales,
   multi-instancia y `cmd/smoke -full` en Vercel.
5. **Frontend Vue**: migrar al contrato v1 (`backend-go/docs/API.md`, `WEBSOCKET.md`); E2E real pendiente.
6. Preparar versión Java de retorno sin `clearOnStartup`/seeds/`ddl-auto` antes de cualquier corte.

## Próximas tareas

1. Obtener acceso a una rama Neon de pruebas → `admin inspect-schema` → revisar/aplicar 0001 → Preview con BD.
2. Resolver la semántica de `reset_game_data` con el responsable del producto.
3. Migración del frontend y E2E; después fase 9 con autorización explícita.

## Inspección del esquema real (2026-11-08, solo lectura)

Neon `ep-dawn-tooth-…eastus2.azure`, base **`bd_bbtronic`** (la base `neondb` del mismo servidor está vacía).
PostgreSQL 17.11. Catálogos: YNN 340, Cultura 386, Probable 382, Incómodas 237, palabras 150, plantillas 3.

| Hallazgo | Impacto | Acción |
|---|---|---|
| **`reset_game_data` no existe** | El endpoint Java `POST /reset` fallaba siempre | F10 sigue sin semántica que migrar; `room.reset` → 501 |
| `ux_game_session_code` UNIQUE(session_code) | Códigos de salas cerradas quedarían ocupados | Al cerrar, Go pone `session_code=NULL` y guarda `runtime_state.closedCode` (test `TestClosedRoomReleasesCode`) |
| `shown_questions.seq` identity (no está en la entidad) | Ninguno: se genera solo | Añadido al baseline de tests |
| Índices existentes `idx_users_game_session`, `idx_question_session_round`, `idx_shown_questions_session` | La migración habría duplicado índices | 0001 solo crea índices si no existe uno equivalente |
| Tabla `auth_sessions` (0 filas, FK a users ON DELETE CASCADE) | Ninguno | No se usa; añadida al baseline |
| Todos los `id` son `GENERATED BY DEFAULT AS IDENTITY`; sin códigos duplicados | Compatible | — |

`.env` local apunta a `bd_bbtronic`. Pendiente: aplicar `0001` (confirmación del usuario).
