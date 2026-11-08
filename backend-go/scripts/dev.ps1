<#
.SYNOPSIS
  Atajos de desarrollo local del backend Go de BBtronic.

.DESCRIPTION
  Uso (desde cualquier carpeta):
    .\backend-go\scripts\dev.ps1 check     # revisa la BD sin modificar nada
    .\backend-go\scripts\dev.ps1 setup     # aplica la migracion y las semillas (pide confirmacion)
    .\backend-go\scripts\dev.ps1 run       # arranca el servidor en http://localhost:8081
    .\backend-go\scripts\dev.ps1 smoke     # prueba completa contra el servidor local (en otra terminal)
    .\backend-go\scripts\dev.ps1 test      # tests unitarios

  La conexion se lee de backend-go\.env (DATABASE_URL). Las variables ya
  definidas en la sesion tienen prioridad sobre el archivo.
#>
param(
    [Parameter(Position = 0)]
    [ValidateSet('check', 'setup', 'run', 'smoke', 'test')]
    [string]$Command = 'check'
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot   # carpeta backend-go
Set-Location $root

function Assert-DatabaseUrl {
    $envFile = Join-Path $root '.env'
    $value = $env:DATABASE_URL
    if (-not $value -and (Test-Path $envFile)) {
        $line = Get-Content $envFile | Where-Object { $_ -match '^\s*DATABASE_URL\s*=' } | Select-Object -First 1
        if ($line) { $value = ($line -split '=', 2)[1].Trim().Trim('"').Trim("'") }
    }
    if (-not $value -or $value -like '*PEGA_AQUI*' -or $value -like '*USER:PASSWORD@HOST*') {
        Write-Host "Falta la conexion: edita $envFile y pega tu cadena de Neon en DATABASE_URL." -ForegroundColor Yellow
        exit 1
    }
}

switch ($Command) {
    'check' {
        Assert-DatabaseUrl
        go run ./cmd/admin check-db
    }
    'setup' {
        Assert-DatabaseUrl
        go run ./cmd/admin check-db
        Write-Host ''
        Write-Host 'Se aplicara la migracion 0001 (solo agrega columnas, una tabla de recibos e indices;' -ForegroundColor Cyan
        Write-Host 'no borra ni cambia datos ni columnas existentes) y las semillas si faltan.' -ForegroundColor Cyan
        $answer = Read-Host 'Escribe SI para continuar'
        if ($answer -ne 'SI') { Write-Host 'Cancelado.'; exit 1 }
        go run ./cmd/admin migrate -confirm
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
        go run ./cmd/admin seed-catalogs -confirm
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
        go run ./cmd/admin check-db
    }
    'run' {
        Assert-DatabaseUrl
        Write-Host 'Servidor en http://localhost:8081  (Ctrl+C para detener)' -ForegroundColor Green
        go run ./cmd/server
    }
    'smoke' {
        go run ./cmd/smoke -base http://localhost:8081 -origin http://localhost:5173 -full
    }
    'test' {
        go test ./...
    }
}
exit $LASTEXITCODE
