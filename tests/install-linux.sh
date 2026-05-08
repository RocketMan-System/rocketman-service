#!/bin/bash

# RocketMan Tunnel Service - Local build & install script (for testing/development)
# Builds from source for the current architecture and installs the service.

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

SERVICE_NAME="rocketman-tunnel"
BINARY_NAME="rocketman-tunnel-service"
INSTALL_DIR="/usr/local/bin"
SYSTEMD_DIR="/etc/systemd/system"
UNIT_FILE="${SYSTEMD_DIR}/${SERVICE_NAME}.service"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

check_root() {
    if [ "$EUID" -ne 0 ]; then
        log_error "This script must be run with sudo"
        exit 1
    fi
}

detect_goarch() {
    case "$(uname -m)" in
        x86_64)  echo "amd64" ;;
        aarch64) echo "arm64" ;;
        armv7l)  echo "arm"   ;;
        *)
            log_error "Unsupported architecture: $(uname -m)"
            exit 1
            ;;
    esac
}

build_binary() {
    log_info "Detecting system architecture..."
    GOARCH=$(detect_goarch)
    log_info "Architecture: $(uname -m) → GOARCH=${GOARCH}"

    if ! command -v go &>/dev/null; then
        log_error "Go compiler not found. Install Go and retry."
        exit 1
    fi

    BUILD_OUT="${REPO_ROOT}/build/linux/${BINARY_NAME}"
    mkdir -p "$(dirname "$BUILD_OUT")"

    log_info "Building from source (${REPO_ROOT}/src/linux)..."
    GOOS=linux GOARCH="$GOARCH" go build \
        -o "$BUILD_OUT" \
        "${REPO_ROOT}/src/linux"

    chmod +x "$BUILD_OUT"
    log_info "Binary built: ${BUILD_OUT}"
}

install_binary() {
    log_info "Installing binary to ${INSTALL_DIR}/${BINARY_NAME}..."
    cp "${REPO_ROOT}/build/linux/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
    chmod +x "${INSTALL_DIR}/${BINARY_NAME}"
    log_info "Binary installed."
}

create_unit_file() {
    log_info "Creating systemd unit file..."
    cat > "$UNIT_FILE" << EOF
[Unit]
Description=RocketMan Tunnel Service
After=network.target
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
User=root
Group=root
ExecStart=${INSTALL_DIR}/${BINARY_NAME}
Restart=always
RestartSec=3
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

[Install]
WantedBy=multi-user.target
EOF
    chmod 644 "$UNIT_FILE"
    log_info "Unit file created: ${UNIT_FILE}"
}

stop_existing_service() {
    if systemctl is-active --quiet "$SERVICE_NAME" 2>/dev/null; then
        log_info "Stopping existing service..."
        systemctl stop "$SERVICE_NAME" || true
        sleep 1
    fi
    if systemctl is-enabled --quiet "$SERVICE_NAME" 2>/dev/null; then
        systemctl disable "$SERVICE_NAME" || true
    fi
}

start_service() {
    log_info "Reloading systemd daemon..."
    systemctl daemon-reload

    log_info "Enabling and starting service..."
    systemctl enable "$SERVICE_NAME"
    systemctl start "$SERVICE_NAME"

    sleep 2

    if systemctl is-active --quiet "$SERVICE_NAME"; then
        log_info "Service started successfully"
        if curl -s --max-time 3 "http://127.0.0.1:5020/ping" > /dev/null 2>&1; then
            log_info "HTTP server is responding on port 5020"
        else
            log_warn "Service is running but HTTP server is not responding yet"
            log_warn "Check logs: journalctl -u ${SERVICE_NAME} -f"
        fi
    else
        log_error "Service failed to start"
        log_error "Check logs: journalctl -u ${SERVICE_NAME} -xe"
        exit 1
    fi
}

main() {
    log_info "RocketMan Tunnel Service - Local Install"
    log_info "========================================"

    check_root
    build_binary
    stop_existing_service
    install_binary
    create_unit_file
    start_service

    log_info "Installation complete!"
    log_info "Useful commands:"
    log_info "  Status : systemctl status ${SERVICE_NAME}"
    log_info "  Logs   : journalctl -u ${SERVICE_NAME} -f"
    log_info "  Stop   : systemctl stop ${SERVICE_NAME}"
    log_info "  Start  : systemctl start ${SERVICE_NAME}"
}

main "$@"
