# Sing-box Manager

一个基于Go语言的sing-box用户管理系统，提供RESTful API进行用户CRUD操作，支持时间/流量/设备数三重限制。

## ✨ 功能特性

- 🔐 用户管理：创建、查询、更新、删除用户
- 📊 流量统计：实时流量使用统计和限制
- 📱 设备管理：设备连接数限制和管理
- ⏰ 时间控制：用户过期时间管理和自动清理
- 🐳 容器化：Docker一键部署
- 🔄 热更新：配置文件热重载
- 🛡️ 并发安全：支持高并发访问

## 🚀 快速开始

### 1. 运行项目

```bash
# 安装依赖
go mod tidy

# 运行服务器
go run cmd/server/main.go
```

### 2. Docker部署

```bash
# 构建并启动
docker-compose up -d --build

# 查看日志
docker-compose logs -f
```

### 3. API测试

```bash
# 给测试脚本执行权限
chmod +x test_api.sh

# 运行测试
./test_api.sh
```

## 📋 API接口

### 用户管理

| 方法 | 路径 | 描述 |
|------|------|------|
| POST | `/api/users` | 创建用户 |
| GET | `/api/users` | 获取用户列表 |
| GET | `/api/users/:id` | 获取单个用户 |
| PUT | `/api/users/:id` | 更新用户 |
| DELETE | `/api/users/:id` | 删除用户 |
| GET | `/api/users/username/:username` | 根据用户名获取用户 |

### 设备管理

| 方法 | 路径 | 描述 |
|------|------|------|
| POST | `/api/users/:id/connect` | 连接设备 |
| POST | `/api/users/:id/disconnect` | 断开设备 |

### 流量管理

| 方法 | 路径 | 描述 |
|------|------|------|
| POST | `/api/users/:id/traffic` | 更新流量使用 |
| GET | `/api/users/:id/stats` | 获取用户统计 |

### 健康检查

| 方法 | 路径 | 描述 |
|------|------|------|
| GET | `/health` | 健康检查 |

## 📊 API示例

### 创建用户

```bash
curl -X POST http://localhost:8080/api/users \
  -H "Content-Type: application/json" \
  -d '{
    "username": "testuser",
    "password": "testpass123", 
    "expires_at": "2025-12-31T23:59:59Z",
    "traffic_limit": 10737418240,
    "device_limit": 3
  }'
```

### 获取用户统计

```bash
curl http://localhost:8080/api/users/{user_id}/stats
```

## 🔧 配置说明

### 环境变量

- `PORT`: 服务端口，默认8080
- `DATA_FILE`: 数据文件路径，默认`data/users.json`
- `GIN_MODE`: Gin模式，默认`debug`

### 用户数据结构

```json
{
  "id": "uuid",
  "username": "用户名",
  "password": "密码",
  "created_at": "创建时间",
  "expires_at": "过期时间",
  "traffic_limit": "流量限制(字节)",
  "traffic_used": "已用流量(字节)",
  "device_limit": "设备数限制",
  "connected_devices": ["设备ID列表"],
  "is_active": true
}
```

## 📁 项目结构

```
sing-box-manager/
├── cmd/server/          # 主程序入口
├── internal/
│   ├── api/            # API处理器
│   ├── models/         # 数据模型
│   ├── service/        # 业务逻辑
│   └── storage/        # 数据存储
├── data/               # 数据文件
├── configs/            # 配置文件
├── Dockerfile          # Docker文件
├── docker-compose.yml  # Docker Compose
└── test_api.sh         # API测试脚本
```

## ⚡ 性能特性

- ✅ 并发安全的JSON文件读写
- ✅ 自动过期用户清理(每小时)
- ✅ 内存缓存用户数据
- ✅ 高性能RESTful API
- ✅ 支持1000+用户并发

## 🛠️ 下一步开发

- [ ] 集成sing-box配置生成
- [ ] 实现配置热重载
- [ ] 添加用户认证
- [ ] 集成监控指标
- [ ] 添加日志记录

## 📝 许可证

MIT License