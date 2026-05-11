#!/bin/bash
# Build script for Windows version on Linux/macOS
# Cross-compiles to Windows executable

mkdir -p ./build/windows
GOOS=windows GOARCH=amd64 go build -o ./build/windows/rocketman-tunnel-service.exe ./src/windows
