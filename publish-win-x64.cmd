@echo off
setlocal
set CGO_ENABLED=0
set GOOS=windows
set GOARCH=amd64
pushd native
go build -trimpath -ldflags="-H=windowsgui" -o ..\dist\BilibiliUrl.exe .
set BUILD_RESULT=%errorlevel%
popd
if not %BUILD_RESULT%==0 exit /b %BUILD_RESULT%
echo.
echo EXE: %CD%\dist\BilibiliUrl.exe
pause
