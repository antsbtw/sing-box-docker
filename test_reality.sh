#!/bin/bash

# Reality协议测试脚本
BASE_URL="http://localhost:8080"

echo "🚀 测试Reality协议功能"
echo "================================"

# 1. 配置服务状态
echo "1️⃣ 检查Reality支持状态"
curl -s "$BASE_URL/api/config/status" | jq
echo -e "\n"

# 2. 生成Reality密钥对
echo "2️⃣ 生成Reality密钥对"
KEYPAIR_RESPONSE=$(curl -s -X POST "$BASE_URL/api/config/reality/keypair")
echo "$KEYPAIR_RESPONSE" | jq
PRIVATE_KEY=$(echo "$KEYPAIR_RESPONSE" | jq -r '.keypair.private_key')
PUBLIC_KEY=$(echo "$KEYPAIR_RESPONSE" | jq -r '.keypair.public_key')
echo "私钥: $PRIVATE_KEY"
echo "公钥: $PUBLIC_KEY"
echo -e "\n"

# 3. 创建Reality用户
echo "3️⃣ 创建Reality用户"
USER_DATA='{
  "username": "realityuser",
  "password": "reality123",
  "expires_at": "2025-12-31T23:59:59Z",
  "traffic_limit": 107374182400,
  "device_limit": 10
}'

RESPONSE=$(curl -s -X POST "$BASE_URL/api/users" \
  -H "Content-Type: application/json" \
  -d "$USER_DATA")

echo "$RESPONSE" | jq
USER_ID=$(echo "$RESPONSE" | jq -r '.user.id')
echo "Reality用户ID: $USER_ID"
echo -e "\n"

# 4. 重新生成配置(包含Reality)
echo "4️⃣ 生成包含Reality的配置"
curl -s -X POST "$BASE_URL/api/config/generate" | jq
echo -e "\n"

# 5. 重载配置
echo "5️⃣ 重载sing-box配置"
curl -s -X POST "$BASE_URL/api/config/reload" | jq
echo -e "\n"

# 6. 检查端口监听
echo "6️⃣ 检查Reality端口监听"
if docker exec sing-box-manager netstat -tlnp | grep :4433; then
    echo "✅ Reality端口4433正在监听"
else
    echo "❌ Reality端口4433未监听"
fi
echo -e "\n"

# 7. 检查生成的Reality配置
echo "7️⃣ 检查Reality配置"
if docker exec sing-box-manager cat configs/sing-box.json | jq '.inbounds[] | select(.tag == "vless-reality-in")' 2>/dev/null; then
    echo "✅ Reality配置已生成"
else
    echo "❌ Reality配置未找到"
fi
echo -e "\n"

# 8. 显示Reality连接信息
echo "8️⃣ Reality连接信息"
echo "================================"
echo "🔐 Reality连接配置:"
echo "   协议: VLESS"
echo "   地址: your-domain.com (替换为实际域名或IP)"
echo "   端口: 4433"
echo "   用户ID: $USER_ID"
echo "   伪装域名: www.google.com"
echo "   公钥: $PUBLIC_KEY"
echo "   短ID: 0123456789abcdef"
echo ""
echo "📱 客户端配置示例:"
echo "{"
echo "  \"protocol\": \"vless\","
echo "  \"settings\": {"
echo "    \"vnext\": [{"
echo "      \"address\": \"your-domain.com\","
echo "      \"port\": 4433,"
echo "      \"users\": [{"
echo "        \"id\": \"$USER_ID\","
echo "        \"encryption\": \"none\""
echo "      }]"
echo "    }]"
echo "  },"
echo "  \"streamSettings\": {"
echo "    \"network\": \"tcp\","
echo "    \"security\": \"reality\","
echo "    \"realitySettings\": {"
echo "      \"serverName\": \"www.google.com\","
echo "      \"publicKey\": \"$PUBLIC_KEY\","
echo "      \"shortId\": \"0123456789abcdef\""
echo "    }"
echo "  }"
echo "}"
echo ""
echo "✅ Reality协议测试完成！"
echo "🎯 Reality是最先进的流量伪装技术，几乎无法被检测和封锁！"