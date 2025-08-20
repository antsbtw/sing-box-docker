#!/bin/bash

# 集成测试脚本
BASE_URL="http://localhost:8080"

echo "🚀 测试Sing-box集成管理系统"
echo "================================"

# 1. 健康检查
echo "1️⃣ 健康检查"
curl -s "$BASE_URL/health" | jq
echo -e "\n"

# 2. 配置状态检查
echo "2️⃣ 配置服务状态"
curl -s "$BASE_URL/api/config/status" | jq
echo -e "\n"

# 3. 创建用户
echo "3️⃣ 创建用户"
USER_DATA='{
  "username": "vipuser",
  "password": "vippass123",
  "expires_at": "2025-12-31T23:59:59Z",
  "traffic_limit": 53687091200,
  "device_limit": 5
}'

RESPONSE=$(curl -s -X POST "$BASE_URL/api/users" \
  -H "Content-Type: application/json" \
  -d "$USER_DATA")

echo "$RESPONSE" | jq
USER_ID=$(echo "$RESPONSE" | jq -r '.user.id')
echo "用户ID: $USER_ID"
echo -e "\n"

# 4. 生成sing-box配置
echo "4️⃣ 生成sing-box配置"
curl -s -X POST "$BASE_URL/api/config/generate" | jq
echo -e "\n"

# 5. 重载配置
echo "5️⃣ 重载sing-box配置"
curl -s -X POST "$BASE_URL/api/config/reload" | jq
echo -e "\n"

# 6. 获取用户统计
echo "6️⃣ 用户统计信息"
curl -s "$BASE_URL/api/users/$USER_ID/stats" | jq
echo -e "\n"

# 7. 连接设备测试
echo "7️⃣ 设备连接测试"
curl -s -X POST "$BASE_URL/api/users/$USER_ID/connect" \
  -H "Content-Type: application/json" \
  -d '{"device_id": "iphone-001"}' | jq
echo -e "\n"

# 8. 再次生成配置(应该包含新用户)
echo "8️⃣ 重新生成配置"
curl -s -X POST "$BASE_URL/api/config/generate" | jq
echo -e "\n"

# 9. 检查配置文件
echo "9️⃣ 检查生成的配置文件"
if docker exec sing-box-manager cat configs/sing-box.json > /dev/null 2>&1; then
    echo "✅ 配置文件存在"
    echo "用户配置:"
    docker exec sing-box-manager cat configs/sing-box.json | jq '.inbounds[].users' 2>/dev/null || echo "配置格式检查中..."
else
    echo "❌ 配置文件不存在"
fi
echo -e "\n"

echo "✅ 集成测试完成！"
echo "🔗 代理连接信息："
echo "   Trojan: your-domain.com:443 (password: vippass123)"
echo "   VLESS: your-domain.com:8443 (uuid: $USER_ID)"
echo "   Mixed Proxy: your-domain.com:1080"