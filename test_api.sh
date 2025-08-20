#!/bin/bash

# API测试脚本
BASE_URL="http://localhost:8080"

echo "🚀 开始测试Sing-box Manager API"
echo "================================"

# 1. 健康检查
echo "1️⃣ 健康检查"
curl -s "$BASE_URL/health" | jq
echo -e "\n"

# 2. 创建用户
echo "2️⃣ 创建用户"
USER_DATA='{
  "username": "testuser",
  "password": "testpass123",
  "expires_at": "2025-12-31T23:59:59Z",
  "traffic_limit": 10737418240,
  "device_limit": 3
}'

RESPONSE=$(curl -s -X POST "$BASE_URL/api/users" \
  -H "Content-Type: application/json" \
  -d "$USER_DATA")

echo "$RESPONSE" | jq
USER_ID=$(echo "$RESPONSE" | jq -r '.user.id')
echo "用户ID: $USER_ID"
echo -e "\n"

# 3. 获取用户列表
echo "3️⃣ 获取用户列表"
curl -s "$BASE_URL/api/users" | jq
echo -e "\n"

# 4. 获取单个用户
echo "4️⃣ 获取单个用户"
curl -s "$BASE_URL/api/users/$USER_ID" | jq
echo -e "\n"

# 5. 连接设备
echo "5️⃣ 连接设备"
curl -s -X POST "$BASE_URL/api/users/$USER_ID/connect" \
  -H "Content-Type: application/json" \
  -d '{"device_id": "device-001"}' | jq
echo -e "\n"

# 6. 更新流量使用
echo "6️⃣ 更新流量使用"
curl -s -X POST "$BASE_URL/api/users/$USER_ID/traffic" \
  -H "Content-Type: application/json" \
  -d '{"bytes_used": 1048576}' | jq
echo -e "\n"

# 7. 获取用户统计
echo "7️⃣ 获取用户统计"
curl -s "$BASE_URL/api/users/$USER_ID/stats" | jq
echo -e "\n"

# 8. 更新用户
echo "8️⃣ 更新用户"
curl -s -X PUT "$BASE_URL/api/users/$USER_ID" \
  -H "Content-Type: application/json" \
  -d '{"traffic_limit": 21474836480}' | jq
echo -e "\n"

# 9. 根据用户名获取用户
echo "9️⃣ 根据用户名获取用户"
curl -s "$BASE_URL/api/users/username/testuser" | jq
echo -e "\n"

echo "✅ API测试完成！"