#!/bin/bash

# RocketMan Tunnel Service - Uninstallation Script for macOS

set -e

# Configuration
SERVICE_NAME="com.rocketman.tunnel"
INSTALL_DIR="/usr/local/bin"
LAUNCHD_DIR="/Library/LaunchDaemons"
PLIST_FILE="${LAUNCHD_DIR}/${SERVICE_NAME}.plist"
BINARY_NAME="rocketman-tunnel-service"
LOG_FILE="/var/log/${SERVICE_NAME}.log"
ERROR_LOG_FILE="/var/log/${SERVICE_NAME}.error.log"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Logging functions
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check if running as root
check_root() {
    if [ "$EUID" -ne 0 ]; then
        log_error "This script must be run with sudo"
        exit 1
    fi
}

# Stop and unload service
stop_service() {
    if [ -f "$PLIST_FILE" ]; then
        log_info "Stopping service..."
        
        if launchctl list | grep -q "$SERVICE_NAME"; then
            launchctl unload "$PLIST_FILE" 2>/dev/null || true
            sleep 1
            log_info "Service stopped"
        else
            log_info "Service is not running"
        fi
    else
        log_info "Service configuration not found"
    fi
}

# Remove files
remove_files() {
    log_info "Removing files..."
    
    # Remove plist
    if [ -f "$PLIST_FILE" ]; then
        rm -f "$PLIST_FILE"
        log_info "Removed: $PLIST_FILE"
    fi
    
    # Remove binary
    if [ -f "${INSTALL_DIR}/${BINARY_NAME}" ]; then
        rm -f "${INSTALL_DIR}/${BINARY_NAME}"
        log_info "Removed: ${INSTALL_DIR}/${BINARY_NAME}"
    fi
    
    # Ask about log files
    if [ -f "$LOG_FILE" ] || [ -f "$ERROR_LOG_FILE" ]; then
        read -p "Remove log files? [y/N] " -n 1 -r
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            rm -f "$LOG_FILE" "$ERROR_LOG_FILE"
            log_info "Log files removed"
        else
            log_info "Log files preserved at:"
            [ -f "$LOG_FILE" ] && log_info "  - $LOG_FILE"
            [ -f "$ERROR_LOG_FILE" ] && log_info "  - $ERROR_LOG_FILE"
        fi
    fi
}

# Verify uninstallation
verify_uninstall() {
    log_info "Verifying uninstallation..."
    
    local errors=0
    
    if launchctl list | grep -q "$SERVICE_NAME"; then
        log_error "Service is still loaded in launchctl"
        errors=$((errors + 1))
    fi
    
    if [ -f "$PLIST_FILE" ]; then
        log_error "Plist file still exists: $PLIST_FILE"
        errors=$((errors + 1))
    fi
    
    if [ -f "${INSTALL_DIR}/${BINARY_NAME}" ]; then
        log_error "Binary still exists: ${INSTALL_DIR}/${BINARY_NAME}"
        errors=$((errors + 1))
    fi
    
    if [ $errors -eq 0 ]; then
        log_info "Uninstallation verified successfully"
        return 0
    else
        log_warn "Uninstallation completed with $errors warnings"
        return 1
    fi
}

# Main uninstallation process
main() {
    log_info "RocketMan Tunnel Service - Uninstallation"
    log_info "=========================================="
    
    check_root
    
    # Confirm uninstallation
    read -p "Are you sure you want to uninstall RocketMan Tunnel Service? [y/N] " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        log_info "Uninstallation cancelled"
        exit 0
    fi
    
    stop_service
    remove_files
    verify_uninstall
    
    echo ""
    log_info "Uninstallation completed!"
}

main "$@"