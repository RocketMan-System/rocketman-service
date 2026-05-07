#!/bin/bash

# RocketMan Tunnel Service - Installation Script for Linux
# Installs the service as a systemd system service

set -e

# Configuration
SERVICE_NAME="rocketman-tunnel"
GITHUB_REPO="RocketMan-System/rocketman-service"
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

# Detect architecture
detect_arch() {
    ARCH=$(uname -m)
    case $ARCH in
        x86_64)  echo "amd64" ;;
        aarch64) echo "arm64" ;;
        armv7l)  echo "arm"   ;;
        *)
            log_error "Unsupported architecture: $ARCH"
            exit 1
            ;;
    esac
}

# Download latest release binary from GitHub
download_binary() {
    log_info "Detecting system architecture..."
    ARCH=$(detect_arch)
    log_info "Architecture: $ARCH"

    log_info "Fetching latest release information..."
    RELEASE_URL="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"

    DOWNLOAD_URL=$(curl -s "$RELEASE_URL" \
        | grep "browser_download_url" \
        | grep -i "linux.*${ARCH}\|${ARCH}.*linux" \
        | cut -d '"' -f 4 \
        | head -n 1)

    if [ -z "$DOWNLOAD_URL" ]; then
        # Fallback: any linux binary
        DOWNLOAD_URL=$(curl -s "$RELEASE_URL" \
            | grep "browser_download_url" \
            | grep -i "linux" \
            | cut -d '"' -f 4 \
            | head -n 1)
    fi

    if [ -z "$DOWNLOAD_URL" ]; then
        log_error "Could not find a Linux binary in the latest release"
        exit 1
    fi

    log_info "Downloading from: $DOWNLOAD_URL"
    TMP_FILE="/tmp/${BINARY_NAME}"

    if ! curl -L -o "$TMP_FILE" "$DOWNLOAD_URL"; then
        log_error "Failed to download binary"
        exit 1
    fi

    chmod +x "$TMP_FILE"
    mv "$TMP_FILE" "${INSTALL_DIR}/${BINARY_NAME}"
    log_info "Binary installed to ${INSTALL_DIR}/${BINARY_NAME}"
}

# Create systemd unit file
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
    log_info "Unit file created: $UNIT_FILE"
}

# Stop existing service if running
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

# Enable and start the service
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

# Main
main() {
    log_info "RocketMan Tunnel Service - Linux Installation"
    log_info "============================================="

    check_root
    stop_existing_service
    download_binary
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
