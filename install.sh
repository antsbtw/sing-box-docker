#!/bin/bash

# sing-box Docker管理系统 一键安装脚本
# 适用于 Debian/Ubuntu 系统

set -e

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# 日志函数
log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# 检查是否为root用户
check_root() {
    if [[ $EUID -eq 0 ]]; then
        log_error "请不要使用root用户运行此脚本"
        exit 1
    fi
}

# 检查系统类型
check_system() {
    if [[ ! -f /etc/debian_version ]]; then
        log_error "此脚本仅支持 Debian/Ubuntu 系统"
        exit 1
    fi
    log_success "系统检查通过"
}

# 安装依赖包
install_dependencies() {
    log_info "更新系统包列表..."
    sudo apt update

    log_info "安装必要依赖..."
    sudo apt install -y curl wget openssl netcat-openbsd

    log_success "依赖包安装完成"
}

# 创建项目目录
setup_directories() {
    log_info "创建项目目录..."
    
    INSTALL_DIR="$HOME/sing-box-docker"
    
    if [[ -d "$INSTALL_DIR" ]]; then
        log_warning "目录已存在，将备份旧版本"
        mv "$INSTALL_DIR" "$INSTALL_DIR.backup.$(date +%Y%m%d_%H%M%S)"
    fi
    
    mkdir -p "$INSTALL_DIR"
    cd "$INSTALL_DIR"
    
    mkdir -p configs data
    
    log_success "目录结构创建完成: $INSTALL_DIR"
}

# 下载程序组件
download_components() {
    log_info "下载 sing-box 管理程序..."
    
    if ! wget -O sing-box-manager-linux https://github.com/antsbtw/sing-box-docker/raw/main/sing-box-manager-linux; then
        log_error "下载管理程序失败"
        exit 1
    fi
    chmod +x sing-box-manager-linux
    
    log_info "下载 sing-box 核心程序..."
    
    if ! wget -O sing-box.tar.gz https://github.com/SagerNet/sing-box/releases/download/v1.8.10/sing-box-1.8.10-linux-amd64.tar.gz; then
        log_error "下载 sing-box 失败"
        exit 1
    fi
    
    tar -xzf sing-box.tar.gz
    sudo mv sing-box-*/sing-box /usr/local/bin/
    rm -rf sing-box*
    
    if ! command -v sing-box &> /dev/null; then
        log_error "sing-box 安装失败"
        exit 1
    fi
    
    log_success "程序组件下载完成"
}

# 创建配置文件
setup_configs() {
    log_info "创建配置文件..."
    
    cat > data/users.json << 'USEREOF'
{
  "users": []
}
USEREOF
    
    log_info "生成SSL证书..."
    openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
        -keyout configs/key.pem \
        -out configs/cert.pem \
        -subj "/C=US/ST=State/L=City/O=Organization/CN=example.com" \
        2>/dev/null
    
    log_success "配置文件创建完成"
}

# 启动服务
start_services() {
    log_info "启动管理服务..."
    
    ./sing-box-manager-linux &
    
    log_info "等待管理程序启动..."
    sleep 8
    
    if ! curl -s http://localhost:8080/health > /dev/null; then
        log_error "管理程序启动失败"
        exit 1
    fi
    
    log_success "管理程序启动成功"
    
    log_info "生成sing-box配置..."
    if ! curl -s -X POST http://localhost:8080/api/config/generate > /dev/null; then
        log_error "配置生成失败"
        exit 1
    fi
    
    if ! sing-box check -c configs/sing-box.json > /dev/null 2>&1; then
        log_error "配置验证失败"
        exit 1
    fi
    
    log_success "配置生成并验证完成"
    
    log_info "启动代理服务..."
    sudo sing-box run -c configs/sing-box.json &
    
    sleep 5
    
    log_success "代理服务启动成功"
}

# 验证安装
verify_installation() {
    log_info "验证安装..."
    
    local ports=(443 8443 4433 1080 8080)
    local failed_ports=()
    
    for port in "${ports[@]}"; do
        if timeout 3 nc -z localhost $port 2>/dev/null; then
            log_success "端口 $port: ✅ 可用"
        else
            failed_ports+=($port)
            log_warning "端口 $port: ❌ 不可用"
        fi
    done
    
    if curl -s http://localhost:8080/health | grep -q "ok"; then
        log_success "管理API: ✅ 正常"
    else
        log_error "管理API: ❌ 异常"
        return 1
    fi
    
    if [[ ${#failed_ports[@]} -eq 0 ]]; then
        log_success "所有服务验证通过！"
        return 0
    else
        log_warning "部分端口验证失败: ${failed_ports[*]}"
        return 1
    fi
}

# 显示安装结果
show_results() {
    echo
    echo "🎉 sing-box Docker管理系统安装完成！"
    echo
    echo "📋 服务信息："
    echo "  • 管理面板: http://localhost:8080"
    echo "  • 项目目录: ~/sing-box-docker"
    echo
    echo "🌐 代理协议："
    echo "  • Trojan (端口 443)  - 高性能加密代理"
    echo "  • VLESS  (端口 8443) - 轻量级协议"
    echo "  • Reality (端口 4433) - 🔥 最先进抗封锁技术"
    echo "  • Mixed   (端口 1080) - 本地代理支持"
    echo
    echo "🔧 常用命令："
    echo "  • 查看用户: curl http://localhost:8080/api/users"
    echo "  • 创建用户: curl -X POST http://localhost:8080/api/users -H 'Content-Type: application/json' -d '{\"username\":\"user1\",\"password\":\"pass123\",\"expires_at\":\"2025-12-31T23:59:59Z\",\"traffic_limit\":107374182400,\"device_limit\":3}'"
    echo
    echo "📝 更多文档: https://github.com/antsbtw/sing-box-docker"
}

# 主函数
main() {
    echo "🚀 sing-box Docker管理系统 一键安装脚本"
    echo "   适用于 Debian/Ubuntu 系统"
    echo
    
    check_root
    check_system
    install_dependencies
    setup_directories
    download_components
    setup_configs
    start_services
    
    if verify_installation; then
        show_results
        log_success "安装成功完成！"
        exit 0
    else
        log_error "安装验证失败，请检查错误信息"
        exit 1
    fi
}

# 错误处理
trap 'log_error "脚本执行出错，请检查错误信息"; exit 1' ERR

# 执行主函数
main "$@"
