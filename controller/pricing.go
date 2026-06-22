package controller

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
)

// ModelPriceInfo 模型价格信息
type ModelPriceInfo struct {
	ModelName        string  `json:"model_name"`
	ModelRatio       float64 `json:"model_ratio"`
	CompletionRatio  float64 `json:"completion_ratio"`
	FixedPrice       float64 `json:"fixed_price"`
	InputPrice       float64 `json:"input_price"`
	OutputPrice      float64 `json:"output_price"`
	PriceType        string  `json:"price_type"`
	HasRatio         bool    `json:"has_ratio"`
	CacheRatio       float64 `json:"cache_ratio"`
	ImageInputRatio  float64 `json:"image_input_ratio"`
	ImageOutputRatio float64 `json:"image_output_ratio"`
	AudioInputRatio  float64 `json:"audio_input_ratio"`
	AudioOutputRatio float64 `json:"audio_output_ratio"`
}

// GetModelPrices 获取所有模型的价格信息
func GetModelPrices(c *gin.Context) {
	// 获取分页参数
	pageStr := c.Query("page")
	pageSizeStr := c.Query("pagesize")
	keyword := c.Query("keyword")
	showNoRatio := c.Query("show_no_ratio") == "true"

	page, _ := strconv.Atoi(pageStr)
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(pageSizeStr)
	if pageSize <= 0 {
		pageSize = 20
	}

	// 获取所有模型价格信息
	allPrices := getAllModelPrices()

	// 过滤
	var filteredPrices []ModelPriceInfo
	for _, price := range allPrices {
		// 按关键词过滤
		if keyword != "" && !strings.Contains(strings.ToLower(price.ModelName), strings.ToLower(keyword)) {
			continue
		}
		// 按是否配置倍率过滤
		if showNoRatio && price.HasRatio {
			continue
		}
		filteredPrices = append(filteredPrices, price)
	}

	// 排序（按模型名称）
	sort.Slice(filteredPrices, func(i, j int) bool {
		return filteredPrices[i].ModelName < filteredPrices[j].ModelName
	})

	// 计算分页
	total := len(filteredPrices)
	start := (page - 1) * pageSize
	end := start + pageSize
	if start >= total {
		start = total
		end = total
	}
	if end > total {
		end = total
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"list":        filteredPrices[start:end],
			"total":       total,
			"page":        page,
			"pageSize":    pageSize,
			"totalPages":  (total + pageSize - 1) / pageSize,
		},
	})
}

// GetUnsetRatioModels 获取未设置倍率的模型列表（从渠道中获取）
func GetUnsetRatioModels(c *gin.Context) {
	// 获取分页参数
	pageStr := c.Query("page")
	pageSizeStr := c.Query("pagesize")
	keyword := c.Query("keyword")

	page, _ := strconv.Atoi(pageStr)
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(pageSizeStr)
	if pageSize <= 0 {
		pageSize = 10
	}

	// 获取所有渠道中使用的模型
	usedModels := getUsedModelsFromChannels()

	// 获取已配置倍率的模型
	configuredModels := make(map[string]bool)
	for modelName := range common.ModelRatio {
		configuredModels[modelName] = true
	}
	for modelName := range common.ModelPrice {
		configuredModels[modelName] = true
	}

	// 找出未配置的模型
	var unsetModels []ModelPriceInfo
	for modelName := range usedModels {
		if _, ok := configuredModels[modelName]; !ok {
			// 按关键词过滤
			if keyword != "" && !strings.Contains(strings.ToLower(modelName), strings.ToLower(keyword)) {
				continue
			}
			unsetModels = append(unsetModels, ModelPriceInfo{
				ModelName:       modelName,
				ModelRatio:      0,
				CompletionRatio: 0,
				FixedPrice:      0,
				InputPrice:      0,
				OutputPrice:     0,
				PriceType:       "",
				HasRatio:        false,
			})
		}
	}

	// 排序
	sort.Slice(unsetModels, func(i, j int) bool {
		return unsetModels[i].ModelName < unsetModels[j].ModelName
	})

	// 分页
	total := len(unsetModels)
	start := (page - 1) * pageSize
	end := start + pageSize
	if start >= total {
		start = total
		end = total
	}
	if end > total {
		end = total
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"list":       unsetModels[start:end],
			"total":      total,
			"page":       page,
			"pageSize":   pageSize,
			"totalPages": (total + pageSize - 1) / pageSize,
		},
	})
}

// UpdateModelRatio 更新单个模型的倍率
func UpdateModelRatio(c *gin.Context) {
	var req struct {
		ModelName        string   `json:"model_name" binding:"required"`
		ModelRatio       *float64 `json:"model_ratio"`
		CompletionRatio  *float64 `json:"completion_ratio"`
		FixedPrice       *float64 `json:"fixed_price"`
		ImageInputRatio  *float64 `json:"image_input_ratio"`
		ImageOutputRatio *float64 `json:"image_output_ratio"`
		AudioInputRatio  *float64 `json:"audio_input_ratio"`
		AudioOutputRatio *float64 `json:"audio_output_ratio"`
		CacheRatio       *float64 `json:"cache_ratio"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Invalid parameters: " + err.Error(),
		})
		return
	}

	// 更新模型倍率
	if req.ModelRatio != nil {
		common.ModelRatio[req.ModelName] = *req.ModelRatio
		// 保存到数据库
		err := model.UpdateOption("ModelRatio", common.ModelRatio2JSONString())
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Failed to save model ratios: " + err.Error(),
			})
			return
		}
	}

	// 更新补全倍率
	if req.CompletionRatio != nil {
		common.CompletionRatio[req.ModelName] = *req.CompletionRatio
		err := model.UpdateOption("CompletionRatio", common.CompletionRatio2JSONString())
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Failed to save completion ratios: " + err.Error(),
			})
			return
		}
	}

	// 更新按次计费价格
	if req.FixedPrice != nil {
		common.ModelPrice[req.ModelName] = *req.FixedPrice
		err := model.UpdateOption("PerCallPricing", common.ModelPrice2JSONString())
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Failed to save per-call prices: " + err.Error(),
			})
			return
		}
	}

	// 更新图片输入倍率
	if req.ImageInputRatio != nil {
		common.ImageInputRatio[req.ModelName] = *req.ImageInputRatio
		err := model.UpdateOption("ImageInputRatio", common.ImageInputRatio2JSONString())
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Failed to save image input ratios: " + err.Error(),
			})
			return
		}
	}

	// 更新图片输出倍率
	if req.ImageOutputRatio != nil {
		common.ImageOutputRatio[req.ModelName] = *req.ImageOutputRatio
		err := model.UpdateOption("ImageOutputRatio", common.ImageOutputRatio2JSONString())
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Failed to save image output ratios: " + err.Error(),
			})
			return
		}
	}

	// 更新音频输入倍率
	if req.AudioInputRatio != nil {
		common.AudioInputRatio[req.ModelName] = *req.AudioInputRatio
		err := model.UpdateOption("AudioInputRatio", common.AudioInputRatio2JSONString())
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Failed to save audio input ratios: " + err.Error(),
			})
			return
		}
	}

	// 更新音频输出倍率
	if req.AudioOutputRatio != nil {
		common.AudioOutputRatio[req.ModelName] = *req.AudioOutputRatio
		err := model.UpdateOption("AudioOutputRatio", common.AudioOutputRatio2JSONString())
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Failed to save audio output ratios: " + err.Error(),
			})
			return
		}
	}

	// 更新缓存倍率
	if req.CacheRatio != nil {
		common.CacheRatio[req.ModelName] = *req.CacheRatio
		err := model.UpdateOption("CacheRatio", common.CacheRatio2JSONString())
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Failed to save cache ratios: " + err.Error(),
			})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Updated.",
	})
}

// BatchUpdateModelRatio 批量更新模型倍率
func BatchUpdateModelRatio(c *gin.Context) {
	var req struct {
		Models []struct {
			ModelName        string   `json:"model_name"`
			ModelRatio       *float64 `json:"model_ratio"`
			CompletionRatio  *float64 `json:"completion_ratio"`
			FixedPrice       *float64 `json:"fixed_price"`
			ImageInputRatio  *float64 `json:"image_input_ratio"`
			ImageOutputRatio *float64 `json:"image_output_ratio"`
			AudioInputRatio  *float64 `json:"audio_input_ratio"`
			AudioOutputRatio *float64 `json:"audio_output_ratio"`
			CacheRatio       *float64 `json:"cache_ratio"`
		} `json:"models"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "Invalid parameters: " + err.Error(),
		})
		return
	}

	modelRatioUpdated := false
	completionRatioUpdated := false
	fixedPriceUpdated := false
	imageInputRatioUpdated := false
	imageOutputRatioUpdated := false
	audioInputRatioUpdated := false
	audioOutputRatioUpdated := false
	cacheRatioUpdated := false

	for _, m := range req.Models {
		if m.ModelRatio != nil {
			common.ModelRatio[m.ModelName] = *m.ModelRatio
			modelRatioUpdated = true
		}
		if m.CompletionRatio != nil {
			common.CompletionRatio[m.ModelName] = *m.CompletionRatio
			completionRatioUpdated = true
		}
		if m.FixedPrice != nil {
			common.ModelPrice[m.ModelName] = *m.FixedPrice
			fixedPriceUpdated = true
		}
		if m.ImageInputRatio != nil {
			common.ImageInputRatio[m.ModelName] = *m.ImageInputRatio
			imageInputRatioUpdated = true
		}
		if m.ImageOutputRatio != nil {
			common.ImageOutputRatio[m.ModelName] = *m.ImageOutputRatio
			imageOutputRatioUpdated = true
		}
		if m.AudioInputRatio != nil {
			common.AudioInputRatio[m.ModelName] = *m.AudioInputRatio
			audioInputRatioUpdated = true
		}
		if m.AudioOutputRatio != nil {
			common.AudioOutputRatio[m.ModelName] = *m.AudioOutputRatio
			audioOutputRatioUpdated = true
		}
		if m.CacheRatio != nil {
			common.CacheRatio[m.ModelName] = *m.CacheRatio
			cacheRatioUpdated = true
		}
	}

	// 保存更新
	if modelRatioUpdated {
		err := model.UpdateOption("ModelRatio", common.ModelRatio2JSONString())
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Failed to save model ratios: " + err.Error(),
			})
			return
		}
	}

	if completionRatioUpdated {
		err := model.UpdateOption("CompletionRatio", common.CompletionRatio2JSONString())
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Failed to save completion ratios: " + err.Error(),
			})
			return
		}
	}

	if fixedPriceUpdated {
		err := model.UpdateOption("PerCallPricing", common.ModelPrice2JSONString())
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Failed to save per-call prices: " + err.Error(),
			})
			return
		}
	}

	if imageInputRatioUpdated {
		err := model.UpdateOption("ImageInputRatio", common.ImageInputRatio2JSONString())
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Failed to save image input ratios: " + err.Error(),
			})
			return
		}
	}

	if imageOutputRatioUpdated {
		err := model.UpdateOption("ImageOutputRatio", common.ImageOutputRatio2JSONString())
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Failed to save image output ratios: " + err.Error(),
			})
			return
		}
	}

	if audioInputRatioUpdated {
		err := model.UpdateOption("AudioInputRatio", common.AudioInputRatio2JSONString())
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Failed to save audio input ratios: " + err.Error(),
			})
			return
		}
	}

	if audioOutputRatioUpdated {
		err := model.UpdateOption("AudioOutputRatio", common.AudioOutputRatio2JSONString())
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Failed to save audio output ratios: " + err.Error(),
			})
			return
		}
	}

	if cacheRatioUpdated {
		err := model.UpdateOption("CacheRatio", common.CacheRatio2JSONString())
		if err != nil {
			c.JSON(http.StatusOK, gin.H{
				"success": false,
				"message": "Failed to save cache ratios: " + err.Error(),
			})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Batch update complete.",
	})
}

// getAllModelPrices 获取所有模型的价格信息
func getAllModelPrices() []ModelPriceInfo {
	var prices []ModelPriceInfo
	
	// 基础价格单位：$0.002 / 1K tokens
	basePricePerK := 0.002
	
	// 获取QuotaPerUnit配置
	quotaPerUnit := config.QuotaPerUnit
	if quotaPerUnit <= 0 {
		quotaPerUnit = 500000 // 默认值
	}

	// 处理按倍率计费的模型
	processedModels := make(map[string]bool)
	for modelName, ratio := range common.ModelRatio {
		processedModels[modelName] = true
		
		// 计算输入价格 ($/1M tokens)
		inputPricePerM := ratio * basePricePerK * 1000
		
		// 获取补全倍率
		completionRatio := common.GetCompletionRatio(modelName)
		
		// 计算输出价格 ($/1M tokens)
		outputPricePerM := inputPricePerM * completionRatio

		prices = append(prices, ModelPriceInfo{
			ModelName:        modelName,
			ModelRatio:       ratio,
			CompletionRatio:  completionRatio,
			FixedPrice:       0,
			InputPrice:       inputPricePerM,
			OutputPrice:      outputPricePerM,
			PriceType:        "ratio",
			HasRatio:         true,
			CacheRatio:       common.GetCacheRatio(modelName),
			ImageInputRatio:  common.GetImageInputRatio(modelName),
			ImageOutputRatio: common.GetImageOutputRatio(modelName),
			AudioInputRatio:  common.GetAudioInputRatio(modelName),
			AudioOutputRatio: common.GetAudioOutputRatio(modelName),
		})
	}

	// 处理按次计费的模型
	for modelName, price := range common.ModelPrice {
		if processedModels[modelName] {
			// 如果已经在倍率模型中处理过，更新固定价格
			for i, p := range prices {
				if p.ModelName == modelName {
					prices[i].FixedPrice = price
					prices[i].PriceType = "fixed" // 按次计费优先
					break
				}
			}
		} else {
			processedModels[modelName] = true
			prices = append(prices, ModelPriceInfo{
				ModelName:       modelName,
				ModelRatio:      0,
				CompletionRatio: 0,
				FixedPrice:      price,
				InputPrice:      0,
				OutputPrice:     0,
				PriceType:       "fixed",
				HasRatio:        true,
			})
		}
	}

	return prices
}

// getUsedModelsFromChannels 从所有渠道中获取使用的模型
func getUsedModelsFromChannels() map[string]bool {
	usedModels := make(map[string]bool)

	// 获取所有渠道
	channels, err := model.GetAllChannels(0, 0, "all")
	if err != nil {
		return usedModels
	}

	for _, channel := range channels {
		if channel.Models != "" {
			models := strings.Split(channel.Models, ",")
			for _, m := range models {
				modelName := strings.TrimSpace(m)
				if modelName != "" {
					usedModels[modelName] = true
				}
			}
		}
	}

	return usedModels
}

