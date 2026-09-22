package controller

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
)

// ModelPlazaItem 模型广场单个模型信息
type ModelPlazaItem struct {
	ChannelID                  int          `json:"channel_id"`
	ModelDiscount              float64      `json:"model_discount"`
	BaseDurationPricePerMinute *float64     `json:"base_duration_price_per_minute,omitempty"`
	ModelName                  string       `json:"model_name"`
	Provider                   string       `json:"provider"`
	PriceType                  string       `json:"price_type"` // "ratio" | "fixed"
	BaseInputPrice             float64      `json:"base_input_price"`
	BaseOutputPrice            float64      `json:"base_output_price"`
	BaseFixedPrice             float64      `json:"base_fixed_price"`
	ChannelDiscount            float64      `json:"channel_discount"`
	GroupPrices                []GroupPrice `json:"group_prices"`
}

// GroupPrice 某个等级对应的折后价格
type GroupPrice struct {
	FinalDurationPricePerMinute *float64 `json:"final_duration_price_per_minute,omitempty"`
	GroupKey                    string   `json:"group_key"`
	DisplayName                 string   `json:"display_name"`
	GroupDiscount               float64  `json:"group_discount"`
	CombinedDiscount            float64  `json:"combined_discount"`
	FinalInputPrice             float64  `json:"final_input_price"`
	FinalOutputPrice            float64  `json:"final_output_price"`
	FinalFixedPrice             float64  `json:"final_fixed_price"`
}

// ModelPlazaResponse 模型广场API响应
type ModelPlazaResponse struct {
	Models    []ModelPlazaItem    `json:"models"`
	Groups    []model.GroupConfig `json:"groups"`
	Providers []ProviderInfo      `json:"providers"`
	Total     int                 `json:"total"`
	Page      int                 `json:"page"`
	PageSize  int                 `json:"page_size"`
}

// ProviderInfo 供应商信息
type ProviderInfo struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// modelChannelInfo 从渠道收集的模型信息
type modelChannelInfo struct {
	Discount float64 // 当前渠道折扣
	Provider string  // 根据渠道类型确定的供应商
}

type modelChannelKey struct {
	ChannelID int
	ModelName string
}

// GetModelPlaza 公开API：获取模型广场数据
func GetModelPlaza(c *gin.Context) {
	keyword := c.Query("keyword")
	provider := c.Query("provider")
	priceType := c.Query("price_type")
	pageStr := c.Query("page")
	pageSizeStr := c.Query("pagesize")

	page, _ := strconv.Atoi(pageStr)
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(pageSizeStr)
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}

	// 1. 同一供应商的同名模型合并展示，在计数和分页前完成去重。
	modelInfoMap := deduplicateModelCatalog(getModelInfoFromChannels())

	// 2. 获取所有模型基础价格
	priceMap := buildPriceMap()

	// 3. 获取所有等级配置
	groupConfigs, err := model.GetAllGroupConfigs()
	if err != nil {
		groupConfigs = []model.GroupConfig{}
	}

	// 4. 以渠道中的模型为基础构建列表（确保所有可用模型都展示）
	allProviderCount := make(map[string]int)
	var items []ModelPlazaItem

	for key, info := range modelInfoMap {
		modelName := key.ModelName
		price, hasPriceConfig := priceMap[modelName]

		// 确定价格类型和基础价格
		var baseInputPrice, baseOutputPrice, baseFixedPrice float64
		var pt string

		if hasPriceConfig {
			pt = price.PriceType
			baseInputPrice = price.InputPrice
			baseOutputPrice = price.OutputPrice
			baseFixedPrice = price.FixedPrice
		} else {
			// 没有配置价格的模型，用默认 ratio 计算
			ratio := common.GetModelRatio(modelName)
			completionRatio := common.GetCompletionRatio(modelName)
			baseInputPrice = ratio * 0.002 * 1000 // $/1M tokens
			baseOutputPrice = baseInputPrice * completionRatio
			pt = "ratio"
		}

		channelDiscount := info.Discount

		// 计算各等级折后价格
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

		item := ModelPlazaItem{
			ChannelID:       key.ChannelID,
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

		// 统计供应商（不受筛选影响）
		allProviderCount[info.Provider]++

		// 应用筛选
		if keyword != "" && !strings.Contains(strings.ToLower(modelName), strings.ToLower(keyword)) {
			continue
		}
		if provider != "" && !strings.EqualFold(info.Provider, provider) {
			continue
		}
		if priceType != "" && pt != priceType {
			continue
		}

		items = append(items, item)
	}

	// 排序
	sort.Slice(items, func(i, j int) bool {
		if items[i].ModelName == items[j].ModelName {
			return items[i].ChannelID < items[j].ChannelID
		}
		return items[i].ModelName < items[j].ModelName
	})

	// 确保不为 nil（避免 JSON 返回 null）
	if items == nil {
		items = []ModelPlazaItem{}
	}

	// 构建供应商列表
	var providers []ProviderInfo
	for name, count := range allProviderCount {
		providers = append(providers, ProviderInfo{Name: name, Count: count})
	}
	sort.Slice(providers, func(i, j int) bool {
		if providers[i].Count == providers[j].Count {
			return providers[i].Name < providers[j].Name
		}
		return providers[i].Count > providers[j].Count
	})
	if providers == nil {
		providers = []ProviderInfo{}
	}

	// 分页
	total := len(items)
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
		"data": ModelPlazaResponse{
			Models:    items[start:end],
			Groups:    groupConfigs,
			Providers: providers,
			Total:     total,
			Page:      page,
			PageSize:  pageSize,
		},
	})
}

// deduplicateModelCatalog 同一供应商的同名模型选取最低价渠道；同价时按 ID 稳定选择。
// 保留渠道标识供详情页读取对应价格，不影响实际请求的渠道路由。
func deduplicateModelCatalog(channels map[modelChannelKey]*modelChannelInfo) map[modelChannelKey]*modelChannelInfo {
	type catalogKey struct {
		Provider  string
		ModelName string
	}
	selected := make(map[catalogKey]modelChannelKey)
	result := make(map[modelChannelKey]*modelChannelInfo)
	for key, info := range channels {
		entry := catalogKey{Provider: info.Provider, ModelName: key.ModelName}
		if previous, exists := selected[entry]; exists {
			previousInfo := result[previous]
			if info.Discount > previousInfo.Discount ||
				(info.Discount == previousInfo.Discount && key.ChannelID > previous.ChannelID) {
				continue
			}
			delete(result, previous)
		}
		selected[entry] = key
		result[key] = info
	}
	return result
}

// getModelInfoFromChannels 保留各渠道的原始模型信息，兼容已有的渠道详情链接。
func getModelInfoFromChannels() map[modelChannelKey]*modelChannelInfo {
	result := make(map[modelChannelKey]*modelChannelInfo)

	channels, err := model.GetAllChannels(0, 0, "all")
	if err != nil {
		return result
	}

	for _, channel := range channels {
		if channel.Status != common.ChannelStatusEnabled {
			continue
		}

		discount := 1.0
		if channel.Discount != nil && *channel.Discount > 0 && *channel.Discount <= 1 {
			discount = *channel.Discount
		}

		if channel.Models == "" {
			continue
		}

		models := strings.Split(channel.Models, ",")
		for _, m := range models {
			modelName := strings.TrimSpace(m)
			if modelName == "" {
				continue
			}
			// 过滤掉内部标识符（AWS ARN、UUID、超长名称等）
			if shouldSkipModel(modelName) {
				continue
			}

			// 综合判断供应商：聚合渠道用模型名推断，其他用渠道类型
			provider := common.GetModelProvider(modelName, channel.Type)

			result[modelChannelKey{ChannelID: channel.Id, ModelName: modelName}] = &modelChannelInfo{
				Discount: discount,
				Provider: provider,
			}
		}
	}

	return result
}

// shouldSkipModel 判断模型名是否应被过滤（不在模型广场展示）
func shouldSkipModel(name string) bool {
	// AWS ARN 格式
	if strings.HasPrefix(name, "arn:") {
		return true
	}
	// 包含 "/" 的内部路径标识符（如 accounts/xxx/models/yyy）
	if strings.Contains(name, "/") {
		return true
	}
	return false
}

// buildPriceMap 构建模型名 → 价格信息的 map
func buildPriceMap() map[string]*ModelPriceInfo {
	result := make(map[string]*ModelPriceInfo)
	for _, price := range getAllModelPrices() {
		result[price.ModelName] = &price
	}
	return result
}
