package controller

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/model"
)

// ListAvailableModels 只返回当前用户分组中已启用渠道实际提供的模型。
// /api/models 仍提供渠道编辑器需要的内置模型目录。
func ListAvailableModels(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	group, err := model.GetUserGroup(c.GetInt("id"))
	if err != nil || group == "" {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "Unable to load your available models. Please try again later."})
		return
	}
	models, err := model.GetEnabledModelsForGroup(group)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "Unable to load your available models. Please try again later."})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": models})
}
