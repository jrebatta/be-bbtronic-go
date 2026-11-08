@echo off
rem ===========================================================================
rem  BBtronic - arranca el backend Go en local con un solo comando.
rem    Uso:  start-backend            (o doble clic en este archivo)
rem          start-backend --skip-check   (no verifica la BD antes de arrancar)
rem  Lee la configuracion de backend-go\.env  (DATABASE_URL, PORT=8081, ...).
rem ===========================================================================
setlocal
chcp 65001 >nul
cd /d "%~dp0backend-go"

where go >nul 2>nul
if errorlevel 1 (
  echo [ERROR] No se encontro Go. Instalalo desde https://go.dev/dl/ y vuelve a intentar.
  goto :fail
)

if not exist ".env" (
  echo [ERROR] Falta backend-go\.env. Copia .env.example a .env y pon tu DATABASE_URL.
  goto :fail
)
if not defined DATABASE_URL (
  findstr /c:"PEGA_AQUI" ".env" >nul && (
    echo [ERROR] Edita backend-go\.env y pega tu cadena de Neon en DATABASE_URL.
    goto :fail
  )
)

echo Compilando el backend...
go build -o bin\bbtronic-server.exe .\cmd\server || goto :fail
go build -o bin\bbtronic-admin.exe .\cmd\admin || goto :fail

if /i not "%~1"=="--skip-check" (
  echo Verificando la base de datos...
  bin\bbtronic-admin.exe check-db
  if errorlevel 1 (
    echo.
    echo [ERROR] La base de datos no esta lista. Si falta la migracion ejecuta una vez:
    echo         powershell -ExecutionPolicy Bypass -File backend-go\scripts\dev.ps1 setup
    goto :fail
  )
)

echo.
echo Backend en http://localhost:8081   (healthz: http://localhost:8081/healthz)
echo Pulsa Ctrl+C para detenerlo.
echo.
bin\bbtronic-server.exe
exit /b %errorlevel%

:fail
echo.
pause
exit /b 1
