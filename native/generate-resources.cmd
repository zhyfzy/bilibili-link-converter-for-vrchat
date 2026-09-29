@echo off
setlocal
go run github.com/tc-hib/go-winres@v0.3.3 make --in winres\winres.json --arch amd64 --out rsrc
if errorlevel 1 exit /b %errorlevel%
echo.
echo Resource: %CD%\rsrc_windows_amd64.syso
pause
