#!/bin/bash

# RocketMan Tunnel Service - Installation Script for macOS
# This script downloads and installs the service as a LaunchDaemon

set -e

# Configuration
SERVICE_NAME="com.rocketman.tunnel"
GITHUB_REPO="RocketMan-System/rocketman-service" 
INSTALL_DIR="/usr/local/bin"
LAUNCHD_DIR="/Library/LaunchDaemons"
PLIST_FILE="${LAUNCHD_DIR}/${SERVICE_NAME}.plist"
BINARY_NAME="rocketman-tunnel-service"

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

# Detect architecture
detect_arch() {
    ARCH=$(uname -m)
    case $ARCH in
        x86_64)
            echo "amd64"
            ;;
        arm64)
            echo "arm64"
            ;;
        *)
            log_error "Unsupported architecture: $ARCH"
            exit 1
            ;;
    esac
}

# Download latest release from GitHub
download_binary() {
    log_info "Detecting system architecture..."
    ARCH=$(detect_arch)
    log_info "Architecture: $ARCH"

    log_info "Fetching latest release information..."
    
    # Get latest release URL
    RELEASE_URL="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"
    
    # Determine binary name based on architecture
    if [ "$ARCH" = "universal" ]; then
        BINARY_PATTERN="${BINARY_NAME}-macos-universal"
    else
        BINARY_PATTERN="${BINARY_NAME}-darwin-${ARCH}"
    fi
    
    # Download binary
    DOWNLOAD_URL=$(curl -s "$RELEASE_URL" | grep "browser_download_url.*${BINARY_NAME}" | cut -d '"' -f 4 | head -n 1)
    
    if [ -z "$DOWNLOAD_URL" ]; then
        log_error "Could not find download URL for latest release"
        log_info "Trying alternative pattern..."
        DOWNLOAD_URL=$(curl -s "$RELEASE_URL" | grep "browser_download_url" | grep -i "macos\|darwin" | cut -d '"' -f 4 | head -n 1)
    fi
    
    if [ -z "$DOWNLOAD_URL" ]; then
        log_error "Failed to get download URL from GitHub"
        exit 1
    fi
    
    log_info "Downloading from: $DOWNLOAD_URL"
    
    TMP_FILE="/tmp/${BINARY_NAME}"
    
    if ! curl -L -o "$TMP_FILE" "$DOWNLOAD_URL"; then
        log_error "Failed to download binary"
        exit 1
    fi
    
    # Make executable
    chmod +x "$TMP_FILE"
    
    # Move to installation directory
    log_info "Installing binary to ${INSTALL_DIR}/${BINARY_NAME}..."
    mv "$TMP_FILE" "${INSTALL_DIR}/${BINARY_NAME}"
    
    log_info "Binary installed successfully"
}

# Create LaunchDaemon plist
create_plist() {
    log_info "Creating LaunchDaemon configuration..."
    
    cat > "$PLIST_FILE" << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${SERVICE_NAME}</string>
    
    <key>ProgramArguments</key>
    <array>
        <string>${INSTALL_DIR}/${BINARY_NAME}</string>
    </array>
    
    <key>RunAtLoad</key>
    <true/>
    
    <key>KeepAlive</key>
    <true/>
    
    <key>StandardOutPath</key>
    <string>/var/log/${SERVICE_NAME}.log</string>
    
    <key>StandardErrorPath</key>
    <string>/var/log/${SERVICE_NAME}.error.log</string>
    
    <key>UserName</key>
    <string>root</string>
    
    <key>GroupName</key>
    <string>wheel</string>
</dict>
</plist>
EOF
    
    # Set correct permissions
    chmod 644 "$PLIST_FILE"
    chown root:wheel "$PLIST_FILE"
    
    log_info "LaunchDaemon configuration created"
}

# Stop existing service if running
stop_existing_service() {
    if launchctl list | grep -q "$SERVICE_NAME"; then
        log_info "Stopping existing service..."
        launchctl unload "$PLIST_FILE" 2>/dev/null || true
        sleep 1
    fi
}

# Load and start service
start_service() {
    log_info "Loading and starting service..."
    
    if ! launchctl load "$PLIST_FILE"; then
        log_error "Failed to load service"
        exit 1
    fi
    
    # Wait a bit for service to start
    sleep 2
    
    # Check if service is running
    if launchctl list | grep -q "$SERVICE_NAME"; then
        log_info "Service started successfully"
        
        # Test if HTTP server is responding
        if curl -s "http://127.0.0.1:5020/ping" > /dev/null 2>&1; then
            log_info "HTTP server is responding on port 5020"
        else
            log_warn "Service is loaded but HTTP server is not responding yet"
            log_warn "Check logs: tail -f /var/log/${SERVICE_NAME}.log"
        fi
    else
        log_error "Service failed to start"
        log_error "Check logs: tail -f /var/log/${SERVICE_NAME}.error.log"
        exit 1
    fi
}

# Main installation process
main() {
    log_info "RocketMan Tunnel Service - Installation"
    log_info "========================================"
    
    check_root
    stop_existing_service
    download_binary
    create_plist
    start_service
    
    echo ""
    log_info "Installation completed successfully!"
    log_info ""
    log_info "Service: ${SERVICE_NAME}"
    log_info "Binary: ${INSTALL_DIR}/${BINARY_NAME}"
    log_info "HTTP API: http://127.0.0.1:5020"
    log_info ""
    log_info "Useful commands:"
    log_info "  - Check status: launchctl list | grep ${SERVICE_NAME}"
    log_info "  - View logs: tail -f /var/log/${SERVICE_NAME}.log"
    log_info "  - View errors: tail -f /var/log/${SERVICE_NAME}.error.log"
    log_info "  - Restart: sudo launchctl unload ${PLIST_FILE} && sudo launchctl load ${PLIST_FILE}"
    log_info "  - Uninstall: Run uninstall.sh with sudo"
}

main "$@"