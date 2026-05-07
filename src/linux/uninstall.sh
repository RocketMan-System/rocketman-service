#!/bin/bash

# RocketMan Tunnel Service - Uninstallation Script for Linux

set -e

# Configuration
SERVICE_NAME="rocketman-tunnel"
INSTALL_DIR="/usr/local/bin"
SYSTEMD_DIR="/etc/systemd/system"
UNIT_FILE="${SYSTEMD_DIR}/${SERVICE_NAME}.service"
BINARY_NAME="rocketman-tunnel-service"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Check if running as root
check_root() {
    if [ "$EUID" -ne 0 ]; then
        log_error "This script must be run with sudo"
        exit 1
    fi
}

# Stop and disable the service
stop_service() {
    if systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
        log_info "Stopping service..."
        systemctl stop "$SERVICE_NAME"
        log_info "Service stopped"
    else
        log_info "Service is not running"
    fi

    if systemctl is-enabled --quiet "$SERVICE_NAME" 2>/dev/null; then
        log_info "Disabling service..."
        systemctl disable "$SERVICE_NAME"
        log_info "Service disabled"
    fi
}

# Remove installed files
remove_files() {
    log_info "Removing files..."

    if [ -f "$UNIT_FILE" ]; then
        rm -f "$UNIT_FILE"
        log_info "Removed: $UNIT_FILE"
    fi

    if [ -f "${INSTALL_DIR}/${BINARY_NAME}" ]; then
        rm -f "${INSTALL_DIR}/${BINARY_NAME}"
        log_info "Removed: ${INSTALL_DIR}/${BINARY_NAME}"
    fi

    log_info "Reloading systemd daemon..."
    systemctl daemon-reload
}

# Verify uninstallation
verify_uninstall() {
    log_info "Verifying uninstallation..."
    local errors=0

    if systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
        log_error "Service is still active"
        errors=$((errors + 1))
    fi

    if [ -f "$UNIT_FILE" ]; then
        log_error "Unit file still exists: $UNIT_FILE"
        errors=$((errors + 1))
    fi

    if [ -f "${INSTALL_DIR}/${BINARY_NAME}" ]; then
        log_error "Binary still exists: ${INSTALL_DIR}/${BINARY_NAME}"
        errors=$((errors + 1))
    fi

    if [ $errors -eq 0 ]; then
        log_info "Uninstallation verified successfully"
    else
        log_warn "Uninstallation completed with $errors warnings"
    fi
}

# Main
main() {
    log_info "RocketMan Tunnel Service - Linux Uninstallation"
    log_info "================================================"

    check_root
    stop_service
    remove_files
    verify_uninstall

    log_info "Uninstallation complete"
    log_info "Note: journal logs are retained. To clear them:"
    log_info "  journalctl --vacuum-time=1s -u ${SERVICE_NAME}"
}

main "$@"
