@echo off
REM Build script: build the frontend, then embed it into a single Go binary.
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

echo [1/3] Checking toolchain...
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

REM Stopping a running instance first is the safe move: on Windows the linker often
REM cannot replace the executable while a process has it open, and whether it
REM succeeds is not predictable. So remember whether it was running, and give a
REM precise hint if the build then fails.
set RUNNING=0
"%SYS%\tasklist.exe" /fi "imagename eq ai_proxy.exe" /nh 2>nul | "%SYS%\findstr.exe" /i "ai_proxy.exe" >nul
if not errorlevel 1 set RUNNING=1

echo.
echo [2/3] Building frontend...
if not exist "web\node_modules" (
  echo   installing dependencies ^(first run, may take a while^)...
  pushd web
  call npm install --no-audit --no-fund || (popd & goto :fail)
  popd
)
pushd web
call npm run build || (popd & goto :fail)
popd

echo.
echo [3/3] Building backend...
"%GO%" build -o ai_proxy.exe .
if errorlevel 1 (
  echo.
  if "%RUNNING%"=="1" (
    echo   ai_proxy.exe is running and Windows would not let the linker replace it.
    echo   Stop the program, then run this script again.
  ) else (
    echo   See the compiler output above for the cause.
  )
  goto :fail
)

if "%RUNNING%"=="1" (
  echo.
  echo   NOTE: the instance that was running still uses the OLD code. Restart it.
)

echo.
echo Done: ai_proxy.exe
echo Run:  ai_proxy.exe -data data
echo Open: http://127.0.0.1:8080/
endlocal
exit /b 0

:fail
echo.
echo Build failed.
endlocal
exit /b 1
