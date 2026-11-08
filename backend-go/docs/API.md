# BBtronic REST API v1

> Contrato máquina: **[swagger.yml](swagger.yml)** (OpenAPI 3.1, validado con `npx @redocly/cli@2 lint docs/swagger.yml`;
> `TestOpenAPIContractCoversRoutes` falla si el router y el contrato divergen).

Contrato implementado en `internal/httpapi`, `internal/coordinator` e `internal/game`.
Base: `/api/v1`. JSON UTF-8. **IDs y versiones son strings decimales** (seguros para JS).
Fechas RFC 3339 UTC. Todo `GET` es lectura pura (`Cache-Control: no-store`); ningún `GET`
consume preguntas ni ejecuta transiciones.

## Convenciones

| Tema | Regla |
|---|---|
| Autenticación | `Authorization: Bearer <accessToken>` (token del propio jugador). La identidad **nunca** sale de un campo `username`/`votingUser`. |
| Idempotencia | Mutaciones críticas exigen `Idempotency-Key` (16–128 chars `[A-Za-z0-9._-]`; usar UUID v4 o 128 bits aleatorios). Reintentar con **la misma clave y el mismo cuerpo**: se devuelve el resultado confirmado con `replayed:true`. Misma clave + otro cuerpo → `409 IDEMPOTENCY_KEY_REUSED`. Retención 24 h. |
| Versiones | `stateVersion` crece una vez por transición confirmada. Los comandos de control del anfitrión exigen `expectedVersion`; si no coincide → `409 STATE_CONFLICT` con `details.currentVersion`. Tras un 409: resincronizar y pedir nueva intención al usuario (no reintentar con otra versión automáticamente). |
| Ámbito | `epoch` (generación de la sala), `gameRunId` (partida), `roundId` (ronda), `questionInstanceId`/`votingRoundId` (votación). Ámbito viejo → `409 STALE_EPOCH/STALE_GAME/STALE_ROUND/STALE_QUESTION`. |
| CORS / Origin | Lista exacta `ALLOWED_ORIGINS`. Una petición con `Origin` no permitido → `403 FORBIDDEN`. Sin `Origin` (clientes no navegador) se acepta pero sigue necesitando Bearer. |
| Límites | Cuerpo ≤ `HTTP_BODY_LIMIT` (64 KiB) → `413`. JSON estricto: campos desconocidos → `400`. Registro: 10/min por IP; comandos: ~10/s por jugador (ráfaga 40) — límites **locales por instancia**, no globales. |

### Errores

```json
{"error":{"code":"STATE_CONFLICT","message":"…","retryable":false,"retryAfterMs":0,"details":{}},"requestId":"9f2c…"}
```

| HTTP | Códigos |
|---|---|
| 400 | `INVALID_INPUT` |
| 401 | `UNAUTHENTICATED` (incluye `WWW-Authenticate`) |
| 403 | `FORBIDDEN` (no miembro, no anfitrión, no vivo, origen no permitido) |
| 404 | `ROOM_NOT_FOUND`, `NOT_FOUND` |
| 409 | `NAME_TAKEN`, `ALREADY_IN_ROOM`, `ROOM_FULL`, `STATE_CONFLICT`, `STALE_EPOCH`, `STALE_GAME`, `STALE_ROUND`, `STALE_QUESTION`, `WRONG_GAME`, `ALREADY_VOTED`, `NO_QUESTIONS`, `QUESTION_TYPE_EXHAUSTED`, `CATALOG_EMPTY`, `ROLE_UNAVAILABLE`, `COOLDOWN_ACTIVE` (con `retryAfterMs` y `Retry-After`), `LIMIT_REACHED`, `LEASE_NOT_FOUND`, `IDEMPOTENCY_KEY_REUSED` |
| 410 | `ROOM_EXPIRED` (2 h sin actividad; cierre lógico, nunca borrado) |
| 413 | `PAYLOAD_TOO_LARGE` |
| 429 | `RATE_LIMITED` (`Retry-After`) |
| 501 | `RESET_NOT_CONFIGURED` (ver `room.reset`) |
| 503 | `DATABASE_UNAVAILABLE` (`retryable:true`) |
| 500 | `INTERNAL_ERROR` (mensaje genérico; nunca SQL/DSN) |

## Endpoints

| ID | Método y ruta | Auth | Request | Respuesta |
|---|---|---|---|---|
| N1 | `POST /players` | pública (rate-limited) | `{"username":"Ana"}` | `201 {"player":{"id","username"},"accessToken","expiresAt"}` |
| N2 | `GET /players/me` | Bearer | — | `200 {"player":{"id","username"},"room":{"code","epoch"}\|null,"expiresAt"}` |
| N3 | `DELETE /players/me` | Bearer | — | `204`. Sale de la sala (juegos reconciliados), **borra el jugador y libera el nombre** (paridad Java). Idempotente; token inválido también `204`. |
| N4 | `POST /rooms` | Bearer + `Idempotency-Key` | `{}` | `201 {"roomCode","epoch","playerId","stateVersion","participation":"player","replayed"}`. El creador es anfitrión y miembro. |
| N5 | `POST /rooms/{code}/members` | Bearer + `Idempotency-Key` | `{"epoch"?: "uuid"}` | `200 {…mismo formato…,"participation":"player"\|"spectator"}`. Repetir siendo miembro es éxito idempotente. |
| N6 | `GET /rooms/{code}/state?epoch=` | Bearer, miembro | — | `200` snapshot (abajo). |
| N7 | `POST /rooms/{code}/commands` | Bearer, miembro + `Idempotency-Key` = `commandId` | Command | `200 {"commandId","operation","committedVersion","replayed","result"}` |
| N8 | `GET /rooms/{code}/games/current/role?epoch=&gameRunId=` | Bearer, participante del Impostor | — | `200 {"gameRunId","role":"civil"\|"impostor","word":string\|null}`; espectador → `409 ROLE_UNAVAILABLE` |
| N9 | `POST /rooms/{code}/reconcile` | Bearer, miembro | `{"epoch"}` | `200 {"stateVersion","changed"}`. Materializa deadlines vencidos y transiciones de presencia. Idempotente. |
| N10 | `GET /realtime` | primer frame | Upgrade WS | Ver [WEBSOCKET.md](WEBSOCKET.md). |
| N11 | `GET /healthz` | pública | — | `200 {"status":"ok","build"}` (sin consultar DB) |
| N12 | `GET /readyz` | `Bearer OPS_HEALTH_TOKEN` | — | `200 {"status":"ready"}` / `503 {"status":"unavailable"}`; `404` si no hay token configurado |
| N13 | `POST /rooms/{code}/connections` | Bearer + `Idempotency-Key` | `{"epoch"}` | `201 {"connectionId","leaseUntil","serverTime"}` (lease para fallback HTTP) |
| N14 | `PUT /rooms/{code}/connections/{id}` | Bearer, titular | `{"epoch"}` | `200 {"connectionId","leaseUntil","serverTime"}`; lease vencido → `409 LEASE_NOT_FOUND` (abrir otro) |
| N15 | `DELETE /rooms/{code}/connections/{id}?epoch=` | Bearer, titular | — | `204` idempotente |

Rutas Java antiguas (`/api/users/*`, `/api/game-sessions/*`) **no existen** (C7): devuelven `404`.
El inventario global (Java `GET /api/users/all`, `/online`) es la herramienta `cmd/admin list-players` (A1), sin tokens.

## Comandos (N7 y WS `command`)

```json
{
  "commandId": "6f1c…(= Idempotency-Key)",
  "epoch": "uuid",
  "gameRunId": "uuid (si hay juego)",
  "roundId": "uuid (Directas: submit/next/checkReady)",
  "expectedVersion": "17",
  "operation": "question.next",
  "payload": {"type": "picante"}
}
```

| operation | Quién | payload | result | Notas |
|---|---|---|---|---|
| `room.activate` | anfitrión, `expectedVersion` | `{}` | `{"active":true}` | Java start-game (`gameStarted`). |
| `member.ready` | el propio jugador | `{"ready":true}` (opcional) | `{"playerId","ready":true}` | `ready:false` → 400 (el reset es colectivo). |
| `members.resetReady` | anfitrión | `{}` | `{"resetCount"}` | |
| `member.kick` | anfitrión | `{"playerId"}` | `{"playerId","removed":true}` | Borra al jugador (paridad Java): su token deja de valer; su socket recibe `access.revoked`. El anfitrión no puede expulsarse (usa N3). |
| `room.reset` | anfitrión | `{}` | — | **Siempre `501 RESET_NOT_CONFIGURED`**: `reset_game_data` no está definida en el repositorio (bloqueo F10). |
| `game.start` | anfitrión | `{"game":"<kind>","impostorCount"?:1}` | `{"gameRunId","roundId"\|null,"phase","countdownEndsAt"?}` | kinds: `preguntas-directas`, `yo-nunca-nunca`, `cultura-pendeja`, `quien-es-mas-probable`, `preguntas-incomodas`, `el-impostor`. Reemplaza el juego anterior. |
| `game.end` | anfitrión | `{}` | `{"endedGameRunId"}` | Vuelve al lobby; conserva miembros, preguntas históricas, historial de palabras y `lastGameResult`. |
| `round.new` | anfitrión (Directas) | `{}` | `{"roundId","phase":"WAITING_QUESTIONS"}` | |
| `round.checkReady` | anfitrión | `{}` | `{"allReady","pendingPlayerIds","waitingMessage","started"}` | Con pendientes devuelve mensaje de `waiting_messages` (`{usuarios}`). En Directas/WAITING_QUESTIONS y todos listos: exige ≥1 pregunta (`409 NO_QUESTIONS`), pasa a IN_PROGRESS y selecciona la de menor ID. |
| `question.submit` | cualquier miembro (Directas, WAITING_QUESTIONS) | `{"toPlayerId","text","anonymous":bool}` | `{"questionId"}` | Autor = token. Texto 1–255 chars. Máx. 200 por ronda. |
| `question.next` | anfitrión | Directas: `{}`; catálogos: `{"type":"tranquilo"}` | Directas `{"questionId","number","total","exhausted"}`; catálogos `{"questionInstanceId","questionId","exhausted"}` | Tipo sin disponibles → `409 QUESTION_TYPE_EXHAUSTED` sin mutar nada. Catálogo/ronda agotado → `{"exhausted":true}` y fase `COMPLETED`. |
| `vote.submit` | Probable: miembro; Impostor: vivo | Probable `{"questionInstanceId","targetPlayerId"}`; Impostor `{"votingRoundId","targetPlayerId"}` | `{"recorded":true}` | Un voto por ámbito (`409 ALREADY_VOTED`). Autovoto permitido. `expectedVersion` opcional. |
| `vote.call` | vivo (Impostor, fase `description`) | `{}` | `{"votingRoundId","deadline"}` | Cooldowns: personal 120 s, global 60 s → `409 COOLDOWN_ACTIVE` con `details.scope` y `retryAfterMs`. Votación de 60 s. |
| `votes.clear` | anfitrión (Probable) | `{"questionInstanceId"}` | `{"cleared":true}` | Mantiene la pregunta. |

## Snapshot (N6 y `room.snapshot.payload.state`)

```json
{
  "v": 1,
  "room": {"code":"4821","epoch":"uuid","hostPlayerId":"12","creatorName":"Ana","status":"lobby|active|closed","activated":true},
  "stateVersion": "18",
  "serverTime": "2026-11-08T12:00:00Z",
  "players": [{"id":"12","username":"Ana","ready":true,"isHost":true,"connectionStatus":"online|reconnecting|offline","participation":"player|spectator"}],
  "readiness": {"ready":1,"total":3,"allReady":false},
  "game": {
    "id":"<gameRunId>","kind":"preguntas-directas","roundId":"uuid|null","phase":"WAITING_QUESTIONS|IN_PROGRESS|COMPLETED|description|voting",
    "startedAt":"…","deadline":null,
    "currentQuestion": {"instanceId?":"…","id":"77","type?":"tranquilo","text":"…",
                        "fromPlayerName?":"Beto|ausente si anónimo","toPlayerId?":"13|ausente si salió","toPlayerName?":"Caro",
                        "anonymous?":true,"number?":1,"total?":3},
    "progress": {"totalQuestions":3,"shownQuestions":1} | {"remaining":40,"remainingByType":{"tranquilo":20,"picante":20}},
    "voting": {"scopeId","votedCount","totalVoters","pendingPlayerIds","complete","counts?","leaderPlayerId?","winnerPlayerId?","deadline?"} | null,
    "lastVotingResult": {"resultId","scopeId","counts":[{"playerId","votes"}],"tie","winnerPlayerId?","winnerName?","eliminatedPlayerId?","eliminatedName?","reason":"all-voted|timeout","pendingAtClose?","resolvedAt"} | null,
    "impostor": {"impostorCount","participantIds","alivePlayerIds","eliminatedPlayerIds","countdownEndsAt","votingRoundId","lastVotingCalledBy","lastVotingCalledAt","lastVotingEndedAt","globalCooldownUntil","personalCooldowns":[{"playerId","availableAt"}],"lastDeparture?":{"playerId","reason","at"}}
  },
  "lastGameResult": {"resultId","gameRunId","kind":"el-impostor","winner":"civilians|impostors","impostorPlayerIds","impostors":[{"id","name"}],"message","finalVoting","endedAt"} | null,
  "presenceObservedAt": "…",
  "reconciliationRequired": false
}
```

Reglas para el cliente:

- `game:null` = lobby. Pregunta anónima: `fromPlayerName` ausente y `anonymous:true`; la UI elige la etiqueta.
- **Privacidad**: nunca hay tokens, palabra del Impostor, lista de impostores (hasta `lastGameResult`), historial de palabras, votos crudos del Impostor ni textos de preguntas Directas no seleccionadas. El rol propio solo vía N8.
- Probable expone `counts` y `leaderPlayerId` durante la votación (Java permitía leer resultados en cualquier momento); `winnerPlayerId` al completarse. Empate → menor ID de jugador (regla C10).
- `reconciliationRequired:true` → llamar N9 y releer (el GET no muta). La cuenta 3-2-1 se calcula desde `countdownEndsAt`; la votación desde `deadline` (usar `serverTime` para compensar reloj).
- Resultados se identifican por `resultId`: mostrar cada uno una sola vez.
