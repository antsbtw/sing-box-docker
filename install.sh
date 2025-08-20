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

# 全局变量
INSTALL_DIR="$HOME/sing-box-docker"

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
    sudo apt update -qq

    log_info "安装必要依赖..."
    sudo apt install -y curl wget openssl netcat-openbsd lsb-release

    log_success "依赖包安装完成"
}

# 清理旧安装
cleanup_old_installation() {
    log_info "清理旧版本..."
    
    # 停止现有进程
    sudo pkill sing-box || true
    pkill sing-box-manager || true
    sleep 2
    
    # 备份旧目录
    if [[ -d "$INSTALL_DIR" ]]; then
        log_warning "发现旧安装，备份到 ${INSTALL_DIR}.backup.$(date +%Y%m%d_%H%M%S)"
        mv "$INSTALL_DIR" "${INSTALL_DIR}.backup.$(date +%Y%m%d_%H%M%S)"
    fi
    
    # 删除系统中的sing-box
    sudo rm -f /usr/local/bin/sing-box
    
    log_success "旧版本清理完成"
}

# 创建项目目录
setup_directories() {
    log_info "创建项目目录..."
    
    mkdir -p "$INSTALL_DIR"
    mkdir -p "$INSTALL_DIR/configs"
    mkdir -p "$INSTALL_DIR/data"
    
    log_success "目录结构创建完成: $INSTALL_DIR"
}

# 下载程序组件
download_components() {
    log_info "下载组件到: $INSTALL_DIR"
    
    # 确保在正确目录
    cd "$INSTALL_DIR"
    
    # 下载管理程序
    log_info "下载 sing-box 管理程序..."
    if ! wget -O sing-box-manager-linux https://github.com/antsbtw/sing-box-docker/raw/main/sing-box-manager-linux; then
        log_error "下载管理程序失败"
        exit 1
    fi
    chmod +x sing-box-manager-linux
    
    # 验证下载
    if [[ ! -f "sing-box-manager-linux" ]]; then
        log_error "管理程序文件不存在"
        exit 1
    fi
    
    log_info "下载 sing-box 核心程序..."
    if ! wget -O sing-box.tar.gz https://github.com/SagerNet/sing-box/releases/download/v1.8.10/sing-box-1.8.10-linux-amd64.tar.gz; then
        log_error "下载 sing-box 失败"
        exit 1
    fi
    
    # 解压并安装sing-box
    tar -xzf sing-box.tar.gz
    sudo mv sing-box-*/sing-box /usr/local/bin/
    rm -rf sing-box.tar.gz sing-box-1.8.10-linux-amd64
    
    # 验证安装
    if ! command -v sing-box &> /dev/null; then
        log_error "sing-box 安装失败"
        exit 1
    fi
    
    log_success "程序组件下载完成"
    log_info "文件位置: $(pwd)"
    log_info "文件列表: $(ls -la sing-box-manager-linux)"
}

# 创建配置文件
setup_configs() {
    log_info "创建配置文件..."
    
    # 确保在正确目录
    cd "$INSTALL_DIR"
    
    # 创建用户数据文件
    cat > data/users.json << 'USEREOF'
{
  "users": []
}
USEREOF
    
    # 生成SSL证书
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
    log_info "启动服务..."
    
    # 确保在正确目录
    cd "$INSTALL_DIR"
    
    # 验证文件存在
    if [[ ! -f "sing-box-manager-linux" ]]; then
        log_error "管理程序文件不存在: $(pwd)/sing-box-manager-linux"
        log_info "当前目录内容: $(ls -la)"
        exit 1
    fi
    
    # 启动管理程序
    log_info "启动管理程序..."
    ./sing-box-manager-linux > manager.log 2>&1 &
    MANAGER_PID=$!
    
    # 等待启动
    log_info "等待管理程序启动 (PID: $MANAGER_PID)..."
    sleep 10
    
    # 检查进程是否还在运行
    if ! kill -0 $MANAGER_PID 2>/dev/null; then
        log_error "管理程序启动失败"
        log_info "错误日志："
        cat manager.log
        exit 1
    fi
    
    # 检查API是否响应
    local retries=5
    while [[ $retries -gt 0 ]]; do
        if curl -s http://localhost:8080/health > /dev/null 2>&1; then
            break
        fi
        log_info "等待API服务... (剩余重试: $retries)"
        sleep 3
        ((retries--))
    done
    
    if [[ $retries -eq 0 ]]; then
        log_error "管理程序API启动失败"
        log_info "错误日志："
        cat manager.log
        exit 1
    fi
    
    log_success "管理程序启动成功"
    
    # 生成配置
    log_info "生成sing-box配置..."
    if ! curl -s -X POST http://localhost:8080/api/config/generate > /dev/null; then
        log_error "配置生成失败"
        exit 1
    fi
    
    # 验证配置
    if ! sing-box check -c configs/sing-box.json > /dev/null 2>&1; then
        log_error "配置验证失败"
        exit 1
    fi
    
    log_success "配置生成并验证完成"
    
    # 启动sing-box
    log_info "启动代理服务..."
    sudo sing-box run -c configs/sing-box.json > singbox.log 2>&1 &
    SINGBOX_PID=$!
    
    # 等待启动
    sleep 5
    
    # 检查sing-box是否启动成功
    if ! sudo kill -0 $SINGBOX_PID 2>/dev/null; then
        log_error "代理服务启动失败"
        log_info "错误日志："
        cat singbox.log
        exit 1
    fi
    
    log_success "代理服务启动成功 (PID: $SINGBOX_PID)"
}

# 验证安装
verify_installation() {
    log_info "验证安装..."
    
    cd "$INSTALL_DIR"
    
    # 检查端口
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
    
    # 检查API
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
        log_info "这可能是正常的，某些端口需要root权限"
        return 0
    fi
}

# 显示安装结果
show_results() {
    local server_ip=$(hostname -I | awk '{print $1}' 2>/dev/null || echo "localhost")
    
    echo
    echo "🎉 sing-box Docker管理系统安装完成！"
    echo
    echo "📋 服务信息："
    echo "  • 管理面板: http://${server_ip}:8080"
    echo "  • 项目目录: $INSTALL_DIR"
    echo "  • 进程状态: $(ps aux | grep -E '(sing-box|sing-box-manager)' | grep -v grep | wc -l) 个进程运行中"
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
    echo "  • 查看进程: ps aux | grep -E '(sing-box|sing-box-manager)'"
    echo "  • 查看日志: cd $INSTALL_DIR && tail -f manager.log"
    echo
    echo "📝 更多文档: https://github.com/antsbtw/sing-box-docker"
    echo
}

# 主函数
main() {
    echo "🚀 sing-box Docker管理系统 一键安装脚本"
    echo "   适用于 Debian/Ubuntu 系统"
    echo "   安装目录: $INSTALL_DIR"
    echo
    
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
        log_success "安装成功完成！"
        exit 0
    else
        log_error "安装验证失败，请检查错误信息"
        exit 1
    fi
}

# 错误处理
trap 'log_error "脚本执行出错在第 $LINENO 行，请检查错误信息"; exit 1' ERR

# 执行主函数
main "$@"