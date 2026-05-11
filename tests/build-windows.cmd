@echo off
REM Build script for Windows version on Windows

if not exist "build\windows" mkdir build\windows
go build -o .\build\windows\rocketman-tunnel-service.exe .\src\windows
