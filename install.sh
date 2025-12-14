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

# 安装 Go (用于编译 sing-box 和 agent)
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

# 从源码编译 sing-box (启用 v2ray_api 支持流量统计)
echo -e "${GREEN}Building sing-box from source with v2ray_api support...${NC}"
echo -e "${YELLOW}This may take a few minutes...${NC}"

# 获取最新稳定版本号
SINGBOX_VERSION=$(curl -s https://api.github.com/repos/SagerNet/sing-box/releases/latest | grep '"tag_name":' | sed -E 's/.*"v([^"]+)".*/\1/')
if [ -z "$SINGBOX_VERSION" ]; then
    SINGBOX_VERSION="1.10.0"  # 备用版本
fi
echo -e "${YELLOW}sing-box version: v${SINGBOX_VERSION}${NC}"

# 下载并编译 sing-box
cd /tmp
rm -rf sing-box-src
git clone --depth 1 --branch "v${SINGBOX_VERSION}" https://github.com/SagerNet/sing-box.git sing-box-src
cd sing-box-src

# 使用多个 tags 编译，启用所需功能：
# - with_v2ray_api: 流量统计
# - with_utls: Reality 协议所需的 uTLS
# - with_reality_server: Reality 服务端支持
go build -tags "with_v2ray_api,with_utls,with_reality_server" -o sing-box ./cmd/sing-box

# 安装编译好的 sing-box
mv sing-box /usr/local/bin/
setcap cap_net_bind_service=+ep /usr/local/bin/sing-box

# 清理源码
cd /tmp
rm -rf sing-box-src

# 验证安装
echo -e "${GREEN}sing-box installed: $(sing-box version | head -1)${NC}"

cd $INSTALL_DIR

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
