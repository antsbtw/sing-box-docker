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

# 清理之前先把已有的本机 API 密钥抢救出来。
#
# ⚠️ 这个 key 是「机器身份」的一部分，不该每次重装都换。
# App 装 sing-box 时会把它存进本地节点记录，之后靠它调
# /api/local/* 管理用户。重装 agent 如果重新生成一个，
# App 手里那份就失效了 —— 表现为「创建用户提示认证失败」，
# 而用户完全看不出是重装把 key 换掉了（2026-09-23 真机踩到）。
#
# 下面 H-2 那段只在 PRESERVED_API_KEY 为空时才生成新的。
PRESERVED_API_KEY=""
if [ -f /etc/systemd/system/otun-agent.service ]; then
    # 用 sed 而不是 grep -oP：精简镜像上未必有 PCRE 支持的 grep
    PRESERVED_API_KEY=$(sed -n 's/.*NODE_API_KEY=\([^"]*\)".*/\1/p' \
        /etc/systemd/system/otun-agent.service 2>/dev/null | head -1)
    if [ -n "$PRESERVED_API_KEY" ]; then
        echo -e "${GREEN}保留已有的本机 API 密钥（App 侧记录仍然有效）${NC}"
    fi
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
apt-get install -y -qq curl

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
        --enroll-token) ENROLL_TOKEN="$2"; shift 2 ;;
        --node-id) NODE_ID="$2"; shift 2 ;;
        --api-url) API_URL="$2"; shift 2 ;;
        --vless-port) VLESS_PORT="$2"; shift 2 ;;
        --management-mode) MANAGEMENT_MODE="$2"; shift 2 ;;
        --server-ip) SERVER_IP="$2"; shift 2 ;;
        --with-singbox) WITH_SINGBOX=1; shift ;;
        --no-singbox) WITH_SINGBOX=0; shift ;;
        --skip-checksum) SKIP_CHECKSUM=1; shift ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

# 两种接入方式二选一：
#   --api-key      旧路径。App 经 SSH 直接安装，密钥由 App 生成，后端无记录。
#                  存量节点全靠这条，**不可移除**。
#   --enroll-token 新路径。用户从官网取一次性 token，agent 注册后主动外连。
if [ -n "$ENROLL_TOKEN" ] && [ -n "$NODE_API_KEY" ]; then
    echo -e "${RED}Error: --enroll-token 与 --api-key 只能二选一${NC}"
    exit 1
fi

if [ -z "$NODE_API_KEY" ] && [ -z "$ENROLL_TOKEN" ]; then
    echo -e "${RED}Error: 需要 --api-key 或 --enroll-token${NC}"
    echo "Usage: $0 --enroll-token <token> --api-url <url>"
    echo "   or: $0 --api-key <key> [--node-id <id>] [--vless-port <port>] [--management-mode local|remote|hybrid] [--server-ip <ip>]"
    exit 1
fi

# H-2（契约 §2.2）：Token 模式下也必须有 NODE_API_KEY。
# agent 的 main.go 在它为空时 log.Fatal，systemd 每 5s 重启一次，
# 永远注册不上 —— 这条路径此前是走不通的。
# 该密钥仅用于本机 /api/local/* 鉴权，不上报后端。
if [ -n "$ENROLL_TOKEN" ] && [ -z "$NODE_API_KEY" ]; then
    # 优先复用重装前那把 —— App 本地记录里存的就是它
    if [ -n "$PRESERVED_API_KEY" ]; then
        NODE_API_KEY="$PRESERVED_API_KEY"
    else
        NODE_API_KEY=$(openssl rand -hex 16 2>/dev/null || head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
    fi
    if [ -z "$NODE_API_KEY" ]; then
        echo -e "${RED}无法生成本地 API 密钥（需要 openssl 或 /dev/urandom）${NC}"
        exit 1
    fi
fi

# H-3（契约 §2.4 第 0 步）：带 token 重装 = 总是重新注册。
# 否则残留的 node.json 会让 agent 优先用旧 secret：
# 用户在 App 里删过节点 → secret 已吊销 → 轮询 401 → agent 删 node.json 退出，
# 而 install.sh 早因为「node.json 存在」把 token 行删了 —— 既无 token 又无 secret，卡死。
# 后端按 machine_id 去重并轮换 secret，重复注册不会产生僵尸条目。
#
# ⚠️ 必须放在参数解析之后：放在前面的清理段里时 ENROLL_TOKEN 尚未赋值，判断恒假。
if [ -n "$ENROLL_TOKEN" ]; then
    rm -f /opt/otun-agent/data/node.json 2>/dev/null || true
fi

# ─────────────────────────────────────────────────────────────
# 是否安装 sing-box
#
# agent 的职责是「把这台机器接到 App 上」,sing-box 只是接上之后
# 装的第一个应用 —— 用户也可能想装别的,或者先不装。
#
# 默认值按接入方式分开,避免改坏存量:
#   --api-key    存量路径,App 经 SSH 装完就指望 sing-box 在跑 → 默认装
#   --enroll-token 新路径,装完由 App 经隧道按需安装 → 默认不装
# 两条路径都可以用 --with-singbox / --no-singbox 显式覆盖。
# ─────────────────────────────────────────────────────────────
if [ -z "$WITH_SINGBOX" ]; then
    if [ -n "$ENROLL_TOKEN" ]; then
        WITH_SINGBOX=0
    else
        WITH_SINGBOX=1
    fi
fi

echo -e "${YELLOW}Node ID: ${NODE_ID}${NC}"
echo -e "${YELLOW}VLESS Port: ${VLESS_PORT}${NC}"
echo -e "${YELLOW}Management Mode: ${MANAGEMENT_MODE}${NC}"

# ─────────────────────────────────────────────────────────────
# 版本与完整性校验
#
# ⚠️ RELEASE_TAG 由 CI 在发版时替换为实际 tag。
# 三件二进制全部取自同一个 release —— 此前 agent 走浮动 latest、
# sing-box 钉版本，两者可能来自不同时间的构建，2026-08-31 事故即源于此。
# ─────────────────────────────────────────────────────────────
RELEASE_TAG="${RELEASE_TAG:-__RELEASE_TAG__}"
REPO="antsbtw/sing-box-docker"
RELEASE_BASE="https://github.com/${REPO}/releases/download/${RELEASE_TAG}"

# 下载并校验。校验失败即退出 —— 宁可装不上，也不装一个来路不明的二进制。
download_verified() {
    local name="$1" dest="$2"
    echo -e "${YELLOW}Downloading ${name}...${NC}"
    if ! curl -fsSL "${RELEASE_BASE}/${name}" -o "$dest"; then
        echo -e "${RED}✗ 下载失败${NC}"
        echo -e "${RED}  地址: ${RELEASE_BASE}/${name}${NC}"
        echo -e "${YELLOW}  可能原因:${NC}"
        echo -e "${YELLOW}    1. 这台服务器访问不了 GitHub —— 检查出网与 DNS${NC}"
        echo -e "${YELLOW}       curl -I https://github.com${NC}"
        echo -e "${YELLOW}    2. release ${RELEASE_TAG} 不存在或资产缺失${NC}"
        echo -e "${YELLOW}  网络恢复后重跑本脚本即可，不会留下半成品。${NC}"
        exit 1
    fi
    if [ "${SKIP_CHECKSUM:-0}" = "1" ]; then
        echo -e "${YELLOW}⚠️  已跳过校验和验证（--skip-checksum）${NC}"
        return 0
    fi
    local want
    want=$(grep " ${name}\$" "$SHA256SUMS_FILE" | awk '{print $1}')
    if [ -z "$want" ]; then
        echo -e "${RED}SHA256SUMS 中找不到 ${name} 的校验和${NC}"
        exit 1
    fi
    local got
    got=$(sha256sum "$dest" | awk '{print $1}')
    if [ "$want" != "$got" ]; then
        echo -e "${RED}校验和不匹配: ${name}${NC}"
        echo -e "${RED}  期望: ${want}${NC}"
        echo -e "${RED}  实际: ${got}${NC}"
        exit 1
    fi
    echo -e "${GREEN}✓ ${name} 校验通过${NC}"
}

SHA256SUMS_FILE="/tmp/otun-SHA256SUMS.$$"
trap 'rm -f "$SHA256SUMS_FILE"' EXIT

if [ "${SKIP_CHECKSUM:-0}" != "1" ]; then
    if ! curl -fsSL "${RELEASE_BASE}/SHA256SUMS" -o "$SHA256SUMS_FILE"; then
        echo -e "${RED}✗ 无法下载校验和文件 SHA256SUMS${NC}"
        echo -e "${RED}  地址: ${RELEASE_BASE}/SHA256SUMS${NC}"
        echo -e "${YELLOW}  这台服务器可能访问不了 GitHub。先确认出网:${NC}"
        echo -e "${YELLOW}    curl -I https://github.com${NC}"
        echo -e "${YELLOW}  确需跳过校验（不推荐）可加 --skip-checksum${NC}"
        exit 1
    fi
fi

# 安装目录
INSTALL_DIR="/opt/otun-agent"
mkdir -p $INSTALL_DIR
cd $INSTALL_DIR

# 架构判定
#
# S-1：原先这里会下载安装 Go 1.23.4（70 MB），只为源码编译回退用。
# 回退分支已移除，Go 不再有任何使用者 —— 它却会 rm -rf /usr/local/go
# 抹掉用户自己装的 Go、改 /etc/profile、每次装机多 30 秒和一个 go.dev 依赖。
# 已删除，仅保留下面的架构判定（后续两处下载都依赖 ARCH）。
ARCH=$(uname -m)
case $ARCH in
    x86_64|aarch64) ;;
    *) echo -e "${RED}不支持的架构: $ARCH（仅支持 x86_64 / aarch64）${NC}"; exit 1 ;;
esac

# 下载 sing-box（本 release 内的官方源码构建版）
#
# 不再提供"下载失败就从源码编译"的回退：那条路径绕过校验和，
# 且编出来的二进制与 release 内的不是同一个，失去可复现性。
# 下载或校验失败一律退出。
if [ "$WITH_SINGBOX" = "1" ]; then
    case $ARCH in
        x86_64) SINGBOX_ARCH="amd64" ;;
        aarch64) SINGBOX_ARCH="arm64" ;;
    esac

    download_verified "sing-box-linux-${SINGBOX_ARCH}" /usr/local/bin/sing-box

    chmod +x /usr/local/bin/sing-box
    setcap cap_net_bind_service=+ep /usr/local/bin/sing-box

    # 验证安装
    if ! sing-box version > /dev/null 2>&1; then
        echo -e "${RED}sing-box installation verification failed${NC}"
        exit 1
    fi
    echo -e "${GREEN}sing-box installed: $(sing-box version | head -1)${NC}"
else
    echo -e "${YELLOW}跳过 sing-box 安装（接入完成后可从 App 按需安装）${NC}"
fi

cd $INSTALL_DIR

# 下载 agent（同一 release，与 sing-box 必然同源）
case $ARCH in
    x86_64) AGENT_ARCH="amd64" ;;
    aarch64) AGENT_ARCH="arm64" ;;
esac

download_verified "agent-linux-${AGENT_ARCH}" "$INSTALL_DIR/agent"
chmod +x "$INSTALL_DIR/agent"



# 创建数据目录
mkdir -p $INSTALL_DIR/data

if [ "$WITH_SINGBOX" = "1" ]; then
    mkdir -p /etc/sing-box
    # 创建初始配置
    cat > /etc/sing-box/config.json << 'CONF'
{
  "log": {"level": "info", "timestamp": true},
  "inbounds": [],
  "outbounds": [{"type": "direct", "tag": "direct"}]
}
CONF
fi

# 未安装 sing-box 时,agent 不要去拉一个不存在的二进制 ——
# 否则每次启动都报错,且 /health 永远 503,装机闸口会误判为失败。
if [ "$WITH_SINGBOX" = "1" ]; then
    SKIP_SINGBOX_ENV="false"
else
    SKIP_SINGBOX_ENV="true"
fi

# 创建 systemd 服务
cat > /etc/systemd/system/otun-agent.service << SYSTEMD
[Unit]
Description=OTun Node Agent
After=network.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
Environment="NODE_API_KEY=$NODE_API_KEY"
Environment="OTUN_ENROLL_TOKEN=$ENROLL_TOKEN"
Environment="NODE_ID=$NODE_ID"
Environment="VLESS_PORT=$VLESS_PORT"
Environment="OTUN_API_URL=$API_URL"
Environment="MANAGEMENT_MODE=$MANAGEMENT_MODE"
Environment="SERVER_IP=$SERVER_IP"
Environment="SKIP_SINGBOX=$SKIP_SINGBOX_ENV"
ExecStart=$INSTALL_DIR/agent
Restart=always
RestartSec=5
# H-4（契约 §5.3）：收到 node_revoked 后 agent 以退出码 3 结束，
# systemd 不得再拉起 —— 否则会每 5 秒去打一个已吊销的节点。
RestartPreventExitStatus=3

[Install]
WantedBy=multi-user.target
SYSTEMD

# 启动服务
systemctl daemon-reload
systemctl enable otun-agent
systemctl start otun-agent

# ⚠️ token 只用于首次注册：agent 注册成功后写入 node.json 并持久化
# node_secret，此后不再需要 token。这里从 systemd 单元里移除它，
# 避免一次性凭据长期留在磁盘上（systemd 单元是 0644，任何用户可读）。
if [ -n "$ENROLL_TOKEN" ]; then
    echo -e "${YELLOW}等待节点注册...${NC}"
    for _ in $(seq 1 30); do
        if [ -f "$INSTALL_DIR/data/node.json" ]; then
            sed -i '/OTUN_ENROLL_TOKEN/d' /etc/systemd/system/otun-agent.service
            systemctl daemon-reload
            echo -e "${GREEN}✓ 注册成功，已清除一次性 token${NC}"
            break
        fi
        sleep 2
    done
    if [ ! -f "$INSTALL_DIR/data/node.json" ]; then
        # H-1（契约 §2.4 第 4 条）：必须 exit 1。
        # 此前只打一行警告就继续往下跑健康闸口，而 sing-box 空配置也能起来、
        # /health 返回 200，脚本末尾照样打印「Installation Complete!」——
        # token 过期或 agent_too_old 时用户看到"成功"，App 里却永远没有节点。
        echo ""
        echo -e "${RED}✗ 节点注册失败（60 秒内未完成）${NC}"
        echo ""
        echo -e "${YELLOW}--- agent 日志 ---${NC}"
        journalctl -u otun-agent -n 30 --no-pager 2>/dev/null || echo "(无日志)"
        echo ""
        echo -e "${YELLOW}常见原因:${NC}"
        echo -e "${YELLOW}  · token 已过期（有效期 15 分钟）—— 回 App 重新获取安装命令${NC}"
        echo -e "${YELLOW}  · token 已被使用过 —— 每个 token 只能用一次${NC}"
        echo -e "${YELLOW}  · 这台服务器访问不了 ${API_URL}${NC}"
        echo ""
        exit 1
    fi
fi

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

# ─────────────────────────────────────────────────────────────
# 数据面闸口(接线契约 §2.5、实施单 §7.7)
#
# ⚠️ 这是第一道闸口。前面的 `sing-box version` 只验二进制可执行,
# 验不出服务能不能起来 —— 配置错误、端口被占、权限不足都会在这里暴露。
# 没有这一道,用户会拿到一个"安装成功"但一个客户端都连不上的节点。
#
# /health 语义:200 = agent(以及装了的话 sing-box)在跑;503 = agent 在、sing-box 没起来。
# 未安装 sing-box 时 unit 里带 SKIP_SINGBOX=true,agent 直接回 200。
# 托管路径上 hosting-service 的 SSH 检查是第二道,两道都要。
# ─────────────────────────────────────────────────────────────
echo ""
echo -e "${YELLOW}检查服务健康状态...${NC}"
HEALTH_OK=0
for i in $(seq 1 6); do
    # curl 失败时 -w 仍会输出 000，与 || echo 叠加会变成 000000，故取末三位
    CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 http://localhost:8080/health 2>/dev/null)
    CODE="${CODE:-000}"; CODE="${CODE: -3}"
    if [ "$CODE" = "200" ]; then
        HEALTH_OK=1
        echo -e "${GREEN}✓ 服务已就绪${NC}"
        break
    fi
    [ $i -lt 6 ] && sleep 5
done

if [ "$HEALTH_OK" != "1" ]; then
    echo -e "${RED}✗ 服务未能在 30 秒内就绪(最后状态码: ${CODE})${NC}"
    echo ""
    # sing-box 由 agent 作为子进程拉起，没有独立的 systemd unit，
    # 它的输出在 agent 日志里（S-2）。
    echo -e "${YELLOW}--- agent 与 sing-box 日志 ---${NC}"
    journalctl -u otun-agent -n 40 --no-pager 2>/dev/null || echo "(无日志)"
    echo ""
    echo -e "${RED}安装未完成。排查后可重跑本脚本。${NC}"
    exit 1
fi

echo ""
echo -e "${GREEN}========================================${NC}"
echo -e "${GREEN}  Installation Complete!${NC}"
echo -e "${GREEN}========================================${NC}"
echo ""
echo -e "Node ID: ${YELLOW}$NODE_ID${NC}"
if [ "$WITH_SINGBOX" = "1" ]; then
    echo -e "Config:  ${YELLOW}/etc/sing-box/config.json${NC}"
fi
echo -e "Data:    ${YELLOW}$INSTALL_DIR/data${NC}"
echo ""
echo -e "Commands:"
echo -e "  ${YELLOW}otun status${NC}  - Check service status"
echo -e "  ${YELLOW}otun logs${NC}    - View logs"
echo -e "  ${YELLOW}otun restart${NC} - Restart service"
echo ""

if [ "$WITH_SINGBOX" = "1" ]; then
    echo -e "${GREEN}Secrets generated:${NC}"
    cat $INSTALL_DIR/data/secrets.json 2>/dev/null || echo "Will be generated on first run"
else
    echo -e "${GREEN}这台机器已接入,可以在 App 里管理了。${NC}"
    echo -e "需要 VPN 服务时,从 App 上安装即可 —— 也可以装别的软件。"
fi
