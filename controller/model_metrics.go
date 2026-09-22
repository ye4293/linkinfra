package controller

import (
	"net/http"
	"strconv"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
)

// GetAllModelMetricsMini GET /api/model-plaza/metrics/all
// 公开接口：返回所有模型的迷你监控摘要
func GetAllModelMetricsMini(c *gin.Context) {
	if !config.ModelMetricsEnabled {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "model metrics disabled",
			"data":    nil,
		})
		return
	}

	data := model.GetCachedAllModelMini()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    data,
	})
}

// GetModelMetricsDetail GET /api/model-plaza/metrics/detail?model_name=xxx
// 公开接口 + 管理员增强：返回单模型的完整监控数据
func GetModelMetricsDetail(c *gin.Context) {
	if !config.ModelMetricsEnabled {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "model metrics disabled",
			"data":    nil,
		})
		return
	}

	modelName := c.Query("model_name")
	if modelName == "" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "model_name is required",
		})
		return
	}

	summary := model.GetCachedModelSummary(modelName)

	// 获取定价数据（复用模型广场逻辑）
	channelID := 0
	if raw, supplied := c.GetQuery("channel_id"); supplied {
		var err error
		channelID, err = strconv.Atoi(raw)
		if err != nil || channelID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid channel_id"})
			return
		}
	}
	pricing := getModelPricing(modelName, channelID)
	provider := ""
	if pricing != nil {
		provider = pricing.Provider
	}

	// 检查是否为管理员 → 返回 channel 级明细（无论是否有公开数据都检查）
	var channels []model.ChannelMetricsSummary
	if isRequestFromAdmin(c) {
		channels = model.GetCachedAdminChannels(modelName)
		if channels == nil {
			channels = []model.ChannelMetricsSummary{}
		}
	}

	if summary == nil {
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": "",
			"data": gin.H{
				"model_name": modelName,
				"provider":   provider,
				"current":    nil,
				"period_24h": nil,
				"pricing":    pricing,
				"channels":   channels,
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"model_name": summary.ModelName,
			"provider":   provider,
			"current":    summary.Current,
			"period_24h": summary.Period24h,
			"pricing":    pricing,
			"channels":   channels,
		},
	})
}

// GetModelMetricsTimeSeries GET /api/model-plaza/metrics/timeseries?model_name=xxx&period=24h
// 公开接口：返回单模型的时间序列数据
func GetModelMetricsTimeSeries(c *gin.Context) {
	if !config.ModelMetricsEnabled {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "model metrics disabled",
			"data":    nil,
		})
		return
	}

	modelName := c.Query("model_name")
	if modelName == "" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "model_name is required",
		})
		return
	}

	period := c.Query("period")
	if period == "" {
		period = "24h"
	}

	var points []model.MetricsTimePoint
	var err error

	switch period {
	case "1h":
		points, err = model.GetModelTimeSeries1h(modelName)
	case "24h":
		points = model.GetCachedModel24hSeries(modelName)
	case "7d":
		points, err = model.GetModelTimeSeriesDaily(modelName, 7)
	case "30d":
		points, err = model.GetModelTimeSeriesDaily(modelName, 30)
	default:
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "invalid period, must be 1h, 24h, 7d, or 30d",
		})
		return
	}

	if err != nil {
		logger.Error(c.Request.Context(), "model metrics timeseries query failed: "+err.Error())
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "query failed, please try again later",
		})
		return
	}

	if points == nil {
		points = []model.MetricsTimePoint{}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"model_name": modelName,
			"period":     period,
			"points":     points,
		},
	})
}

// isRequestFromAdmin 检查请求是否来自管理员（非中间件强制，而是可选检测）
// 同时检查 gin context（API token 认证）和 session（Web 登录认证）
func isRequestFromAdmin(c *gin.Context) bool {
	// 1. 先检查 gin context（通过 API token 认证的请求）
	if role, exists := c.Get("role"); exists {
		if roleInt, ok := role.(int); ok {
			return roleInt >= common.RoleAdminUser
		}
	}
	// 2. 再检查 session（通过 Web 登录的请求）
	session := sessions.Default(c)
	role := session.Get("role")
	if role != nil {
		if roleInt, ok := role.(int); ok {
			return roleInt >= common.RoleAdminUser
		}
	}
	return false
}

// getModelPricing 获取模型的定价信息（复用现有逻辑）
func getModelPricing(modelName string, channelIDs ...int) *ModelPlazaItem {
	infoMap := getModelInfoFromChannels()
	priceMap := buildPriceMap()

	channelID := 0
	if len(channelIDs) > 0 {
		channelID = channelIDs[0]
	}
	var info *modelChannelInfo
	if channelID > 0 {
		info = infoMap[modelChannelKey{ChannelID: channelID, ModelName: modelName}]
	} else {
		// 同一供应商使用目录选中的渠道；跨供应商仍需明确渠道，避免价格歧义。
		for key, candidate := range deduplicateModelCatalog(infoMap) {
			if key.ModelName == modelName {
				if info != nil {
					return nil
				}
				info, channelID = candidate, key.ChannelID
			}
		}
	}
	if info == nil {
		return nil
	}

	price, hasPriceConfig := priceMap[modelName]
	var baseInputPrice, baseOutputPrice, baseFixedPrice float64
	var pt string

	if hasPriceConfig {
		pt = price.PriceType
		baseInputPrice = price.InputPrice
		baseOutputPrice = price.OutputPrice
		baseFixedPrice = price.FixedPrice
	} else {
		ratio := common.GetModelRatio(modelName)
		completionRatio := common.GetCompletionRatio(modelName)
		baseInputPrice = ratio * 0.002 * 1000
		baseOutputPrice = baseInputPrice * completionRatio
		pt = "ratio"
	}

	channelDiscount := info.Discount

	groupConfigs, err := model.GetAllGroupConfigs()
	if err != nil {
		groupConfigs = []model.GroupConfig{}
	}

	var groupPrices []GroupPrice
	for _, gc := range groupConfigs {
		combinedDiscount := channelDiscount * gc.Discount * common.GetModelDiscount(modelName)
		gp := GroupPrice{
			GroupKey:         gc.GroupKey,
			DisplayName:      gc.DisplayName,
			GroupDiscount:    gc.Discount,
			CombinedDiscount: combinedDiscount,
		}
		if pt == "duration" && price.DurationPricePerMinute != nil {
			amount := *price.DurationPricePerMinute * combinedDiscount
			gp.FinalDurationPricePerMinute = &amount
		} else if pt == "fixed" {
			gp.FinalFixedPrice = baseFixedPrice * combinedDiscount
		} else {
			gp.FinalInputPrice = baseInputPrice * combinedDiscount
			gp.FinalOutputPrice = baseOutputPrice * combinedDiscount
		}
		groupPrices = append(groupPrices, gp)
	}

	item := &ModelPlazaItem{
		ChannelID:       channelID,
		ModelName:       modelName,
		Provider:        info.Provider,
		PriceType:       pt,
		BaseInputPrice:  baseInputPrice,
		BaseOutputPrice: baseOutputPrice,
		BaseFixedPrice:  baseFixedPrice,
		ChannelDiscount: channelDiscount,
		ModelDiscount:   common.GetModelDiscount(modelName),
		GroupPrices:     groupPrices,
	}
	if hasPriceConfig {
		item.BaseDurationPricePerMinute = price.DurationPricePerMinute
	}
	return item
}
