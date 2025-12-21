#!/bin/bash
set -e

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  OTun Node Agent Installer v1.0.0${NC}"
echo -e "${GREEN}========================================${NC}"

# 检查 root 权限
if [ "$EUID" -ne 0 ]; then
    echo -e "${RED}Please run as root (sudo)${NC}"
    exit 1
fi

# 彻底清理已有环境
echo -e "${YELLOW}Cleaning up existing installation...${NC}"

# 停止服务
systemctl stop otun-agent 2>/dev/null || true
systemctl stop sing-box 2>/dev/null || true
systemctl disable otun-agent 2>/dev/null || true
systemctl disable sing-box 2>/dev/null || true

# 强制终止进程
pkill -9 sing-box 2>/dev/null || true
pkill -9 agent 2>/dev/null || true
sleep 2

# 删除旧的二进制文件
rm -f /usr/local/bin/sing-box 2>/dev/null || true
rm -f /opt/otun-agent/agent 2>/dev/null || true

# 删除旧的配置（保留用户数据）
rm -f /etc/sing-box/config.json 2>/dev/null || true

# 删除旧的 systemd 服务文件
rm -f /etc/systemd/system/otun-agent.service 2>/dev/null || true
rm -f /etc/systemd/system/sing-box.service 2>/dev/null || true
systemctl daemon-reload

echo -e "${GREEN}Cleanup completed${NC}"

# 安装必要依赖
echo -e "${GREEN}Installing dependencies...${NC}"
apt-get update -qq
apt-get install -y -qq git curl

# 解析参数
NODE_API_KEY=""
NODE_ID="node-$(hostname)"
VLESS_PORT=443
MANAGEMENT_MODE="local"
SERVER_IP=""

# 默认值
API_URL="https://otun-manager.situstechnologies.com"

while [[ $# -gt 0 ]]; do
    case $1 in
        --api-key) NODE_API_KEY="$2"; shift 2 ;;
        --node-id) NODE_ID="$2"; shift 2 ;;
        --api-url) API_URL="$2"; shift 2 ;;
        --vless-port) VLESS_PORT="$2"; shift 2 ;;
        --management-mode) MANAGEMENT_MODE="$2"; shift 2 ;;
        --server-ip) SERVER_IP="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "$NODE_API_KEY" ]; then
    echo -e "${RED}Error: --api-key is required${NC}"
    echo "Usage: $0 --api-key <key> [--node-id <id>] [--vless-port <port>] [--management-mode local|remote|hybrid] [--server-ip <ip>]"
    exit 1
fi

echo -e "${YELLOW}Node ID: ${NODE_ID}${NC}"
echo -e "${YELLOW}VLESS Port: ${VLESS_PORT}${NC}"
echo -e "${YELLOW}Management Mode: ${MANAGEMENT_MODE}${NC}"

# 安装目录
INSTALL_DIR="/opt/otun-agent"
mkdir -p $INSTALL_DIR
cd $INSTALL_DIR

# 安装 Go (仅用于编译 agent)
GO_VERSION="1.23.4"
echo -e "${GREEN}Installing Go ${GO_VERSION}...${NC}"
rm -rf /usr/local/go
ARCH=$(uname -m)
case $ARCH in
    x86_64) GO_ARCH="amd64" ;;
    aarch64) GO_ARCH="arm64" ;;
    *) echo -e "${RED}Unsupported architecture: $ARCH${NC}"; exit 1 ;;
esac
curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz" -o go.tar.gz
tar -C /usr/local -xzf go.tar.gz
rm go.tar.gz
export PATH=$PATH:/usr/local/go/bin
grep -q '/usr/local/go/bin' /etc/profile || echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile
echo -e "${GREEN}Go installed: $(go version)${NC}"

# 下载预编译的 sing-box (已包含 v2ray_api 和 utls 支持)
echo -e "${GREEN}Downloading pre-built sing-box with v2ray_api support...${NC}"

# sing-box 版本和预编译二进制下载地址
SINGBOX_VERSION="1.10.7"

# 确定架构
case $ARCH in
    x86_64) SINGBOX_ARCH="amd64" ;;
    aarch64) SINGBOX_ARCH="arm64" ;;
esac

# 从 GitHub Release 下载预编译二进制文件
# 这个二进制文件由项目维护者预编译，包含 with_v2ray_api,with_utls 标签
SINGBOX_URL="https://github.com/antsbtw/sing-box-docker/releases/download/v${SINGBOX_VERSION}/sing-box-linux-${SINGBOX_ARCH}"

echo -e "${YELLOW}Downloading sing-box v${SINGBOX_VERSION} for ${SINGBOX_ARCH}...${NC}"
if ! curl -fsSL "$SINGBOX_URL" -o /usr/local/bin/sing-box; then
    echo -e "${RED}Failed to download sing-box from ${SINGBOX_URL}${NC}"
    echo -e "${YELLOW}Falling back to source compilation...${NC}"

    # 备用方案：从源码编译
    cd /tmp
    rm -rf sing-box-src
    git clone --depth 1 --branch "v${SINGBOX_VERSION}" https://github.com/SagerNet/sing-box.git sing-box-src
    cd sing-box-src
    if ! go build -tags "with_v2ray_api,with_utls,with_reality_server" -o sing-box ./cmd/sing-box; then
        echo -e "${RED}Failed to build sing-box${NC}"
        cd /tmp && rm -rf sing-box-src
        exit 1
    fi
    mv sing-box /usr/local/bin/
    cd /tmp && rm -rf sing-box-src
fi

chmod +x /usr/local/bin/sing-box
setcap cap_net_bind_service=+ep /usr/local/bin/sing-box

# 验证安装
if ! sing-box version > /dev/null 2>&1; then
    echo -e "${RED}sing-box installation verification failed${NC}"
    exit 1
fi
echo -e "${GREEN}sing-box installed: $(sing-box version | head -1)${NC}"

cd $INSTALL_DIR

# 下载预编译的 agent
echo -e "${GREEN}Downloading OTun Node Agent...${NC}"

# 确定架构
case $ARCH in
    x86_64) AGENT_ARCH="amd64" ;;
    aarch64) AGENT_ARCH="arm64" ;;
esac

# 从 GitHub Release 下载预编译的 agent
AGENT_URL="https://github.com/antsbtw/sing-box-docker/releases/download/latest/agent-linux-${AGENT_ARCH}"

echo -e "${YELLOW}Downloading agent for ${AGENT_ARCH}...${NC}"
if curl -fsSL "$AGENT_URL" -o $INSTALL_DIR/agent; then
    chmod +x $INSTALL_DIR/agent
    echo -e "${GREEN}Agent downloaded successfully${NC}"
else
    echo -e "${YELLOW}Download failed, falling back to source compilation...${NC}"

    # 备用方案：从源码编译
    if [ -d "repo" ]; then
        cd repo
        git fetch origin
        git reset --hard origin/main
    else
        git clone https://github.com/antsbtw/sing-box-docker.git repo
        cd repo
    fi

    echo -e "${GREEN}Building agent from source...${NC}"
    if ! go build -o $INSTALL_DIR/agent ./cmd/agent; then
        echo -e "${RED}Failed to build agent${NC}"
        exit 1
    fi
    cd $INSTALL_DIR
fi

if [ ! -f "$INSTALL_DIR/agent" ]; then
    echo -e "${RED}Agent binary not found${NC}"
    exit 1
fi
echo -e "${GREEN}Agent ready${NC}"

# 创建数据目录
mkdir -p $INSTALL_DIR/data
mkdir -p /etc/sing-box

# 创建初始配置
cat > /etc/sing-box/config.json << 'CONF'
{
  "log": {"level": "info", "timestamp": true},
  "inbounds": [],
  "outbounds": [{"type": "direct", "tag": "direct"}]
}
CONF

# 创建 systemd 服务
cat > /etc/systemd/system/otun-agent.service << SYSTEMD
[Unit]
Description=OTun Node Agent
After=network.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
Environment="NODE_API_KEY=$NODE_API_KEY"
Environment="NODE_ID=$NODE_ID"
Environment="VLESS_PORT=$VLESS_PORT"
Environment="OTUN_API_URL=$API_URL"
Environment="MANAGEMENT_MODE=$MANAGEMENT_MODE"
Environment="SERVER_IP=$SERVER_IP"
ExecStart=$INSTALL_DIR/agent
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
SYSTEMD

# 启动服务
systemctl daemon-reload
systemctl enable otun-agent
systemctl start otun-agent

# 创建管理命令
cat > /usr/local/bin/otun << 'CMD'
#!/bin/bash
case "$1" in
    start)   systemctl start otun-agent ;;
    stop)    systemctl stop otun-agent ;;
    restart) systemctl restart otun-agent ;;
    status)  systemctl status otun-agent ;;
    logs)    journalctl -u otun-agent -f ;;
    *)       echo "Usage: otun {start|stop|restart|status|logs}" ;;
esac
CMD
chmod +x /usr/local/bin/otun

echo ""
echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  Installation Complete!${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""
echo -e "Node ID: ${YELLOW}$NODE_ID${NC}"
echo -e "Config:  ${YELLOW}/etc/sing-box/config.json${NC}"
echo -e "Data:    ${YELLOW}$INSTALL_DIR/data${NC}"
echo ""
echo -e "Commands:"
echo -e "  ${YELLOW}otun status${NC}  - Check service status"
echo -e "  ${YELLOW}otun logs${NC}    - View logs"
echo -e "  ${YELLOW}otun restart${NC} - Restart service"
echo ""
echo -e "${GREEN}Secrets generated:${NC}"
cat $INSTALL_DIR/data/secrets.json 2>/dev/null || echo "Will be generated on first run"
