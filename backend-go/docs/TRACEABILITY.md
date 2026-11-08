# Matriz de equivalencia funcional — 37 endpoints Java → backend Go

Referencia Java: `be-bbtronic` rev. `f5c0daa` (`UserController` = U `/api/users`, `GameSessionController` =
S `/api/game-sessions`, R = `/api/game-sessions/{sessionCode}`). Tests: `go test ./...` (unitarios, `internal/game`, …)
y `go test -tags integration ./...` (PostgreSQL real: `internal/coordinator`, `internal/e2e`).

Leyenda de estado: **Impl** implementado · **Test-U** probado con test unitario · **Test-PG** probado contra
PostgreSQL real · **E2E** probado vía HTTP/WS real (httptest) · **Preview** validado en Vercel Preview · **Bloqueado**.

| ID | Endpoint Java | Funcionalidad | Mecanismo Go | Cambio deliberado | Tests | Estado |
|---|---|---|---|---|---|---|
| F01 | GET U/all | Inventario de usuarios | `cmd/admin list-players` (A1) | Solo operador, sin tokens (C2) | Ejecutado manualmente contra BD demo | Impl, verificado manual |
| F02 | POST U/register | Registro, nombre único | N1 `POST /players` | Token 256 bits, solo hash SHA-256; validación uniforme 1–32 chars | `TestRegister`, `TestRegister_OrphanAndLegacyNames`, `TestREST_Contract`, `TestRegisterValidation` | Test-PG, E2E |
| F03 | GET U/online | Usuarios "en línea" | `players[].connectionStatus` (N6) + `admin list-players -online` | Presencia real por leases (Java listaba todos) | `TestPresence_LeasesAndGrace`, `TestMember_PresenceLifecycle`, `TestWS_PresenceGrace` | Test-PG, E2E |
| F04 | DELETE U/logout | Salida y borrado | N3 `DELETE /players/me` | Bearer; reconcilia juegos; idempotente | `TestLogout`, `TestImpostor_LogoutDuringVote`, `TestREST_Contract` | Test-PG, E2E |
| F05 | POST U/{username}/ready | Listo propio + conteos | `member.ready` + `readiness` | Identidad por token (sin suplantación) | `TestApply_ReadyAndReset`, `TestReadyKick`, `TestWS_CrossInstanceConvergence` | Test-PG, E2E |
| F06 | POST S/create | Registro+sala+anfitrión | N1 → N4 `POST /rooms` | Registro separado; código único activo (índice parcial) con reintento; idempotente | `TestCreateAndJoin`, `TestREST_Contract` | Test-PG, E2E |
| F07 | GET R | Ver sala | N6 snapshot | Sin tokens/datos privados; solo miembros | `TestState_PrivacyAndAuthorization`, `TestREST_Contract` | Test-PG, E2E |
| F08 | POST S/join | Unirse | N1 → N5 | Una sala por jugador; espectador en Impostor tardío | `TestCreateAndJoin`, `TestImpostor_EndToEnd` | Test-PG, E2E |
| F09 | POST R/kick | Expulsar | `member.kick` | Solo anfitrión; reconciliación C11; revoca socket | `TestApply_Kick`, `TestReadyKick`, `TestWS_RevocationAcrossInstances` | Test-PG, E2E |
| F10 | POST S/reset | `CALL reset_game_data` | `room.reset` → `501 RESET_NOT_CONFIGURED` | **No se inventa la rutina** | `TestRoomResetBlocked`, `TestApply_Permissions` | **Bloqueado** (falta definición) |
| F11 | GET R/sync | Recuperación | N6 + WS `state.request`/`room.snapshot` | PG autoritativo; snapshot válido sin pregunta | `TestDirectas_CheckReadyFlow` (otra instancia), `TestWS_Protocol`, `TestWS_CrossInstanceConvergence` | Test-PG, E2E |
| F12 | POST R/start-game | `gameStarted=true` | `room.activate` | Host + versión; idempotente | `TestApply_ActivateIsIdempotent`, `TestIdempotency` | Test-PG |
| F13 | POST R/end-game | Volver a lobby | `game.end` | Conserva miembros, preguntas, historial y último resultado | `TestApply_GameEnd`, `TestDirectas_CheckReadyFlow` | Test-PG |
| F14 | GET R/round-info | Progreso | `game.progress` + `readiness` | Campos normalizados | `TestDirect_CheckReady`, `TestApply_ReadyAndReset` | Test-U |
| F15 | POST R/check-all-ready | Iniciar reproducción | `round.checkReady` | Plantilla `{usuarios}`; `409 NO_QUESTIONS`; primera pregunta por ID | `TestDirect_CheckReady`, `TestDirect_CheckReadyDefaultMessage`, `TestDirectas_CheckReadyFlow` | Test-PG |
| F16 | POST R/reset-users-ready | Reset ready | `members.resetReady` | Host | `TestApply_ReadyAndReset`, `TestReadyKick` | Test-PG |
| F17 | POST R/start-preguntas-directas | Iniciar Directas | `game.start {game:preguntas-directas}` | Estado limpio (C14) | `TestDirect_Start`, `TestConcurrentControlCommands` | Test-PG |
| F18 | POST R/start-new-round | Nueva ronda | `round.new` | Host/versión; preguntas históricas conservadas | `TestDirect_NewRound`, `TestDirectas_CheckReadyFlow` | Test-PG |
| F19 | POST R/send-question | Aportar pregunta | `question.submit` | Autor real del token; destinatario miembro; texto validado | `TestDirect_Submit`, `TestDirectas_PersistenceAndRestart` | Test-PG |
| F20 | GET R/current-question | Pregunta actual | `game.currentQuestion` | Lectura pura; ID real (C3); anonimato como proyección | `TestDirect_CheckReady`, `TestDirect_NextAvoidsRecipientAndCompletes`, `TestDirectas_CheckReadyFlow` | Test-PG |
| F21 | POST R/next-random-question | Siguiente | `question.next` | Destinatario anterior desde PG; COMPLETED al agotar | `TestDirect_NextAvoidsRecipientAndCompletes`, `TestConcurrentControlCommands` | Test-PG |
| F22 | POST R/yo-nunca-nunca/start | Iniciar YNN | `game.start` | Orden barajado materializado en PG | `TestCatalog_YoNuncaNuncaAndCultura`, `TestCatalogs_PersistedOrder` | Test-PG |
| F23 | GET R/next-yo-nunca-nunca | Consumir por tipo | `question.next {type}` | POST; tipo agotado ≠ catálogo agotado | `TestCatalog_YoNuncaNuncaAndCultura`, `TestCatalogs_PersistedOrder` | Test-PG |
| F24 | POST R/cultura-pendeja/start | Iniciar Cultura | `game.start` | Host | `TestCatalog_YoNuncaNuncaAndCultura`, `TestCatalog_RestartClearsPreviousGame` | Test-U |
| F25 | GET R/next-cultura-pendeja | Consumir Cultura | `question.next {type}` | POST | `TestCatalog_YoNuncaNuncaAndCultura`, `TestCatalog_SkipsDeletedRows` | Test-U |
| F26 | POST R/quien-es-mas-probable/start | Iniciar Probable | `game.start` | Votos limpios | `TestProbable_VotingCompletesWithWinner` | Test-PG |
| F27 | GET R/next-quien-es-mas-probable | Pregunta + `{player}` | `question.next {type}` | No borra votos si falla la selección | `TestProbable_TypeExhaustedKeepsVotes`, `TestProbable_ConcurrentVotes` | Test-PG |
| F28 | POST R/vote | Votar | `vote.submit {questionInstanceId,targetPlayerId}` | Votante = token; candidato miembro | `TestProbable_ConcurrentVotes`, `TestProbable_VoteValidation` | Test-PG |
| F29 | GET R/vote-results | Ganador | `voting.counts/leaderPlayerId/winnerPlayerId`, `lastVotingResult` | Desempate estable menor ID (C10) | `TestProbableTally_Tiebreak`, `TestProbable_ConcurrentVotes` | Test-PG |
| F30 | GET R/check-all-voted | Todos votaron | `voting.complete`, `pendingPlayerIds` | Electorado explícito, congelado al completar (C11) | `TestProbable_MembershipChanges` | Test-U |
| F31 | POST R/clear-votes | Limpiar votos | `votes.clear` | Host/ámbito | `TestProbable_ClearVotes`, `TestProbable_ConcurrentVotes` | Test-PG |
| F32 | POST R/start-preguntas-incomodas | Iniciar Incómodas | `game.start` | ready=false | `TestUncomfortable_BagWithoutRepetition` | Test-U |
| F33 | GET R/next-preguntas-incomodas | Pregunta + destinatario | `question.next {type}` | Bolsa reconciliada con altas/bajas (C11) | `TestUncomfortable_BagWithoutRepetition`, `TestUncomfortable_BagReconcilesMembership`, `TestUncomfortable_Cycle` | Test-PG |
| F34 | POST R/el-impostor/start | Iniciar Impostor | `game.start {game:el-impostor, impostorCount}` | Countdown persistido; secretos privados | `TestImpostor_StartValidation`, `TestImpostor_WordHistory`, `TestImpostor_EndToEnd` | Test-PG |
| F35 | GET R/el-impostor/my-role | Rol propio | N8 | Solo titular participante (C5) | `TestImpostor_RolesArePrivate`, `TestImpostor_EndToEnd` | Test-PG |
| F36 | POST R/el-impostor/call-vote | Convocar | `vote.call` | Deadline persistido; 120/60 s | `TestImpostor_Cooldowns`, `TestImpostor_EndToEnd` | Test-PG |
| F37 | POST R/el-impostor/vote | Votar/eliminar/ganar | `vote.submit {votingRoundId,targetPlayerId}` | Timeout resuelto una vez; correcciones C12 | `TestImpostor_VotingAndVictory`, `TestImpostor_DepartureDuringVoting`, `TestImpostor_NewRunInvalidatesOldDeadline`, `TestImpostor_EndToEnd` | Test-PG |

## Diferencias intencionales (C1–C14)

| ID | Decisión | Dónde | Evidencia |
|---|---|---|---|
| C1 | Sin borrado/seed/DDL en arranque; seeds y migraciones solo por `cmd/admin` con `-confirm` | `cmd/server`, `cmd/admin` | `TestLegacyRowsAndExpiry`; ejecución manual de `admin` |
| C2 | Identidad por token; sin tokens ajenos; inventario fuera de la API | `auth`, `game/view.go` | `TestState_PrivacyAndAuthorization`, `TestREST_Contract` |
| C3 | ID real de la pregunta actual; GET no consume; anonimato como proyección | `game/direct.go` | `TestDirect_NextAvoidsRecipientAndCompletes` |
| C4 | Snapshot válido sin pregunta; presencia real por leases | `game/view.go`, `game/presence.go` | `TestPresence_LeasesAndGrace` |
| C5 | Rol solo titular; sin `usedWords` públicos | `game/impostor.go`, `view.go` | `TestImpostor_RolesArePrivate` |
| C6 | Transacciones + `FOR UPDATE` + versión + recibos; deadlines persistidos; resultado recuperable | `coordinator`, `store` | `TestIdempotency`, `TestConcurrentControlCommands`, `TestImpostor_EndToEnd` |
| C7 | REST v1 + WS JSON v1; rutas antiguas y STOMP retiradas | `httpapi`, `realtime` | `TestUnknownRouteAndReadyzDisabled`; smoke Preview |
| C8 | Selecciones/inicios son POST; GET puro; reconciliación explícita (N9) | `httpapi` | `TestImpostor_EndToEnd` (`reconciliationRequired`) |
| C9 | Código único entre salas activas con reintento; validación uniforme; errores saneados | `store/rooms.go`, `apierror` | `TestCreateAndJoin`, `TestClassify` |
| C10 | Empate Probable → menor ID | `game/probable.go` | `TestProbableTally_Tiebreak` |
| C11 | Bolsas y electorado reconciliados; espectador en Impostor tardío | `game/catalog.go`, `probable.go`, `view.go` | `TestUncomfortable_BagReconcilesMembership`, `TestProbable_MembershipChanges` |
| C12 | Votos de/para retirados invalidados; voting continúa; run nuevo invalida deadlines viejos; salida no reinicia cooldown global | `game/impostor.go` | `TestImpostor_DepartureDuringVoting`, `TestImpostor_NewRunInvalidatesOldDeadline` |
| C13 | Gracia 20 s desde la detección; salida silenciosa por lease (≤45 s + ciclo + 20 s) | `game/presence.go`, `realtime` | `TestWS_PresenceGrace`, `TestPresence_LeasesAndGrace` |
| C14 | Start limpia la partida anterior; conserva historial privado de palabras | `game/*` | `TestCatalog_RestartClearsPreviousGame`, `TestImpostor_WordHistory` |

Reglas Java preservadas explícitamente: mínimo 2 jugadores en Impostor, `1 ≤ k < N`, autovoto, cooldowns reales
120 s/60 s, votación 60 s, countdown 3 s, empate/cero votos sin eliminado, victoria civil/impostora, historial de
palabras reiniciado al agotarse, 150 palabras y 3 plantillas de espera, filtro de tipo case-insensitive,
`{player}` reemplazado en todas sus apariciones, Incómodas siempre elige destinatario, anfitrión sin sucesión.

## Eventos STOMP Java (plan 4.7) → protocolo v1

Todos los eventos `/topic/{code}` se sustituyen por `room.snapshot` (estado completo) con `reason` y por
`access.revoked`/`server.reconnect`. Correspondencia principal: `userUpdate`/`userLeft`/`creatorLeft` →
`member.joined`/`member.left` + `players`; `kicked` → `member.kicked` + `access.revoked{kicked}`;
`userReconnecting` → `presence.changed` (`connectionStatus:"reconnecting"`); `gameStarted` → `room.activated`;
`*Started` → `game.started`; `newRoundStarted` → `round.started`; `allReady` → `round.playing`;
`allQuestionsShown` → `questions.exhausted`; `nextQuestion` → `question.changed`; `voteCompleted` / `votingResult`
→ `voting.resolved` + `lastVotingResult`; `voteUpdate` → `voting.progress`; `votingStarted` → `voting.started`;
`votingTimerExpired` → `voting.resolved` con `reason:"timeout"`; `impostorCountdown`/`impostorGameStarted` →
`countdownEndsAt`; `impostorRoundContinues` → fase `description`; `impostorPlayerEliminated` → `lastDeparture`;
`impostorGameOver` → `game.finished` + `lastGameResult`.
