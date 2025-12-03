# OTun Node Agent

轻量级 sing-box VPN 节点管理代理，支持 VLESS + Reality 和 Shadowsocks 协议。

## 功能特性

- ✅ 从管理服务器自动同步用户配置（每60秒）
- ✅ 动态生成 sing-box 配置
- ✅ 本地流量限额检测（实时）
- ✅ 用户过期时间检测（实时）
- ✅ 流量统计上报（每5分钟）
- ✅ 离线容错（使用缓存配置）
- ✅ Reality 密钥自动生成
- ✅ 进程守护（崩溃自动重启）
- ✅ 健康检查接口

## 快速部署
```bash
curl -fsSL https://your-domain/install.sh | bash -s -- \
  --api-key YOUR_API_KEY \
  --node-id node-tokyo-01
```

## 手动部署

1. 克隆仓库
2. 配置 `.env` 文件
3. 运行 `docker compose up -d`

## 环境变量

| 变量 | 必填 | 默认值 | 说明 |
|------|------|--------|------|
| NODE_API_KEY | ✅ | - | 节点 API 密钥 |
| NODE_ID | - | node-default | 节点标识 |
| OTUN_API_URL | - | https://saasapi.situstechnologies.com | 管理服务器地址 |
| VLESS_PORT | - | 443 | VLESS 端口 |
| SS_PORT | - | 8388 | Shadowsocks 端口 |
| SYNC_INTERVAL | - | 60 | 配置同步间隔（秒） |
| STATS_INTERVAL | - | 300 | 统计上报间隔（秒） |

## 管理命令
```bash
otun start    # 启动
otun stop     # 停止
otun restart  # 重启
otun logs     # 查看日志
otun status   # 检查状态
otun update   # 更新版本
```

## API 接口

- `GET /health` - 健康检查
- `GET /ready` - 就绪检查

## 目录结构
```
/opt/otun-agent/
├── docker-compose.yml
├── .env
├── data/
│   ├── keys.json      # Reality 密钥对
│   ├── users.json     # 用户配置缓存
│   └── stats/         # 统计缓存
└── singbox/
    └── config.json    # sing-box 配置
```

## 开发
```bash
# 本地测试
go build -o bin/agent ./cmd/agent
SKIP_SINGBOX=true NODE_API_KEY=test ./bin/agent

# Docker 构建
docker build -t otun-node-agent .
```

## License

MIT
