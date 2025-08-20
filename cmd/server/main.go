package main

import (
	"log"
	"os"

	"sing-box-manager/internal/api"
	"sing-box-manager/internal/service"
	"sing-box-manager/internal/storage"

	"github.com/gin-gonic/gin"
)

func main() {
	// 获取配置
	port := getEnv("PORT", "8080")
	dataFile := getEnv("DATA_FILE", "data/users.json")
	configPath := getEnv("SINGBOX_CONFIG", "configs/sing-box.json")
	templatePath := getEnv("SINGBOX_TEMPLATE", "configs/sing-box-template.json")
	serverName := getEnv("SERVER_NAME", "example.com")
	
	// 初始化存储
	jsonStorage := storage.NewJSONStorage(dataFile)
	
	// 初始化服务
	userService := service.NewUserService(jsonStorage)
	configService := service.NewConfigService(jsonStorage, configPath, templatePath, serverName)
	
	// 生成初始配置
	if err := configService.GenerateConfig(); err != nil {
		log.Printf("Warning: Failed to generate initial config: %v", err)
	}
	
	// 启动配置自动重载
	go configService.AutoReloadConfig()
	
	// 初始化API处理器
	userHandler := api.NewUserHandler(userService)
	configHandler := api.NewConfigHandler(configService)
	
	// 设置Gin模式
	if getEnv("GIN_MODE", "debug") == "release" {
		gin.SetMode(gin.ReleaseMode)
	}
	
	// 创建路由
	router := gin.Default()
	
	// 添加中间件
	router.Use(corsMiddleware())
	router.Use(gin.Logger())
	router.Use(gin.Recovery())
	
	// 健康检查端点
	router.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"status": "ok",
			"service": "sing-box-manager",
		})
	})
	
	// 注册用户路由
	userHandler.RegisterRoutes(router)
	
	// 注册配置路由
	configHandler.RegisterRoutes(router)
	
	// 启动服务器
	log.Printf("Starting server on port %s", port)
	if err := router.Run(":" + port); err != nil {
		log.Fatal("Failed to start server:", err)
	}
}

// getEnv 获取环境变量，如果不存在则使用默认值
func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// corsMiddleware CORS中间件
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, Authorization")
		
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		
		c.Next()
	}
}
