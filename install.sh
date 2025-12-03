#!/bin/bash
set -e

# OTun Node Agent 一键部署脚本
# 用法: curl -fsSL https://your-domain/install.sh | bash -s -- --api-key YOUR_KEY --node-id NODE_ID

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# 默认值
INSTALL_DIR="/opt/otun-agent"
API_URL="https://saasapi.situstechnologies.com"
NODE_ID=""
API_KEY=""
VLESS_PORT=443
SS_PORT=8388

# 解析参数
while [[ $# -gt 0 ]]; do
    case $1 in
        --api-key)    API_KEY="$2"; shift 2 ;;
        --node-id)    NODE_ID="$2"; shift 2 ;;
        --api-url)    API_URL="$2"; shift 2 ;;
        --vless-port) VLESS_PORT="$2"; shift 2 ;;
        --ss-port)    SS_PORT="$2"; shift 2 ;;
        --install-dir) INSTALL_DIR="$2"; shift 2 ;;
        -h|--help)
            echo "Usage: $0 --api-key KEY --node-id ID [options]"
            echo ""
            echo "Required:"
            echo "  --api-key KEY      Node API key from management server"
            echo "  --node-id ID       Unique node identifier"
            echo ""
            echo "Optional:"
            echo "  --api-url URL      Management server URL (default: $API_URL)"
            echo "  --vless-port PORT  VLESS port (default: 443)"
            echo "  --ss-port PORT     Shadowsocks port (default: 8388)"
            echo "  --install-dir DIR  Installation directory (default: $INSTALL_DIR)"
            exit 0
            ;;
        *) log_error "Unknown option: $1"; exit 1 ;;
    esac
done

# 验证必要参数
if [ -z "$API_KEY" ]; then
    log_error "API key is required. Use --api-key YOUR_KEY"
    exit 1
fi

if [ -z "$NODE_ID" ]; then
    NODE_ID="node-$(hostname)-$(date +%s | tail -c 5)"
    log_warn "No node ID specified, using: $NODE_ID"
fi

# 检查 root 权限
if [ "$EUID" -ne 0 ]; then
    log_error "Please run as root (use sudo)"
    exit 1
fi

# 检查系统
log_info "Checking system requirements..."
if ! command -v docker &> /dev/null; then
    log_info "Installing Docker..."
    curl -fsSL https://get.docker.com | sh
    systemctl enable docker
    systemctl start docker
fi

if ! command -v docker-compose &> /dev/null && ! docker compose version &> /dev/null; then
    log_info "Installing Docker Compose..."
    apt-get update && apt-get install -y docker-compose-plugin
fi

# 创建安装目录
log_info "Creating installation directory: $INSTALL_DIR"
mkdir -p "$INSTALL_DIR"/{data,singbox}
cd "$INSTALL_DIR"

# 创建 docker-compose.yml
log_info "Creating docker-compose.yml..."
cat > docker-compose.yml << EOF
version: '3.8'

services:
  otun-agent:
    image: ghcr.io/situstechnologies/otun-node-agent:latest
    container_name: otun-agent
    restart: unless-stopped
    network_mode: host
    
    environment:
      - OTUN_API_URL=${API_URL}
      - NODE_API_KEY=${API_KEY}
      - NODE_ID=${NODE_ID}
      - VLESS_PORT=${VLESS_PORT}
      - SS_PORT=${SS_PORT}
      - LOG_LEVEL=info
    
    volumes:
      - ./data:/app/data
      - ./singbox:/etc/sing-box
    
    cap_add:
      - NET_ADMIN
      - NET_RAW
    
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
EOF

# 创建 .env 文件
cat > .env << EOF
NODE_API_KEY=${API_KEY}
NODE_ID=${NODE_ID}
OTUN_API_URL=${API_URL}
VLESS_PORT=${VLESS_PORT}
SS_PORT=${SS_PORT}
EOF

# 创建管理脚本
log_info "Creating management scripts..."
cat > otun << 'MGMT'
#!/bin/bash
cd /opt/otun-agent

case "$1" in
    start)   docker compose up -d ;;
    stop)    docker compose down ;;
    restart) docker compose restart ;;
    logs)    docker compose logs -f --tail=100 ;;
    status)  docker compose ps ;;
    update)  docker compose pull && docker compose up -d ;;
    *)
        echo "Usage: otun {start|stop|restart|logs|status|update}"
        exit 1
        ;;
esac
MGMT
chmod +x otun
ln -sf "$INSTALL_DIR/otun" /usr/local/bin/otun

# 启动服务
log_info "Starting OTun Node Agent..."
docker compose pull
docker compose up -d

# 等待启动
sleep 3

# 检查状态
if docker compose ps | grep -q "Up"; then
    log_info "========================================="
    log_info "  OTun Node Agent installed successfully!"
    log_info "========================================="
    echo ""
    echo "Node ID:     $NODE_ID"
    echo "VLESS Port:  $VLESS_PORT"
    echo "SS Port:     $SS_PORT"
    echo "Install Dir: $INSTALL_DIR"
    echo ""
    echo "Management commands:"
    echo "  otun start   - Start the agent"
    echo "  otun stop    - Stop the agent"
    echo "  otun restart - Restart the agent"
    echo "  otun logs    - View logs"
    echo "  otun status  - Check status"
    echo "  otun update  - Update to latest version"
    echo ""
    
    # 显示公钥
    if [ -f "$INSTALL_DIR/data/keys.json" ]; then
        PUBLIC_KEY=$(grep -o '"public_key":"[^"]*' "$INSTALL_DIR/data/keys.json" | cut -d'"' -f4)
        echo "Reality Public Key: $PUBLIC_KEY"
        echo "(Use this key in your client configuration)"
    fi
else
    log_error "Failed to start agent. Check logs with: docker compose logs"
    exit 1
fi
