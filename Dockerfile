# 构建阶段
FROM golang:1.21-alpine AS builder

WORKDIR /app

# 安装构建依赖
RUN apk add --no-cache git

# 复制 go.mod 和 go.sum
COPY go.mod go.sum ./
RUN go mod download

# 复制源码
COPY . .

# 构建
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /agent ./cmd/agent

# 运行阶段
FROM alpine:3.19

# 安装运行时依赖
RUN apk add --no-cache ca-certificates tzdata curl

# 安装 sing-box
ARG SINGBOX_VERSION=1.10.0
RUN set -ex \
    && ARCH=$(uname -m) \
    && case "$ARCH" in \
        x86_64) ARCH="amd64" ;; \
        aarch64) ARCH="arm64" ;; \
    esac \
    && curl -Lo /tmp/sing-box.tar.gz \
        "https://github.com/SagerNet/sing-box/releases/download/v${SINGBOX_VERSION}/sing-box-${SINGBOX_VERSION}-linux-${ARCH}.tar.gz" \
    && tar -xzf /tmp/sing-box.tar.gz -C /tmp \
    && mv /tmp/sing-box-*/sing-box /usr/local/bin/ \
    && chmod +x /usr/local/bin/sing-box \
    && rm -rf /tmp/*

# 创建工作目录
WORKDIR /app
RUN mkdir -p /app/data /etc/sing-box

# 复制 agent
COPY --from=builder /agent /app/agent

# 环境变量默认值
ENV OTUN_API_URL=https://saasapi.situstechnologies.com \
    NODE_API_KEY="" \
    NODE_ID="node-default" \
    SYNC_INTERVAL=60 \
    STATS_INTERVAL=300 \
    VLESS_PORT=443 \
    SS_PORT=8388 \
    SINGBOX_BIN=/usr/local/bin/sing-box \
    SINGBOX_CONFIG=/etc/sing-box/config.json \
    LOG_LEVEL=info

# 健康检查
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8080/health || exit 1

# 暴露端口
EXPOSE 443/tcp 443/udp 8388/tcp 8388/udp 8080/tcp

# 启动
CMD ["/app/agent"]
