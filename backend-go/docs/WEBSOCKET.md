# BBtronic WebSocket protocol v1

Endpoint: `GET /api/v1/realtime` (WSS en producción). JSON nativo (sin STOMP/SockJS), frames de texto,
máximo `WS_FRAME_LIMIT` (16 KiB; mayor → cierre 1009). Compresión desactivada.

## Validación en Vercel (evidencia, 2026-11-08)

Go + `github.com/coder/websocket` v1.8.15 sobre el preset `go`, **Preview real** (Hobby):
101, eco Unicode, ping/pong, 2 clientes simultáneos en la misma instancia, ≥3 reconexiones, Origin 403,
límite 1009. **Duración efectiva: el servidor recibe EOF a los 300 s exactos** (el cliente lo nota a ~310 s),
sin frame de cierre ni señal previa. Por eso el servidor **rota a los 270 s** (`WS_ROTATE_SECONDS`).
El backend completo (middleware real) también respondió 101 en Preview. Detalle: `spike/vercel-ws/README.md`.

## Sobre

```json
{"v":1,"type":"…","requestId":"…","eventId":"…","roomCode":"…","epoch":"…","stateVersion":"…","reason":"…","payload":{}}
```

## Ciclo de vida

1. Conectar con `Origin` permitido (otro → HTTP 403 en el handshake).
2. **Primer frame en ≤ 5 s** (si no → cierre 4002):
   ```json
   {"v":1,"type":"auth","requestId":"r1","payload":{"accessToken":"<propio>","roomCode":"4821","epoch":"uuid"}}
   ```
   Token/miembro/epoch inválidos → `error` + cierre **1008** (no reconectar con esa credencial).
   DB no disponible → `error` (`DATABASE_UNAVAILABLE`, retryable) + cierre **1013**.
3. El servidor responde **en este orden estricto**:
   `auth.ok` → `room.snapshot` (estado completo).
   ```json
   {"v":1,"type":"auth.ok","requestId":"r1","payload":{"connectionId":"uuid","playerId":"12","roomCode":"4821","epoch":"uuid","stateVersion":"18","serverTime":"…","leaseUntil":"…","rotateAfterSeconds":270,"leaseRenewalSeconds":15}}
   ```
4. Enviar `heartbeat` cada `leaseRenewalSeconds` (15 s). Sin ningún frame durante 45 s → cierre **4001**.
5. Antes de `rotateAfterSeconds` el servidor envía `server.reconnect` y cierra con **1012**: reconectar de inmediato.

## Mensajes

| type | Dirección | payload | Respuesta / uso |
|---|---|---|---|
| `auth` | C→S | `{accessToken, roomCode, epoch}` | `auth.ok` + `room.snapshot` |
| `heartbeat` | C→S | `{connectionId, lastVersion}` | `heartbeat.ack {connectionId, leaseUntil, serverTime, stateVersion}`. Si el lease expiró el servidor reabre otro (nuevo `connectionId` en el ack). |
| `state.request` | C→S | `{knownVersion?}` | `room.snapshot` vigente (aunque la versión coincida) |
| `command` | C→S | Command de [API.md](API.md) (`commandId` = `requestId`) | `command.result {commandId, operation, committedVersion, replayed, result}` o `error`. Mismo motor y recibos que N7. |
| `room.snapshot` | S→C | `{state}` (snapshot de API.md) | `eventId = epoch:stateVersion`. Reemplazo completo. |
| `error` | S→C | `{error:{code,message,retryable,retryAfterMs?,details?}}` | Mismos códigos que REST. JSON inválido o tipo desconocido no cierran la conexión. |
| `server.reconnect` | S→C | `{reason:"rotate"\|"shutdown", retryAfterMs, lastVersion}` | Seguido de cierre 1012. |
| `access.revoked` | S→C | `{reason:"left"\|"kicked"\|"token-expired"\|"room-closed"}` | Seguido de cierre 1008: volver al flujo de ingreso. |

`reason` de snapshot (ayuda de UI, no fuente de verdad): `room.created`, `member.joined`, `member.left`,
`member.kicked`, `member.ready`, `members.readyReset`, `room.activated`, `game.started`, `game.ended`,
`round.started`, `round.playing`, `question.submitted`, `question.changed`, `questions.exhausted`,
`voting.started`, `voting.progress`, `voting.resolved`, `votes.cleared`, `game.finished`,
`presence.changed`, `sync` (detección por sondeo o petición explícita).

## Códigos de cierre

| Código | Significado | Cliente |
|---|---|---|
| 1000 | Normal | — |
| 1008 | Política: auth inválida, revocado | No reconectar con ese token |
| 1009 | Frame demasiado grande | Corregir cliente |
| 1011 | Error interno | Reconectar con backoff |
| 1012 | Rotación / apagado | Reconectar ya |
| 1013 | Inténtalo más tarde (DB) | Backoff |
| 4000 | Cliente lento (cola llena) | Reconectar y resincronizar |
| 4001 | Sin heartbeat | Reconectar |
| 4002 | Auth no recibida a tiempo | Reconectar enviando `auth` primero |

## Garantías y límites (sin brokers ni Redis)

- PostgreSQL es la única autoridad. Cada instancia mantiene un hub **local**; tras un commit local avisa
  a sus sockets de inmediato; un **watcher por proceso** (solo mientras haya sockets) consulta en lote
  `state_version`/deadlines/presencia cada 2 s (juego) / 5 s (lobby) ±20 % y empuja snapshots.
- Entre instancias la entrega es **eventual**: medido en local con dos instancias y sondeo de 150–200 ms,
  ~210–230 ms. **No hay entrega instantánea ni garantizada de cada evento**; un aviso perdido se repara con
  el siguiente snapshot. No reproducir animaciones a partir de `reason`: comparar estado e IDs (`resultId`).
- Snapshots coalescidos por conexión (solo el más reciente pendiente). Cola de control acotada (32).
- La expulsión/revocación se detecta en el siguiente ciclo del watcher de la instancia que tiene el socket.
- `LISTEN/NOTIFY` no se usa: el endpoint pooled de Neon no lo admite (plan 7.6).

## Recomendaciones para el cliente

- Aplicar solo snapshots con `epoch` correcto; ignorar `stateVersion` menor; deduplicar igual (`eventId`).
- Reconexión: backoff con jitter 0.5/1/2/4/8 s (tope 10 s durante la gracia de 20 s, luego 30 s). Tras
  reconectar no asumir cursor: siempre llega snapshot completo.
- Un ACK perdido se recupera reenviando **el mismo** `command`/`commandId`.
- Varias pestañas = varios leases (máx. 4 por jugador); cerrar una no saca al jugador.
- Salida silenciosa (sin cerrar): hasta 45 s de lease + ciclo del watcher + 20 s de gracia.
- Fallback sin WS: N6 cada 2–5 s + leases N13–N15; es una degradación, no equivalente a WS.
