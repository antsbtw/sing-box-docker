# 多阶段构建
FROM golang:1.25-alpine AS builder

# 设置工作目录
WORKDIR /app

# 安装依赖
COPY go.mod go.sum ./
RUN go mod download

# 复制源代码
COPY . .

# 构建应用
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main cmd/server/main.go

# 运行阶段
FROM alpine:latest

# 安装必要的包
RUN apk --no-cache add ca-certificates tzdata curl wget unzip openssl

# 设置时区
ENV TZ=Asia/Shanghai

WORKDIR /root/

# 安装sing-box (使用正确版本号)
RUN ARCH=$(uname -m) && \
    if [ "$ARCH" = "x86_64" ]; then ARCH="amd64"; elif [ "$ARCH" = "aarch64" ]; then ARCH="arm64"; fi && \
    wget -O sing-box.tar.gz "https://github.com/SagerNet/sing-box/releases/download/v1.11.15/sing-box-1.11.15-linux-$ARCH.tar.gz" && \
    tar -xzf sing-box.tar.gz && \
    mv sing-box-*/sing-box /usr/local/bin/ && \
    chmod +x /usr/local/bin/sing-box && \
    rm -rf sing-box*

# 从构建阶段复制二进制文件
COPY --from=builder /app/main .

# 复制配置文件
COPY configs/ ./configs/

# 创建目录
RUN mkdir -p data logs

# 创建启动脚本
RUN cat > start.sh << 'SCRIPT'
#!/bin/sh

# 生成自签名证书 (仅用于测试)
if [ ! -f "/root/configs/cert.pem" ]; then
    echo "Generating self-signed certificate..."
    openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
        -keyout /root/configs/key.pem \
        -out /root/configs/cert.pem \
        -subj "/C=US/ST=State/L=City/O=Organization/CN=${SERVER_NAME:-example.com}"
fi

echo "Starting sing-box manager..."
./main &
MANAGER_PID=$!

# 等待管理器启动
sleep 5

echo "Starting sing-box server..."
sing-box run -c configs/sing-box.json &
SINGBOX_PID=$!

# 等待任一进程退出
wait $MANAGER_PID $SINGBOX_PID
SCRIPT

RUN chmod +x start.sh

# 暴露端口
EXPOSE 8080 443 1080 8443 4433

# 设置环境变量
ENV GIN_MODE=release
ENV PORT=8080
ENV DATA_FILE=data/users.json
ENV SINGBOX_CONFIG=configs/sing-box.json
ENV SINGBOX_TEMPLATE=configs/sing-box-template.json
ENV SERVER_NAME=example.com

# 启动脚本
CMD ["./start.sh"]
