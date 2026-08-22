package controller

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/model"
)

type DashboardData struct {
	CurrentQuota int64                   `json:"current_quota"`
	UsedQuota    int64                   `json:"used_quota"`
	TopupQuota   int64                   `json:"topup_quota"`
	GiftQuota    int64                   `json:"gift_quota"`
	TPM          int64                   `json:"tpm"`
	RPM          int64                   `json:"rpm"`
	QuotaPM      int64                   `json:"quota_pm"`
	RequestPD    int64                   `json:"request_pd"`
	UsedPD       int64                   `json:"used_pd"`
	ModelStats   []model.ModelQuotaStats `json:"model_stats"` // 使用 model 包中的类型
}

func GetAdminDashboard(c *gin.Context) {
	// 获取管理员用户信息
	userId := c.GetInt("id")
	user, err := model.GetUserById(userId, false)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	// 一次性获取所有 Dashboard 指标
	metrics, err := model.GetAllDashboardMetrics()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Failed to get metrics: " + err.Error(),
		})
		return
	}

	// 构造返回数据
	dashboard := DashboardData{
		CurrentQuota: user.Quota,
		UsedQuota:    user.UsedQuota,
		TopupQuota:   user.TopupQuota,
		GiftQuota:    user.GiftQuota,
		TPM:          metrics.TPM,
		RPM:          metrics.RPM,
		QuotaPM:      metrics.QuotaPM,
		RequestPD:    metrics.RequestPD,
		UsedPD:       metrics.UsedPD,
		ModelStats:   metrics.ModelStats,
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    dashboard,
		"message": "",
	})
}

func GetUserDashboard(c *gin.Context) {
	// 获取用户信息
	userId := c.GetInt("id")
	user, err := model.GetUserById(userId, false)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	// 一次性获取所有 Dashboard 指标
	metrics, err := model.GetUserDashboardMetrics(userId)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Failed to get metrics: " + err.Error(),
		})
		return
	}

	// 构造返回数据
	dashboard := DashboardData{
		CurrentQuota: user.Quota,
		UsedQuota:    user.UsedQuota,
		TopupQuota:   user.TopupQuota,
		GiftQuota:    user.GiftQuota,
		TPM:          metrics.TPM,
		RPM:          metrics.RPM,
		QuotaPM:      metrics.QuotaPM,
		RequestPD:    metrics.RequestPD,
		UsedPD:       metrics.UsedPD,
		ModelStats:   metrics.ModelStats,
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    dashboard,
		"message": "",
	})
}

func GetAdminDashboardStats(c *gin.Context) {
	stats, err := model.GetDashboard24hStats(0)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": stats, "message": ""})
}

func GetUserDashboardStats(c *gin.Context) {
	stats, err := model.GetDashboard24hStats(c.GetInt("id"))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": stats, "message": ""})
}

func GetAdminUsageMetrics(c *gin.Context) {
	metrics, err := model.GetUsageMetrics(0)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": metrics, "message": ""})
}

func GetUserUsageMetrics(c *gin.Context) {
	metrics, err := model.GetUsageMetrics(c.GetInt("id"))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": metrics, "message": ""})
}

func GetAllGraph(c *gin.Context) {
	target := c.Query("target")
	timestamp, _ := strconv.ParseInt(c.Query("time"), 10, 64)
	hourlyDate, err := model.GetAllGraph(timestamp, target)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data":    hourlyDate,
		"success": true,
		"message": "",
	})
	return
}

func GetUserGraph(c *gin.Context) {
	userId := c.GetInt("id")
	target := c.Query("target")
	timestamp, _ := strconv.ParseInt(c.Query("time"), 10, 64)
	hourlyDate, err := model.GetUserGraph(userId, timestamp, target)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data":    hourlyDate,
		"success": true,
		"message": "",
	})
	return
}
