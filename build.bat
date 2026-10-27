@echo off
REM Build script: build the frontend once, then produce two binaries:
REM   dist\ai_proxy-server.exe   console / server form (unchanged behaviour)
REM   dist\ai_proxy-desktop.exe  native window desktop app (GUI subsystem)
REM
REM NOTE: this file is deliberately ASCII-only. cmd.exe reads .bat files using the
REM console OEM code page (936/GBK on Chinese Windows) and does NOT understand
REM UTF-8. Non-ASCII characters here get mis-decoded, and some of the resulting
REM bytes act as command separators, so lines get split into bogus commands.
REM Keep this file ASCII-only.
REM
REM External commands are called by absolute path: when this script is launched
REM from Git Bash, PATH puts MSYS's bin first, and names like "find" resolve to
REM the GNU tool instead of the Windows one.

setlocal

cd /d "%~dp0"

set "SYS=%SystemRoot%\System32"
set "VER=0.2.0"

echo [1/4] Checking toolchain...
"%SYS%\where.exe" npm >nul 2>nul || (echo   ERROR: "npm" not found in PATH. & goto :fail)

REM Use a specific Go from the version directory rather than PATH's "go". The
REM PATH shim (g keeps one at ...\g\go) points at whichever version was last
REM selected -- 1.19 on this machine -- and the machine-wide GOROOT points at
REM the same old tree, so a bare "go build" compiles against the wrong standard
REM library. Both are overridden here; setlocal drops them when the script ends.
set "GO_HOME=D:\Program Files\g\versions\1.25.4"
if not exist "%GO_HOME%\bin\go.exe" set "GO_HOME="
if not defined GO_HOME (
  REM Not there any more: take the newest version g has installed.
  for /f "delims=" %%d in ('dir /b /ad /o-n "D:\Program Files\g\versions" 2^>nul') do (
    if not defined GO_HOME set "GO_HOME=D:\Program Files\g\versions\%%d"
  )
)
if not defined GO_HOME (
  echo   ERROR: no Go found under "D:\Program Files\g\versions".
  goto :fail
)
set "GOROOT=%GO_HOME%"
set "GO=%GO_HOME%\bin\go.exe"
set "PATH=%GO_HOME%\bin;%PATH%"

REM This project needs Go 1.25+ (modernc.org/sqlite requires it).
set "GO_VER="
for /f "tokens=3" %%v in ('"%GO%" version') do set "GO_VER=%%v"
set "GO_MAJOR=0"
set "GO_MINOR=0"
for /f "tokens=1,2 delims=." %%a in ("%GO_VER:go=%") do (set "GO_MAJOR=%%a" & set "GO_MINOR=%%b")
echo   go %GO_VER%   (%GO_HOME%)
if %GO_MAJOR% LSS 2 if %GO_MINOR% LSS 25 (
  echo   ERROR: Go 1.25+ is required, found %GO_VER%.
  goto :fail
)
for /f %%v in ('node --version') do echo   node %%v

echo.
echo [2/4] Building frontend...
if not exist "web\node_modules" (
  echo   installing dependencies ^(first run, may take a while^)...
  pushd web
  call npm install --no-audit --no-fund || (popd & goto :fail)
  popd
)
pushd web
call npm run typecheck || (popd & goto :fail)
call npm run build || (popd & goto :fail)
popd

echo.
echo [3/4] Building Windows resources...
REM Pass the .go file explicitly instead of a package path: after the frontend
REM step cmd.exe may still be resolving relative package patterns from web\,
REM which makes "go run ./scripts/gen-icon" look under the toolchain's src tree.
"%GO%" run scripts\gen-icon\main.go || goto :fail
"%GO%" run scripts\gen-rc\main.go -version %VER% || goto :fail
set "WINDRES="
if exist "D:\TDM-GCC-64\bin\windres.exe" set "WINDRES=D:\TDM-GCC-64\bin\windres.exe"
if not defined WINDRES (
  for /f "delims=" %%w in ('"%SYS%\where.exe" windres 2^>nul') do (
    if not defined WINDRES set "WINDRES=%%w"
  )
)
if defined WINDRES (
  pushd build\windows
  REM -c 65001: app.rc holds UTF-8 Chinese text; without it windres reads the
  REM file as ANSI and the exe's FileDescription shows mojibake.
  "%WINDRES%" --target=pe-x86-64 -i app.rc -o ..\..\internal\winres\rsrc_windows_amd64.syso -O coff -c 65001 || (popd & goto :fail)
  popd
  echo   resources compiled
) else (
  echo   WARNING: windres not found, reusing existing .syso ^(icon/version may be stale^).
)

if not exist "dist" mkdir dist

echo.
echo [4/4] Building binaries...
"%GO%" build -trimpath -ldflags "-X ai_proxy/internal/version.Version=%VER%" -o dist\ai_proxy-server.exe . || goto :fail
"%GO%" build -tags "production desktop" -trimpath -ldflags "-H windowsgui -s -w -X ai_proxy/internal/version.Version=%VER%" -o dist\ai_proxy-desktop.exe . || goto :fail

echo.
echo Done:
echo   dist\ai_proxy-server.exe   ^(console / server form^)
echo   dist\ai_proxy-desktop.exe  ^(desktop window^)
echo.
echo Run server:  dist\ai_proxy-server.exe -data data
echo Run desktop: dist\ai_proxy-desktop.exe
endlocal
exit /b 0

:fail
echo.
echo Build failed.
endlocal
exit /b 1
