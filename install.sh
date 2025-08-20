#!/bin/bash

# sing-box Docker Management System - One-Click Installation Script
# Compatible with Debian/Ubuntu systems
# Pure ASCII characters for maximum terminal compatibility

set -e

# Global variables
INSTALL_DIR="$HOME/sing-box-docker"

# Pure ASCII logging functions
log_info() {
    echo "[INFO] $1"
}

log_success() {
    echo "[SUCCESS] $1"
}

log_warning() {
    echo "[WARNING] $1"
}

log_error() {
    echo "[ERROR] $1"
}

# Check if running as root user
check_root() {
    if [[ $EUID -eq 0 ]]; then
        log_error "Please do not run this script as root user"
        exit 1
    fi
}

# Check system type
check_system() {
    if [[ ! -f /etc/debian_version ]]; then
        log_error "This script only supports Debian/Ubuntu systems"
        exit 1
    fi
    log_success "System check passed"
}

# Install dependencies
install_dependencies() {
    log_info "Updating system package list..."
    sudo apt update -qq >/dev/null 2>&1

    log_info "Installing required dependencies..."
    sudo apt install -y curl wget openssl netcat-openbsd lsb-release >/dev/null 2>&1

    log_success "Dependencies installation completed"
}

# Clean up old installations
cleanup_old_installation() {
    log_info "Cleaning up old installations..."
    
    # Stop existing processes
    sudo pkill sing-box >/dev/null 2>&1 || true
    pkill sing-box-manager >/dev/null 2>&1 || true
    sleep 2
    
    # Backup old directory
    if [[ -d "$INSTALL_DIR" ]]; then
        log_warning "Found existing installation, creating backup"
        mv "$INSTALL_DIR" "${INSTALL_DIR}.backup.$(date +%Y%m%d_%H%M%S)"
    fi
    
    # Remove sing-box from system
    sudo rm -f /usr/local/bin/sing-box
    
    log_success "Old installation cleanup completed"
}

# Setup project directories
setup_directories() {
    log_info "Creating project directories..."
    
    mkdir -p "$INSTALL_DIR"
    mkdir -p "$INSTALL_DIR/configs"
    mkdir -p "$INSTALL_DIR/data"
    
    log_success "Directory structure created: $INSTALL_DIR"
}

# Download program components
download_components() {
    log_info "Downloading components to: $INSTALL_DIR"
    
    # Ensure we are in correct directory
    cd "$INSTALL_DIR"
    
    # Download management program
    log_info "Downloading sing-box management program..."
    if ! wget -q -O sing-box-manager-linux https://github.com/antsbtw/sing-box-docker/raw/main/sing-box-manager-linux; then
        log_error "Failed to download management program"
        exit 1
    fi
    chmod +x sing-box-manager-linux
    
    # Verify download
    if [[ ! -f "sing-box-manager-linux" ]]; then
        log_error "Management program file does not exist"
        exit 1
    fi
    
    log_info "Downloading sing-box core program..."
    if ! wget -q -O sing-box.tar.gz https://github.com/SagerNet/sing-box/releases/download/v1.8.10/sing-box-1.8.10-linux-amd64.tar.gz; then
        log_error "Failed to download sing-box"
        exit 1
    fi
    
    # Extract and install sing-box
    tar -xzf sing-box.tar.gz >/dev/null 2>&1
    sudo mv sing-box-*/sing-box /usr/local/bin/
    rm -rf sing-box.tar.gz sing-box-1.8.10-linux-amd64
    
    # Verify installation
    if ! command -v sing-box &> /dev/null; then
        log_error "sing-box installation failed"
        exit 1
    fi
    
    log_success "Program components download completed"
}

# Setup configuration files
setup_configs() {
    log_info "Creating configuration files..."
    
    # Ensure we are in correct directory
    cd "$INSTALL_DIR"
    
    # Create user data file
    cat > data/users.json << 'EOF'
{
  "users": []
}
EOF
    
    # Generate SSL certificates
    log_info "Generating SSL certificates..."
    openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
        -keyout configs/key.pem \
        -out configs/cert.pem \
        -subj "/C=US/ST=State/L=City/O=Organization/CN=example.com" \
        >/dev/null 2>&1
    
    log_success "Configuration files creation completed"
}

# Start services
start_services() {
    log_info "Starting services..."
    
    # Ensure we are in correct directory
    cd "$INSTALL_DIR"
    
    # Verify file exists
    if [[ ! -f "sing-box-manager-linux" ]]; then
        log_error "Management program file does not exist"
        exit 1
    fi
    
    # Start management program
    log_info "Starting management program..."
    ./sing-box-manager-linux > manager.log 2>&1 &
    MANAGER_PID=$!
    
    # Wait for startup
    log_info "Waiting for management program to start..."
    sleep 10
    
    # Check if process is still running
    if ! kill -0 $MANAGER_PID 2>/dev/null; then
        log_error "Management program startup failed"
        exit 1
    fi
    
    # Check if API is responding
    local retries=5
    while [[ $retries -gt 0 ]]; do
        if curl -s http://localhost:8080/health >/dev/null 2>&1; then
            break
        fi
        log_info "Waiting for API service to start..."
        sleep 3
        ((retries--))
    done
    
    if [[ $retries -eq 0 ]]; then
        log_error "Management program API startup failed"
        exit 1
    fi
    
    log_success "Management program started successfully"
    
    # Generate configuration
    log_info "Generating sing-box configuration..."
    if ! curl -s -X POST http://localhost:8080/api/config/generate >/dev/null; then
        log_error "Configuration generation failed"
        exit 1
    fi
    
    # Verify configuration
    if ! sing-box check -c configs/sing-box.json >/dev/null 2>&1; then
        log_error "Configuration verification failed"
        exit 1
    fi
    
    log_success "Configuration generated and verified successfully"
    
    # Start sing-box
    log_info "Starting proxy service..."
    sudo sing-box run -c configs/sing-box.json > singbox.log 2>&1 &
    SINGBOX_PID=$!
    
    # Wait for startup
    sleep 5
    
    # Check if sing-box started successfully
    if ! sudo kill -0 $SINGBOX_PID 2>/dev/null; then
        log_error "Proxy service startup failed"
        exit 1
    fi
    
    log_success "Proxy service started successfully"
}

# Verify installation
verify_installation() {
    log_info "Verifying installation..."
    
    cd "$INSTALL_DIR"
    
    # Check ports
    local ports=(443 8443 4433 1080 8080)
    local success_count=0
    
    for port in "${ports[@]}"; do
        if timeout 3 nc -z localhost $port >/dev/null 2>&1; then
            log_success "Port $port: Available"
            ((success_count++))
        else
            log_warning "Port $port: Unavailable"
        fi
    done
    
    # Check API
    if curl -s http://localhost:8080/health | grep -q "ok"; then
        log_success "Management API: Normal"
    else
        log_error "Management API: Abnormal"
        return 1
    fi
    
    log_success "Verification completed, $success_count ports available"
    return 0
}

# Show installation results
show_results() {
    local server_ip
    server_ip=$(hostname -I | awk '{print $1}' 2>/dev/null || echo "localhost")
    
    echo ""
    echo "================================================"
    echo "   sing-box Management System Installed"
    echo "================================================"
    echo ""
    echo "Service Information:"
    echo "  Management Panel: http://${server_ip}:8080"
    echo "  Project Directory: $INSTALL_DIR"
    echo ""
    echo "Proxy Protocols:"
    echo "  Trojan (Port 443)  - High-performance encrypted proxy"
    echo "  VLESS  (Port 8443) - Lightweight protocol" 
    echo "  Reality (Port 4433) - Advanced anti-censorship technology"
    echo "  Mixed   (Port 1080) - Local proxy support"
    echo ""
    echo "Common Commands:"
    echo "  View users: curl http://localhost:8080/api/users"
    echo "  View processes: ps aux | grep sing-box"
    echo "  View logs: cd $INSTALL_DIR && tail -f manager.log"
    echo ""
    echo "Documentation: https://github.com/antsbtw/sing-box-docker"
    echo "================================================"
    echo ""
}

# Main function
main() {
    echo "sing-box Docker Management System - One-Click Installation"
    echo "Compatible with Debian/Ubuntu systems"
    echo ""
    
    check_root
    check_system
    install_dependencies
    cleanup_old_installation
    setup_directories
    download_components
    setup_configs
    start_services
    
    if verify_installation; then
        show_results
        log_success "Installation completed successfully!"
        exit 0
    else
        log_error "Installation verification failed"
        exit 1
    fi
}

# Execute main function
main "$@"