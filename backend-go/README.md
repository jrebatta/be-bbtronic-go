# BBtronic — backend Go

Migración del backend Spring Boot de BBtronic a Go: `net/http`, `pgx/v5` sobre el PostgreSQL existente
(Neon), WebSockets JSON nativos con `github.com/coder/websocket`, preparado para Vercel Hobby. Sin Redis,
Docker, brokers, ORM ni framework HTTP.

- Contrato REST v1: [docs/API.md](docs/API.md) · OpenAPI 3.1: [docs/swagger.yml](docs/swagger.yml)
- Protocolo WebSocket v1: [docs/WEBSOCKET.md](docs/WEBSOCKET.md)
- Equivalencia con los 37 endpoints Java: [docs/TRACEABILITY.md](docs/TRACEABILITY.md)
- Despliegue, migración y rollback: [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)
- Registro de avance: [../MIGRATION_PROGRESS.md](../MIGRATION_PROGRESS.md)

## Arquitectura

```text
cmd/server        entrypoint Vercel (preset go), PORT, apagado ordenado
cmd/admin         herramienta de operador: inspect-schema, migrate, seed-catalogs, list-players, purge-receipts
cmd/smoke         cliente de ensayo contra un despliegue
internal/game        dominio puro: salas, presencia, 6 motores, vista pública (reloj/RNG inyectables)
internal/coordinator transacción por operación: lock sala→usuarios, recibos idempotentes, versión, AdvanceDue
internal/store       pgx: SQL explícito, migraciones embebidas (aditivas), seeds Java, consultas admin
internal/httpapi     REST v1, middleware (requestId, logs sin secretos, CORS/Origin, límites, rate limit local)
internal/realtime    hub local + watcher de revisiones PG + protocolo WS v1
internal/auth        tokens 256 bits (solo SHA-256 en BD)
internal/apierror    errores públicos estables compartidos REST/WS
internal/config      entorno validado (sin imprimir valores)
internal/testdb      BD efímera por test (solo hosts locales)
```

Flujo de una mutación: autenticar → `BEGIN` → `SELECT … FOR UPDATE` sala y miembros → recibo idempotente →
`AdvanceDue` (deadlines/presencia) → `expectedVersion` → regla de juego → persistir + `state_version++` +
recibo → `COMMIT` → aviso al hub local. Otras instancias lo descubren por sondeo batch de revisiones.

## Ejecución local

Requisitos: Go ≥ 1.25, PostgreSQL ≥ 13 local (sin Docker).

```powershell
# BD local de desarrollo con el esquema Java inferido + migración Go
createdb bbtronic_dev
psql -d bbtronic_dev -f internal/testdb/baseline_hibernate_inferred.sql   # SOLO local: esquema inferido
$env:DATABASE_URL = "postgres://USER@127.0.0.1:5432/bbtronic_dev?sslmode=disable"
go run ./cmd/admin migrate -confirm
go run ./cmd/admin seed-catalogs -confirm
# (opcional) cargar preguntas de prueba en yo_nunca_nunca, cultura_pendeja, quien_es_mas_probable, preguntas_incomodas

$env:APP_ENV = "development"; $env:ALLOWED_ORIGINS = "http://localhost:5173"
go run ./cmd/server          # http://localhost:8080/healthz
go run ./cmd/smoke -base http://localhost:8080 -origin http://localhost:5173 -full
```

## Pruebas

```powershell
go build ./... ; go vet ./... ; go test ./...                     # unitarias (sin BD)

# Integración y E2E contra un PostgreSQL LOCAL (crea y borra una BD por test; rechaza hosts no locales)
$env:BBTRONIC_TEST_DATABASE_URL = "postgres://USER@127.0.0.1:5432/postgres?sslmode=disable"
go test -tags integration ./...

go test -run=^$ -fuzz=FuzzApply -fuzztime=30s ./internal/game/
go test -run=^$ -bench=. -benchmem ./internal/game/
go test -tags integration -run=^$ -bench=. -benchmem ./internal/coordinator/
```

`go test -race` necesita cgo con gcc/mingw; en Windows sin gcc se ejecutó en WSL2 (Linux), que es la
plataforma de Vercel. `BBTRONIC_TEST_DB_ALLOW_PRIVATE=1` permite un host RFC 1918 (p. ej. la IP del host vista
desde WSL) para los tests de integración.

## Seguridad (resumen)

Identidad solo por Bearer (hash SHA-256 en BD); permisos por sala/rol en el servidor; snapshot sin tokens, sin
palabra/roles del Impostor; SQL parametrizado (tablas de catálogo desde constantes); JSON estricto y límites de
tamaño; Origin exacto en REST y WS; errores saneados; logs sin cuerpos ni credenciales; ninguna operación
destructiva en el arranque; `reset_game_data` nunca se invoca.
