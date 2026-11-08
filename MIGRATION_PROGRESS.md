# BBtronic — Registro de avance de la migración a Go

Rama: `migration/go-backend` (repositorio local nuevo en `D:\Proyectos\be-bbtronic-go`).
Backend Spring Boot original (`D:\Proyectos\be-bbtronic`, rev. `f5c0daa`) sin modificar.

## Estado actual (2026-11-08)

**Fase en curso: 1 — Validación Go + coder/websocket en Vercel Preview (prioridad pedida por el usuario).**
La migración completa está en pausa hasta tener evidencia desplegada.

| Fase | Estado |
|---|---|
| 0 Preparación/contratos | Plan leído completo; código Java auditado (controllers, 6 servicios, listener WS, seeders, modelos). SHA coincide con el plan (`f5c0daa`; origin/master solo añade un test trivial). |
| 1 Vercel WS | Spike implementado y **probado localmente 12/12**. **Pendiente: despliegue Preview** (CLI 65.12.1 disponible vía npx; sin sesión iniciada). |
| 2–9 | Iniciado `backend-go/internal/game` (modelo, motor, Directas). No compila aún (archivos de juego pendientes). En pausa. |

## Evidencia documental Vercel (leída 2026-11-08)

- Blog *Vercel Services* (2026-06-30): "persistent WebSocket connections in every Vercel runtime, whether … Node.js, Python, or Go".
- Changelog Go zero-config (2026-04-02): "Backends on Vercel use Fluid compute … by default".
- Docs Fluid compute (upd. 2026-08-24): lista Node.js, Python, Edge, Bun, Rust — **no Go**.
- Docs WebSockets (upd. 2026-08-10): requieren Fluid; no mencionan Go. Docs Go runtime (upd. 2026-08-11): Beta; preset `go`; entrypoints `main.go`, `cmd/api/main.go`, `cmd/server/main.go`; escuchar `PORT`.
- Duración Hobby: 300 s default/máx. (aplicación efectiva a Go **no documentada**).
- Conclusión: contradicción documental → **ni compatible ni incompatible hasta medir**.

## Archivos creados

- `spike/vercel-ws/` — servidor, probe, `vercel.json`, `README.md` con comandos de despliegue.
- `backend-go/` — módulo Go (pgx v5.11.0, coder/websocket v1.8.15), `internal/game/{model,errors,engine,direct}.go` (en progreso).

## Entorno de pruebas

- PostgreSQL 17.2 aislado (initdb) en `127.0.0.1:55432`, usuario `bbtronic_test`, datos en el scratchpad de la sesión. No se usó ninguna credencial del repositorio Java.
- Sin gcc: `go test -race` probablemente no ejecutable en esta máquina (a confirmar).

## Bloqueos

1. Sesión de Vercel del usuario (`npx vercel login`) y decisión sobre protección de Preview (bypass o desactivarla en el proyecto de pruebas).

## Próximas tareas

1. Desplegar spike en Preview, ejecutar `wsprobe` y `-hold 16m`, registrar resultados/logs aquí.
2. Si aprueba: continuar fases 2–9 (motores restantes, store pgx, coordinador, REST v1, WS v1, tests).
3. Si falla por limitación confirmada: detener partes dependientes de WS e informar antes de cambiar tecnología.
