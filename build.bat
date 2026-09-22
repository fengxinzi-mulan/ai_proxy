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
"%SYS%\where.exe" go >nul 2>nul || (echo   ERROR: "go" not found in PATH. & goto :fail)
"%SYS%\where.exe" npm >nul 2>nul || (echo   ERROR: "npm" not found in PATH. & goto :fail)

REM This project needs Go 1.25+ (modernc.org/sqlite requires it).
for /f "tokens=3" %%v in ('go version') do echo   go %%v
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
go build -o ai_proxy.exe .
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
