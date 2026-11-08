# Despliegue en Vercel Hobby, migración de datos y rollback

## 1. Estado de validación

| Elemento | Estado | Evidencia |
|---|---|---|
| Runtime/preset Go (`framework: go`, `cmd/server/main.go`, `PORT`) | **Validado en Preview** | `runtime: "executable"`, `go1.25.0` (tomado de `go.mod`), región `iad1` |
| WebSocket `coder/websocket` (101, eco, ping, 2 clientes, reconexión, Origin, 1009) | **Validado en Preview** (spike) | `spike/vercel-ws/README.md` |
| Duración efectiva de una conexión WS en Hobby | **Medida: 300 s** (servidor), corte abrupto sin frame de cierre | 3 cortes consecutivos en sesión de 16 min |
| Backend completo (middleware, CORS, errores, upgrade, protocolo auth) | **Validado en Preview sin BD** | `cmd/smoke`: 12/12 |
| Backend con PostgreSQL desde Vercel (TLS `verify-full`, pooler Neon, prepared statements) | **Pendiente** | Requiere una rama/BD Neon de pruebas autorizada |
| Multi-instancia real en Vercel | **Pendiente** | Solo se observó una instancia; la convergencia multi-instancia está probada en local (2 servidores) |
| Despliegue a producción | **No realizado** | Requiere autorización explícita |

## 2. Configuración del proyecto Vercel

- **Root Directory: `backend-go`** (el preset exige `go.mod` en la raíz del proyecto).
- `vercel.json`: `{"framework": "go"}`. Fluid compute activo (por defecto en proyectos nuevos).
- Región: elegir la más cercana a la región de Neon (medir; no asumir la del usuario).
- Deployment Protection: mantener *Vercel Authentication* en Preview; para pruebas automáticas usar
  *Protection Bypass for Automation* (`x-vercel-protection-bypass`), nunca desactivarla en producción.

### Variables de entorno (ver `.env.example`)

| Variable | Producción / Preview |
|---|---|
| `APP_ENV` | `production` / `preview` |
| `DATABASE_URL` | DSN **pooled** de Neon con `sslmode=verify-full` (obligatorio fuera de dev). Credenciales **nuevas**: las del `application.properties` Java están expuestas y deben rotarse. |
| `ALLOWED_ORIGINS` | `https://fe-bbtronic-vue.vercel.app` (+ previews del frontend si aplica) |
| `TRUST_PROXY_HEADERS` | `true` en Vercel (IP de cliente para rate limiting) |
| `WS_ROTATE_SECONDS` | `270` (límite medido 300 s) |
| `PG_MAX_CONNS` / `PG_MIN_CONNS` | `2` / `0` (se multiplica por instancia) |
| `PG_QUERY_EXEC_MODE` | vacío (pgx `cache_statement`). Si el pooler rechaza prepared statements: `exec` o `simple_protocol` **con evidencia** |
| `OPS_HEALTH_TOKEN` | opcional, ≥32 chars, habilita `/readyz` |
| Resto | valores por defecto del plan (ver `internal/config/config.go`) |

`DATABASE_DIRECT_URL` solo para `cmd/admin` (no configurarlo en el proyecto web).

### Comandos

```powershell
cd backend-go
npx vercel login
npx vercel link --project <proyecto>
npx vercel deploy                      # Preview
go run ./cmd/smoke -base https://<preview>            # sin BD
go run ./cmd/smoke -base https://<preview> -full      # con BD de pruebas
npx vercel logs --deployment <url>
# Producción solo con autorización:  npx vercel deploy --prod
```

## 3. PostgreSQL (Neon): procedimiento seguro

1. **Backup verificado** (esquema + datos + rutinas) y restauración de prueba en destino separado.
2. Inspección read-only: `go run ./cmd/admin inspect-schema` (añadir `-show-routine` para ver `reset_game_data`;
   revisar antes de versionar). Comparar con `internal/testdb/baseline_hibernate_inferred.sql` (**inferido**).
3. Revisar `internal/store/migrations/0001_go_runtime.sql` contra el catálogo real (tipos, índices existentes,
   duplicados, tamaño). Todo es aditivo e idempotente; los `CREATE INDEX` no son `CONCURRENTLY`: en tablas grandes
   evaluarlo antes (bloquean escrituras brevemente).
4. Aplicar en una **rama Neon de pruebas** primero: `go run ./cmd/admin migrate -confirm`.
5. Catálogos: `go run ./cmd/admin seed-catalogs -confirm` (solo inserta si las tablas están vacías).
   Los cuatro catálogos de preguntas no tienen seeder en Java: deben existir ya en la BD.
6. Producción: `APP_ENV=production go run ./cmd/admin migrate -confirm -allow-production`.

Filas Java existentes (`epoch IS NULL`) **no se modifican** y el backend Go las ignora (salas legacy invisibles,
usuarios legacy bloquean su nombre). Mantenimiento: `admin list-players`, `admin purge-receipts -confirm`.

## 4. Corte (fase 9)

1. Preparar la versión Java de retorno con `clearOnStartup`, seeds y `ddl-auto` **desactivados** y secretos por
   entorno — sin eso, re-arrancar Java borra usuarios y salas.
2. Rotar las credenciales PostgreSQL expuestas en el repositorio Java.
3. Drenar partidas Java (el estado en memoria Java no es exportable); no ejecutar ambos motores sobre la misma sala.
4. Aplicar migración aditiva, desplegar Go validado, migrar el frontend al contrato v1 y validar E2E.
5. Observar errores/cuotas/latencia; mantener columnas y recibos durante la ventana de retorno.

## 5. Rollback

- **Go → Go anterior**: compatible mientras `state_schema_version` = 1 (lectores rechazan versiones mayores).
- **Go → Java**: drenar salas Go; Java preparado sin limpieza destructiva ignora las columnas nuevas. No hay
  continuidad de partidas activas entre motores.
- No hay migración "down": las columnas/tabla nuevas se conservan; nunca restaurar un backup encima de escrituras
  recientes sin reconciliación.

## 6. Capacidad Hobby (estimación, no benchmark)

Sala activa con sockets en una instancia: 1 consulta batch de revisiones cada 2 s (1.800/h) + renovaciones de
lease (1 por pestaña cada 15 s). Cada conexión WS dura ≤ 270 s: ~13 reconexiones/h por pestaña (invocaciones).
Comando medido en local: ~1,15 ms, 10 sentencias SQL; en Neon dominan los RTT de red. Medir en Preview con BD
antes de prometer un número de jugadores; reservar ≥30 % de las cuotas Hobby (uso personal/no comercial).
