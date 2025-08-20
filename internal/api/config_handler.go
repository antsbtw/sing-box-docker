package api

import (
	"net/http"

	"sing-box-manager/internal/service"

	"github.com/gin-gonic/gin"
)

// ConfigHandler 配置API处理器
type ConfigHandler struct {
	configService *service.ConfigService
}

// NewConfigHandler 创建配置处理器
func NewConfigHandler(configService *service.ConfigService) *ConfigHandler {
	return &ConfigHandler{
		configService: configService,
	}
}

// GenerateConfig 生成配置
// POST /api/config/generate
func (h *ConfigHandler) GenerateConfig(c *gin.Context) {
	if err := h.configService.GenerateConfig(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Configuration generated successfully",
	})
}

// ReloadConfig 重载配置
// POST /api/config/reload
func (h *ConfigHandler) ReloadConfig(c *gin.Context) {
	if err := h.configService.ReloadSingBox(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Configuration reloaded successfully",
	})
}

// RestartSingBox 重启sing-box
// POST /api/config/restart
func (h *ConfigHandler) RestartSingBox(c *gin.Context) {
	if err := h.configService.RestartSingBox(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "sing-box restarted successfully",
	})
}

// GetConfigStatus 获取配置状态
// GET /api/config/status
func (h *ConfigHandler) GetConfigStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":          "sing-box configuration service is running",
		"config_file":     "configs/sing-box.json",
		"auto_reload":     true,
		"reality_enabled": true,
	})
}

// GenerateRealityKeypair 生成Reality密钥对
// POST /api/config/reality/keypair
func (h *ConfigHandler) GenerateRealityKeypair(c *gin.Context) {
	keypair, err := h.configService.GenerateRealityKeypair()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Reality keypair generated successfully",
		"keypair": keypair,
	})
}

// RegisterRoutes 注册路由
func (h *ConfigHandler) RegisterRoutes(router *gin.Engine) {
	api := router.Group("/api")
	{
		config := api.Group("/config")
		{
			config.GET("/status", h.GetConfigStatus)
			config.POST("/generate", h.GenerateConfig)
			config.POST("/reload", h.ReloadConfig)
			config.POST("/restart", h.RestartSingBox)

			// Reality相关路由
			reality := config.Group("/reality")
			{
				reality.POST("/keypair", h.GenerateRealityKeypair)
			}
		}
	}
}
