# Spike: Go + coder/websocket en Vercel Hobby (fase 1 del plan)

Objetivo: obtener evidencia **desplegada** de si el runtime/preset Go oficial de Vercel
admite WebSockets con `github.com/coder/websocket`. No usa base de datos ni datos reales.

| Archivo | Rol |
|---|---|
| `cmd/server/main.go` | Servidor `net/http` (entrypoint detectado por el preset Go). `GET /healthz`, `GET /ws` (eco JSON, `info`, `broadcast:<texto>`). Escucha `PORT`. |
| `cmd/wsprobe/main.go` | Cliente de validación: health, 101, eco Unicode, secuencia, ping/pong, cierre, 2 clientes simultáneos, broadcast local, reconexión ×N, Origin rechazado/permitido, límite de frame 16 KiB y sesión prolongada (`-hold`). |
| `vercel.json` | `{"framework": "go"}` — preset Go oficial. |
| `go.mod` | Go 1.25.0 (Vercel lee la directiva `go`), `coder/websocket v1.8.15` fijado. |

## Resultado local (2026-11-08) — NO es evidencia de Vercel

`go run ./cmd/wsprobe -base http://localhost:3999 -allowed-origin http://localhost:5173` → 12/12 PASS
(101, eco Unicode, ping, cierre 1000, 2 clientes, broadcast, 3 reconexiones, Origin 403, frame 20 KiB → 1009).

## Resultado en Vercel Preview (2026-11-08) — EVIDENCIA DESPLEGADA

Proyecto de pruebas `bbtronic-ws-spike` (cuenta Hobby del usuario), Deployment Protection activa + bypass
de automatización. Preview `bbtronic-ws-spike-9769ttcww-…vercel.app` (`vercelEnv=preview`, región `iad1`,
`goVersion=go1.25.0`, runtime de logs `executable`). Control sin bypass: `302` (protección efectiva).

| Prueba | Resultado |
|---|---|
| `GET /healthz` | 200, ~400–600 ms en frío desde `gru1` |
| Handshake WS | **101** `Upgrade: websocket` |
| Eco Unicode, 10 mensajes en orden, ping/pong | PASS, RTT ~195 ms (cliente en Perú → `iad1`) |
| Cierre normal 1000 | PASS (logs del servidor: `status=1000`) |
| 2 clientes simultáneos | PASS, misma instancia (`6b7d7fb10897`), broadcast local entregado |
| 4 reconexiones | PASS |
| Origin ajeno | 403 |
| Frame 20 KiB | cierre 1009 |
| Sesión de 16 min (`-hold 16m`) | **3 cortes de la plataforma a 5 min 10 s** (cliente) / **300 000 ms** (servidor: EOF, sin frame de cierre, sin señal); reconexión correcta tras cada corte; el proceso siguió vivo (misma instancia) y ejecutó su limpieza |

Conclusión: **Go + coder/websocket funciona en Vercel Hobby (Preview)** con un máximo efectivo de 300 s por
conexión. Diseño resultante: rotación del servidor a 270 s (`server.reconnect` + 1012) y recuperación por
snapshot. No demostrado: reparto real entre varias instancias (todas las conexiones cayeron en una) ni el
comportamiento durante un redeploy con sockets abiertos.

El backend completo (`backend-go`) se desplegó también como Preview del mismo proyecto y pasó `cmd/smoke`
(12/12: health, cabeceras, CORS, Origin, errores, upgrade 101 a través del middleware real, cierres 1008, y
degradación `DATABASE_UNAVAILABLE`/1013 al no tener BD alcanzable).

## Despliegue Preview (requiere tu sesión; no compartas credenciales)

```powershell
# 1. Autenticación (desde tu terminal o con "! " en Claude Code)
npx vercel login

# 2. Crear/vincular un proyecto de pruebas aislado (no toca producción)
cd D:\Proyectos\be-bbtronic-go\spike\vercel-ws
npx vercel link --yes --project bbtronic-ws-spike

# 3. Desplegar como PREVIEW (sin --prod)
npx vercel deploy --yes
#   -> imprime https://bbtronic-ws-spike-<hash>-<team>.vercel.app
```

### Protección de despliegues Preview

En Hobby los Preview suelen tener *Vercel Authentication* activa: el probe recibiría 401/redirección
(falso negativo). Una de estas dos opciones, solo en el proyecto `bbtronic-ws-spike`:

- **A (recomendada):** Settings → Deployment Protection → *Protection Bypass for Automation* → generar secreto.
  Exportarlo en tu shell: `$env:VERCEL_AUTOMATION_BYPASS_SECRET = "<secreto>"` (el probe lo envía como header
  `x-vercel-protection-bypass`, nunca lo imprime).
- **B:** desactivar *Vercel Authentication* en ese proyecto de pruebas (no contiene datos).

### Validación

```powershell
$base = "https://<url-del-preview>"
go run ./cmd/wsprobe -base $base                 # comprobaciones funcionales
go run ./cmd/wsprobe -base $base -hold 16m       # duración efectiva y >=3 cierres/reconexiones
npx vercel logs $base                            # logs: "ws open"/"ws closed"/durationMs
```

Opcional: `npx vercel env add ALLOWED_ORIGINS preview` con un origen de prueba y `-allowed-origin <origen>`.

## Criterios de decisión

- **Aprobado:** `ws.handshake` = 101 en el Preview, eco bidireccional, 2 clientes, ≥3 reconexiones, y
  duración efectiva medida con `-hold` (registrar el valor real, no asumir 300 s).
- **Limitación confirmada:** el upgrade falla de forma reproducible (p. ej. 400/426/500 sin 101) con
  logs que lo atribuyan a la plataforma, tras descartar protección de despliegue y Origin.
- Un 101 local, un 200 HTTP o un ejemplo Node funcionando no cuentan como evidencia.
