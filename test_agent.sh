#!/bin/bash

# 设置测试环境变量
export NODE_API_KEY="test-api-key-12345"
export NODE_ID="node-test-01"
export OTUN_API_URL="http://localhost:9999"  # 假的服务器地址
export SINGBOX_CONFIG="/tmp/singbox-test/config.json"
export SKIP_SINGBOX="true"  # 跳过实际启动 sing-box

echo "Starting agent in test mode..."
./bin/agent
