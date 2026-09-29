@echo off
setlocal
set CGO_ENABLED=0
set GOOS=windows
set GOARCH=amd64
go build -trimpath -ldflags="-H=windowsgui -s -w" -o ..\dist\BilibiliUrl.exe .
if errorlevel 1 exit /b %errorlevel%
echo.
echo EXE: %CD%\..\dist\BilibiliUrl.exe
pause
