# Guía: 2 jugadores en una sala y jugar con curl

Todos los pasos de esta guía se ejecutaron contra producción (`https://be-bbtronic-go.vercel.app`) el
2026-11-08 y las respuestas mostradas son reales (recortadas). Sirven igual en local cambiando la URL
por `http://localhost:8081`.

## 1. Cómo funciona (lo que hay que entender antes)

| Concepto | Qué es | De dónde sale | Dónde se usa |
|---|---|---|---|
| **accessToken** | La "llave" de cada jugador. Identifica quién eres; nunca se envía el nombre para decir quién eres. | Respuesta de crear jugador | Cabecera `Authorization: Bearer <token>` en todo lo demás |
| **playerId** | ID del jugador (texto con números). | Respuesta de crear jugador (`player.id`) | Para señalar a otro jugador: destinatario de una pregunta, voto, expulsión |
| **roomCode** | Código de 4 dígitos de la sala. | Respuesta de crear sala | En la URL: `/rooms/{roomCode}/...` |
| **epoch** | "Generación" de la sala. Los códigos se reutilizan cuando una sala se cierra; el epoch distingue la sala actual de una vieja con el mismo código. | Respuesta de crear sala (`epoch`) | En el estado (`?epoch=`) y en el cuerpo de cada comando. **Si no coincide → `STALE_EPOCH`.** |
| **stateVersion** | Contador del estado de la sala; sube con cada cambio. | Estado de la sala o `committedVersion` del último comando | `expectedVersion` en las acciones del anfitrión |
| **gameRunId / roundId** | ID de la partida y de la ronda en curso. | Respuesta de `game.start` (`result.gameRunId`, `result.roundId`) o el estado (`game.id`, `game.roundId`) | En los comandos de esa partida |
| **Idempotency-Key = commandId** | Identificador único de cada acción, para que un reintento no se ejecute dos veces. | Lo inventas tú (16–128 caracteres, p. ej. `cmd-0000000000000001`) | Cabecera **y** campo `commandId`, con el **mismo valor** |

Reglas del juego que el servidor hace cumplir:

- **Anfitrión** = quien crea la sala. Solo él inicia y termina juegos, pasa preguntas y arranca rondas (si otro lo intenta → `403`).
- Un jugador solo puede estar en **una sala** a la vez (`409 ALREADY_IN_ROOM`).
- Las acciones del anfitrión llevan `expectedVersion`: "hago esto sobre la versión N". Si la sala cambió entretanto
  → `409 STATE_CONFLICT` con `details.currentVersion`; repite con ese número **y una clave nueva**.
- Repetir el **mismo** curl (misma clave y mismo cuerpo) no hace nada nuevo: devuelve el mismo resultado con `replayed: true`.
  Para una acción nueva, cambia la clave en **los dos** sitios.

Cada comando devuelve `committedVersion`: es el `expectedVersion` que debes usar en la **siguiente** acción del anfitrión.

## 2. Preparación en Postman

**Import → Raw text** → pega un curl. Crea un *Environment* con estas variables y ve rellenándolas con las respuestas:

| Variable | Valor inicial | Se rellena en |
|---|---|---|
| `baseUrl` | `https://be-bbtronic-go.vercel.app` (o `http://localhost:8081`) | — |
| `tokenAna`, `anaId` | | paso 1 |
| `tokenBeto`, `betoId` | | paso 2 |
| `code`, `epoch` | | paso 3 |
| `version` | | cada respuesta (`stateVersion` o `committedVersion`) |
| `gameRunId`, `roundId` | | cada `game.start` |
| `questionInstanceId` | | `question.next` de un juego de catálogo |
| `votingRoundId` | | `vote.call` del Impostor |

> Usa nombres de jugador nuevos en cada prueba (los nombres son únicos) o cierra sesión al final (paso 31).
> En PowerShell usa `curl.exe` en vez de `curl`, o pruébalos en Git Bash / Postman.

## 3. Jugadores y sala

```bash
# 1. Crear jugador Ana  -> tokenAna = accessToken, anaId = player.id
curl -X POST '{{baseUrl}}/api/v1/players' -H 'Content-Type: application/json' \
  --data-raw '{"username":"Ana"}'
```
```json
{"accessToken":"g_ZY59nL…","expiresAt":"2026-11-09T09:26:26Z","player":{"id":"314","username":"Ana"}}
```

```bash
# 2. Crear jugador Beto -> tokenBeto, betoId
curl -X POST '{{baseUrl}}/api/v1/players' -H 'Content-Type: application/json' \
  --data-raw '{"username":"Beto"}'
```

```bash
# 3. Ana crea la sala (Ana será la anfitriona) -> code = roomCode, epoch = epoch
curl -X POST '{{baseUrl}}/api/v1/rooms' -H 'Authorization: Bearer {{tokenAna}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: room-0000000000000001' --data-raw '{}'
```
```json
{"roomCode":"2181","epoch":"056f1102-db1b-47b1-bcbe-a65f68777824","playerId":"314","stateVersion":"1","participation":"player","replayed":false}
```

```bash
# 4. Beto se une a la sala de Ana
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/members' -H 'Authorization: Bearer {{tokenBeto}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: join-0000000000000001' \
  --data-raw '{"epoch":"{{epoch}}"}'
```
```json
{"roomCode":"2181","epoch":"056f1102-…","playerId":"315","stateVersion":"2","participation":"player","replayed":false}
```

```bash
# 5. Ver el estado de la sala (cualquiera de los dos) -> version = stateVersion
curl '{{baseUrl}}/api/v1/rooms/{{code}}/state?epoch={{epoch}}' -H 'Authorization: Bearer {{tokenAna}}'
```
```json
{"room":{"code":"2181","hostPlayerId":"314","status":"lobby"},"stateVersion":"2",
 "players":[{"id":"314","username":"Ana","isHost":true,"ready":false},{"id":"315","username":"Beto","isHost":false,"ready":false}],
 "game":null, "...":"..."}
```

## 4. Preguntas Directas

Cada jugador escribe preguntas para otro; cuando todos están listos el anfitrión arranca la ronda y las va pasando.

```bash
# 6. Ana inicia el juego (version = 2) -> gameRunId, roundId, version = committedVersion
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenAna}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000001' \
  --data-raw '{"commandId":"cmd-0000000000000001","epoch":"{{epoch}}","expectedVersion":"{{version}}","operation":"game.start","payload":{"game":"preguntas-directas"}}'
```
```json
{"committedVersion":"3","result":{"gameRunId":"ad51a6d5-…","roundId":"e09d0227-…","phase":"WAITING_QUESTIONS"}}
```

```bash
# 7. Ana escribe una pregunta para Beto
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenAna}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000002' \
  --data-raw '{"commandId":"cmd-0000000000000002","epoch":"{{epoch}}","gameRunId":"{{gameRunId}}","roundId":"{{roundId}}","operation":"question.submit","payload":{"toPlayerId":"{{betoId}}","text":"¿Cuál es tu mayor miedo?","anonymous":false}}'

# 8. Beto escribe una pregunta ANÓNIMA para Ana
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenBeto}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000003' \
  --data-raw '{"commandId":"cmd-0000000000000003","epoch":"{{epoch}}","gameRunId":"{{gameRunId}}","roundId":"{{roundId}}","operation":"question.submit","payload":{"toPlayerId":"{{anaId}}","text":"¿Tu peor cita?","anonymous":true}}'

# 9. Ana se marca lista
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenAna}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000004' \
  --data-raw '{"commandId":"cmd-0000000000000004","epoch":"{{epoch}}","operation":"member.ready","payload":{"ready":true}}'

# 10. Beto se marca listo -> version = committedVersion (7)
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenBeto}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000005' \
  --data-raw '{"commandId":"cmd-0000000000000005","epoch":"{{epoch}}","operation":"member.ready","payload":{"ready":true}}'
```

```bash
# 11. Ana comprueba que todos están listos -> arranca la ronda y muestra la 1.ª pregunta
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenAna}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000006' \
  --data-raw '{"commandId":"cmd-0000000000000006","epoch":"{{epoch}}","gameRunId":"{{gameRunId}}","roundId":"{{roundId}}","expectedVersion":"{{version}}","operation":"round.checkReady","payload":{}}'
```
```json
{"committedVersion":"8","result":{"allReady":true,"started":true,"pendingPlayerIds":[],"waitingMessage":null}}
```
Si falta alguien por estar listo responde `allReady:false`, la lista `pendingPlayerIds` y un `waitingMessage` gracioso.

```bash
# 12. Ver la pregunta actual (paso 5): mira game.currentQuestion
```
```json
{"id":"172","text":"¿Cuál es tu mayor miedo?","fromPlayerName":"Ana","toPlayerId":"315","toPlayerName":"Beto","anonymous":false,"number":1,"total":2}
```

```bash
# 13. Ana pasa a la siguiente pregunta (version = 8)
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenAna}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000007' \
  --data-raw '{"commandId":"cmd-0000000000000007","epoch":"{{epoch}}","gameRunId":"{{gameRunId}}","roundId":"{{roundId}}","expectedVersion":"{{version}}","operation":"question.next","payload":{}}'
```
La pregunta anónima aparece **sin** `fromPlayerName` y con `anonymous:true`:
```json
{"id":"173","text":"¿Tu peor cita?","toPlayerId":"314","toPlayerName":"Ana","anonymous":true,"number":2,"total":2}
```
Cuando no quedan preguntas, `question.next` responde `{"exhausted":true}` y la ronda queda `COMPLETED`.
Para otra ronda: operación `round.new` (anfitrión, con `gameRunId` y `expectedVersion`).

## 5. Quién es más probable (con votación)

```bash
# 14. Ana inicia el juego (gameRunId = el de la partida anterior, version = la última)
#     -> nuevo gameRunId y roundId
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenAna}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000008' \
  --data-raw '{"commandId":"cmd-0000000000000008","epoch":"{{epoch}}","gameRunId":"{{gameRunId}}","expectedVersion":"{{version}}","operation":"game.start","payload":{"game":"quien-es-mas-probable"}}'

# 15. Ana saca una pregunta (en tus catálogos el tipo es "all") -> questionInstanceId
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenAna}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000009' \
  --data-raw '{"commandId":"cmd-0000000000000009","epoch":"{{epoch}}","gameRunId":"{{gameRunId}}","roundId":"{{roundId}}","expectedVersion":"{{version}}","operation":"question.next","payload":{"type":"all"}}'
```
```json
{"committedVersion":"12","result":{"questionId":"179","questionInstanceId":"9a8ef018-…","exhausted":false}}
```

```bash
# 16. Ana vota por Beto
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenAna}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000010' \
  --data-raw '{"commandId":"cmd-0000000000000010","epoch":"{{epoch}}","gameRunId":"{{gameRunId}}","operation":"vote.submit","payload":{"questionInstanceId":"{{questionInstanceId}}","targetPlayerId":"{{betoId}}"}}'

# 17. Beto vota por Beto (cuando votan todos, la votación se cierra sola)
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenBeto}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000011' \
  --data-raw '{"commandId":"cmd-0000000000000011","epoch":"{{epoch}}","gameRunId":"{{gameRunId}}","operation":"vote.submit","payload":{"questionInstanceId":"{{questionInstanceId}}","targetPlayerId":"{{betoId}}"}}'
```
En el estado (paso 5), `game.voting` y `game.lastVotingResult`:
```json
{"voting":{"votedCount":2,"totalVoters":2,"complete":true,"winnerPlayerId":"315"},
 "lastVotingResult":{"winnerName":"Beto","tie":false,"counts":[{"playerId":"314","votes":0},{"playerId":"315","votes":2}]}}
```
Votar dos veces → `409 ALREADY_VOTED`. Empate → gana el jugador con ID menor.

**Yo Nunca Nunca, Cultura Pendeja y Preguntas Incómodas** funcionan igual que los pasos 14–15 cambiando
`"game"` por `yo-nunca-nunca`, `cultura-pendeja` o `preguntas-incomodas` (sin votación; Incómodas además
elige un destinatario: `toPlayerName`).

## 6. El Impostor

```bash
# 18. Ana inicia con 1 impostor -> nuevo gameRunId (roundId es null en este juego)
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenAna}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000012' \
  --data-raw '{"commandId":"cmd-0000000000000012","epoch":"{{epoch}}","gameRunId":"{{gameRunId}}","expectedVersion":"{{version}}","operation":"game.start","payload":{"game":"el-impostor","impostorCount":1}}'

# 19. Cada uno consulta SU rol (es privado: prueba con tokenAna y con tokenBeto)
curl '{{baseUrl}}/api/v1/rooms/{{code}}/games/current/role?epoch={{epoch}}&gameRunId={{gameRunId}}' \
  -H 'Authorization: Bearer {{tokenAna}}'
```
```json
{"role":"civil","word":"avestruz"}        // Ana
{"role":"impostor","word":null}           // Beto
```

```bash
# 20. Ana convoca votación (dura 60 s) -> votingRoundId
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenAna}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000013' \
  --data-raw '{"commandId":"cmd-0000000000000013","epoch":"{{epoch}}","gameRunId":"{{gameRunId}}","operation":"vote.call","payload":{}}'

# 21. Ana vota por Beto
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenAna}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000014' \
  --data-raw '{"commandId":"cmd-0000000000000014","epoch":"{{epoch}}","gameRunId":"{{gameRunId}}","operation":"vote.submit","payload":{"votingRoundId":"{{votingRoundId}}","targetPlayerId":"{{betoId}}"}}'

# 22. Beto vota (se puede votar a uno mismo)
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenBeto}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000015' \
  --data-raw '{"commandId":"cmd-0000000000000015","epoch":"{{epoch}}","gameRunId":"{{gameRunId}}","operation":"vote.submit","payload":{"votingRoundId":"{{votingRoundId}}","targetPlayerId":"{{betoId}}"}}'
```
Beto (el impostor) queda eliminado y ganan los civiles. El estado muestra `game:null` y el resultado en `lastGameResult`:
```json
{"winner":"civilians","impostors":[{"id":"315","name":"Beto"}],"message":"Todos los impostores fueron eliminados",
 "finalVoting":{"eliminatedName":"Beto","tie":false,"reason":"all-voted"}}
```
Reglas: mínimo 2 jugadores; al menos 1 civil; cada jugador puede convocar una vez cada 120 s y hay 60 s de espera
global tras cada votación (`409 COOLDOWN_ACTIVE` con `retryAfterMs`). Si nadie vota en 60 s, la votación se
resuelve sola: consulta el estado y, si sale `reconciliationRequired:true`, llama a:

```bash
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/reconcile' -H 'Authorization: Bearer {{tokenAna}}' \
  -H 'Content-Type: application/json' --data-raw '{"epoch":"{{epoch}}"}'
```

## 7. Terminar, expulsar y salir

```bash
# Terminar el juego actual (vuelve al lobby; necesita gameRunId si hay juego)
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenAna}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000016' \
  --data-raw '{"commandId":"cmd-0000000000000016","epoch":"{{epoch}}","gameRunId":"{{gameRunId}}","expectedVersion":"{{version}}","operation":"game.end","payload":{}}'

# Expulsar a Beto (anfitrión). El token de Beto deja de valer.
curl -X POST '{{baseUrl}}/api/v1/rooms/{{code}}/commands' -H 'Authorization: Bearer {{tokenAna}}' \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: cmd-0000000000000017' \
  --data-raw '{"commandId":"cmd-0000000000000017","epoch":"{{epoch}}","expectedVersion":"{{version}}","operation":"member.kick","payload":{"playerId":"{{betoId}}"}}'

# 31. Cerrar sesión (borra al jugador y libera su nombre; la sala se cierra cuando sale el último)
curl -X DELETE '{{baseUrl}}/api/v1/players/me' -H 'Authorization: Bearer {{tokenAna}}'
```

## 8. Errores frecuentes (reales)

| Respuesta | Causa | Solución |
|---|---|---|
| `409 STALE_EPOCH` "la sala con ese código pertenece a otra generación" | El `epoch` enviado no es el de la sala: se dejó el texto `{{epoch}}`/`EPOCH` sin sustituir, o es de otra sala | Copia el `epoch` de la respuesta de crear sala (paso 3) en la URL del estado **y** en el cuerpo de los comandos |
| `409 STATE_CONFLICT` con `details.currentVersion` | `expectedVersion` desactualizada | Usa `currentVersion` y una **clave nueva** |
| `403 FORBIDDEN` "solo el anfitrión…" | Un jugador que no creó la sala intenta una acción de anfitrión | Usa el token del anfitrión |
| `409 ALREADY_IN_ROOM` (con `details.roomCode`) | Ese jugador ya está en una sala | Sal (paso 31) o usa otro jugador |
| `409 NAME_TAKEN` | Nombre ya registrado | Otro nombre |
| `400 INVALID_INPUT` "commandId debe coincidir con Idempotency-Key" | Cabecera y `commandId` distintos | Pon el mismo valor en ambos |
| `409 IDEMPOTENCY_KEY_REUSED` | Reusaste una clave con otro cuerpo | Clave nueva para cada acción distinta |
| `409 STALE_GAME` / `STALE_ROUND` | `gameRunId`/`roundId` de una partida o ronda anterior | Cópialos del último `game.start` o del estado |
| `409 ALREADY_VOTED` | Ya votaste en esa votación | — |
| `401 UNAUTHENTICATED` | Falta el token, está mal copiado, o el jugador fue expulsado / cerró sesión | Crea el jugador de nuevo |

Contrato completo: [API.md](API.md), [swagger.yml](swagger.yml) y, para el tiempo real, [WEBSOCKET.md](WEBSOCKET.md).
