package controller

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
)

// GetAllGroupConfigs 获取所有分组等级配置
func GetAllGroupConfigs(c *gin.Context) {
	configs, err := model.GetAllGroupConfigs()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Failed to load group configs: " + err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    configs,
	})
}

// CreateGroupConfigHandler 创建分组等级配置
func CreateGroupConfigHandler(c *gin.Context) {
	var config model.GroupConfig
	if err := c.ShouldBindJSON(&config); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Invalid parameters: " + err.Error(),
		})
		return
	}

	if config.GroupKey == "" || config.DisplayName == "" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "group_key and display_name are required.",
		})
		return
	}

	// discount 是计费乘数：1.0 = 无折扣，0.5 = 五折，0 = 免费。
	// 任何 > 1 的值都会让当前分组的所有请求被放大 N 倍，必须挡住。
	if config.Discount < 0 || config.Discount > 1 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "discount must be between 0 and 1 (multiplier; 1 = no discount).",
		})
		return
	}

	if err := model.CreateGroupConfig(&config); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Failed to create group config: " + err.Error(),
		})
		return
	}

	// 同步更新 common.GroupRatio
	common.GroupRatio[config.GroupKey] = config.Discount

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Created.",
	})
}

// UpdateGroupConfigHandler 更新分组等级配置
func UpdateGroupConfigHandler(c *gin.Context) {
	var config model.GroupConfig
	if err := c.ShouldBindJSON(&config); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Invalid parameters: " + err.Error(),
		})
		return
	}

	if config.ID == 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "id is required.",
		})
		return
	}

	// 同 Create：discount 必须在 [0, 1] 区间内，防止 UI 以外的客户端误把百分比传进来
	if config.Discount < 0 || config.Discount > 1 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "discount must be between 0 and 1 (multiplier; 1 = no discount).",
		})
		return
	}

	if err := model.UpdateGroupConfig(&config); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Failed to update group config: " + err.Error(),
		})
		return
	}

	// 同步更新 common.GroupRatio
	common.GroupRatio[config.GroupKey] = config.Discount

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Updated.",
	})
}

// DeleteGroupConfigHandler 删除分组等级配置
func DeleteGroupConfigHandler(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Invalid id.",
		})
		return
	}

	// 先查询要删除的配置，以便同步清理内存
	config, err := model.GetGroupConfigByID(id)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Group config not found.",
		})
		return
	}

	if err := model.DeleteGroupConfigByID(id); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Failed to delete group config: " + err.Error(),
		})
		return
	}

	// 同步删除 common.GroupRatio 中的条目
	delete(common.GroupRatio, config.GroupKey)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Deleted.",
	})
}
