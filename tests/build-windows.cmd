@echo off
REM Build script for Windows version on Windows

if not exist "build\windows" mkdir build\windows
go build -o .\build\windows\win_service.exe .\src\windows
