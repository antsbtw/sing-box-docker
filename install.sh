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

# 解析参数
NODE_API_KEY=""
NODE_ID="node-$(hostname)"
VLESS_PORT=443

# 默认值
API_URL="https://otun-manager.situstechnologies.com"

while [[ $# -gt 0 ]]; do
    case $1 in
        --api-key) NODE_API_KEY="$2"; shift 2 ;;
        --node-id) NODE_ID="$2"; shift 2 ;;
        --api-url) API_URL="$2"; shift 2 ;;
        --vless-port) VLESS_PORT="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

if [ -z "$NODE_API_KEY" ]; then
    echo -e "${RED}Error: --api-key is required${NC}"
    echo "Usage: $0 --api-key <key> [--node-id <id>] [--vless-port <port>]"
    exit 1
fi

echo -e "${YELLOW}Node ID: ${NODE_ID}${NC}"
echo -e "${YELLOW}VLESS Port: ${VLESS_PORT}${NC}"

# 安装目录
INSTALL_DIR="/opt/otun-agent"
mkdir -p $INSTALL_DIR
cd $INSTALL_DIR

# 下载预编译的 sing-box
echo -e "${GREEN}Downloading sing-box...${NC}"
SINGBOX_URL="https://github.com/antsbtw/sing-box-docker/releases/download/v1.0.0/sing-box-linux-amd64.gz"
curl -fsSL $SINGBOX_URL -o sing-box.gz
gunzip -f sing-box.gz
chmod +x sing-box
mv sing-box /usr/local/bin/
setcap cap_net_bind_service=+ep /usr/local/bin/sing-box
echo -e "${GREEN}sing-box installed: $(sing-box version | head -1)${NC}"

# 安装 Go (如果需要编译 agent)
if ! command -v go &> /dev/null; then
    echo -e "${GREEN}Installing Go...${NC}"
    curl -fsSL https://go.dev/dl/go1.21.5.linux-amd64.tar.gz -o go.tar.gz
    rm -rf /usr/local/go
    tar -C /usr/local -xzf go.tar.gz
    rm go.tar.gz
    export PATH=$PATH:/usr/local/go/bin
    echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile
fi
export PATH=$PATH:/usr/local/go/bin

# 克隆或更新代码
echo -e "${GREEN}Downloading OTun Node Agent...${NC}"
if [ -d "repo" ]; then
    cd repo && git pull
else
    git clone https://github.com/antsbtw/sing-box-docker.git repo
    cd repo
fi

# 编译 agent
echo -e "${GREEN}Building agent...${NC}"
go build -o $INSTALL_DIR/agent ./cmd/agent

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
