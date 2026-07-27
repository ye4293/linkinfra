package controller

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
)

// persistGroupRatio 把内存中的 common.GroupRatio 持久化到 options 表。
//
// 原实现只改内存不写 options 表：重启后 model.InitOptionMap 会用 options
// 表的旧值覆盖内存，但 group_configs 表仍是新值，两处永久漂移，管理员
// 在后台改的折扣重启就丢。
//
// 注意 commission_rate 与 upgrade_threshold 不走 options 表、也不进内存
// map —— 它们只在充值回调里被读取（低频），每次直接查 group_configs，
// 从根本上避免这一类缓存漂移。
func persistGroupRatio() {
	if err := model.UpdateOption("GroupRatio", common.GroupRatio2JSONString()); err != nil {
		logger.SysError("failed to persist GroupRatio to options: " + err.Error())
	}
}

// validateGroupConfigRanges 校验 discount / commission_rate / upgrade_threshold 的取值范围。
// 返回空串表示通过。
func validateGroupConfigRanges(config *model.GroupConfig) string {
	// discount 是计费乘数：1.0 = 无折扣，0.5 = 五折，0 = 免费。
	// 任何 > 1 的值都会让该分组的所有请求被放大 N 倍，必须挡住。
	if config.Discount < 0 || config.Discount > 1 {
		return "discount must be between 0 and 1 (multiplier; 1 = no discount)."
	}
	// commission_rate 是返现比例。> 1 意味着返的比充的多，直接资金漏洞。
	if config.CommissionRate < 0 || config.CommissionRate > 1 {
		return "commission_rate must be between 0 and 1."
	}
	// 负门槛会让等级判定行为不可预期
	if config.UpgradeThreshold < 0 {
		return "upgrade_threshold must not be negative."
	}
	return ""
}

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

	if msg := validateGroupConfigRanges(&config); msg != "" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": msg,
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

	// 同步内存并持久化到 options 表，两者缺一都会导致重启后配置漂移
	common.GroupRatio[config.GroupKey] = config.Discount
	persistGroupRatio()

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

	if msg := validateGroupConfigRanges(&config); msg != "" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": msg,
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

	// 同步内存并持久化到 options 表
	common.GroupRatio[config.GroupKey] = config.Discount
	persistGroupRatio()

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

	// 同步内存并持久化到 options 表
	delete(common.GroupRatio, config.GroupKey)
	persistGroupRatio()

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Deleted.",
	})
}
