package api

import (
	"net/http"

	"sing-box-manager/internal/models"
	"sing-box-manager/internal/service"

	"github.com/gin-gonic/gin"
)

// UserHandler 用户API处理器
type UserHandler struct {
	userService *service.UserService
}

// NewUserHandler 创建用户处理器
func NewUserHandler(userService *service.UserService) *UserHandler {
	return &UserHandler{
		userService: userService,
	}
}

// CreateUser 创建用户
// POST /api/users
func (h *UserHandler) CreateUser(c *gin.Context) {
	var req models.CreateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}
	
	user, err := h.userService.CreateUser(&req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}
	
	c.JSON(http.StatusCreated, gin.H{
		"message": "User created successfully",
		"user":    user,
	})
}

// GetUser 获取用户
// GET /api/users/:id
func (h *UserHandler) GetUser(c *gin.Context) {
	id := c.Param("id")
	
	user, err := h.userService.GetUser(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": err.Error(),
		})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{
		"user": user,
	})
}

// UpdateUser 更新用户
// PUT /api/users/:id
func (h *UserHandler) UpdateUser(c *gin.Context) {
	id := c.Param("id")
	
	var req models.UpdateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}
	
	user, err := h.userService.UpdateUser(id, &req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{
		"message": "User updated successfully",
		"user":    user,
	})
}

// DeleteUser 删除用户
// DELETE /api/users/:id
func (h *UserHandler) DeleteUser(c *gin.Context) {
	id := c.Param("id")
	
	if err := h.userService.DeleteUser(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": err.Error(),
		})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{
		"message": "User deleted successfully",
	})
}

// ListUsers 列出所有用户
// GET /api/users
func (h *UserHandler) ListUsers(c *gin.Context) {
	users, err := h.userService.ListUsers()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{
		"users": users,
		"count": len(users),
	})
}

// GetUserByUsername 根据用户名获取用户
// GET /api/users/username/:username
func (h *UserHandler) GetUserByUsername(c *gin.Context) {
	username := c.Param("username")
	
	user, err := h.userService.GetUserByUsername(username)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": err.Error(),
		})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{
		"user": user,
	})
}

// ConnectDevice 连接设备
// POST /api/users/:id/connect
func (h *UserHandler) ConnectDevice(c *gin.Context) {
	userID := c.Param("id")
	
	var req struct {
		DeviceID string `json:"device_id" binding:"required"`
	}
	
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}
	
	if err := h.userService.ConnectDevice(userID, req.DeviceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{
		"message": "Device connected successfully",
	})
}

// DisconnectDevice 断开设备
// POST /api/users/:id/disconnect
func (h *UserHandler) DisconnectDevice(c *gin.Context) {
	userID := c.Param("id")
	
	var req struct {
		DeviceID string `json:"device_id" binding:"required"`
	}
	
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}
	
	if err := h.userService.DisconnectDevice(userID, req.DeviceID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{
		"message": "Device disconnected successfully",
	})
}

// UpdateTraffic 更新流量使用
// POST /api/users/:id/traffic
func (h *UserHandler) UpdateTraffic(c *gin.Context) {
	userID := c.Param("id")
	
	var req struct {
		BytesUsed int64 `json:"bytes_used" binding:"required"`
	}
	
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}
	
	if err := h.userService.UpdateTrafficUsage(userID, req.BytesUsed); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": err.Error(),
		})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{
		"message": "Traffic updated successfully",
	})
}

// GetUserStats 获取用户统计信息
// GET /api/users/:id/stats
func (h *UserHandler) GetUserStats(c *gin.Context) {
	userID := c.Param("id")
	
	stats, err := h.userService.GetUserStats(userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"error": err.Error(),
		})
		return
	}
	
	c.JSON(http.StatusOK, gin.H{
		"stats": stats,
	})
}

// RegisterRoutes 注册路由
func (h *UserHandler) RegisterRoutes(router *gin.Engine) {
	api := router.Group("/api")
	{
		users := api.Group("/users")
		{
			users.POST("", h.CreateUser)
			users.GET("", h.ListUsers)
			users.GET("/:id", h.GetUser)
			users.PUT("/:id", h.UpdateUser)
			users.DELETE("/:id", h.DeleteUser)
			users.GET("/username/:username", h.GetUserByUsername)
			users.POST("/:id/connect", h.ConnectDevice)
			users.POST("/:id/disconnect", h.DisconnectDevice)
			users.POST("/:id/traffic", h.UpdateTraffic)
			users.GET("/:id/stats", h.GetUserStats)
		}
	}
}