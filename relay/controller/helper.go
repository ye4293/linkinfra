package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/constant"
	relaymodel "github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

func getAndValidateTextRequest(c *gin.Context, relayMode int) (*relaymodel.GeneralOpenAIRequest, error) {
	textRequest := &relaymodel.GeneralOpenAIRequest{}
	err := common.UnmarshalBodyReusable(c, textRequest)
	if err != nil {
		return nil, err
	}
	if relayMode == constant.RelayModeModerations && textRequest.Model == "" {
		textRequest.Model = "text-moderation-latest"
	}
	if relayMode == constant.RelayModeEmbeddings && textRequest.Model == "" {
		textRequest.Model = c.Param("model")
	}
	err = util.ValidateTextRequest(textRequest, relayMode)
	if err != nil {
		return nil, err
	}
	return textRequest, nil
}

func getImageRequest(c *gin.Context, relayMode int) (*relaymodel.ImageRequest, error) {
	// 检查内容类型
	contentType := c.GetHeader("Content-Type")
	isFormRequest := strings.Contains(contentType, "multipart/form-data") || strings.Contains(contentType, "application/x-www-form-urlencoded")

	if isFormRequest {
		// 对于表单请求，我们只需要获取基本信息，不消费请求体
		imageRequest := &relaymodel.ImageRequest{
			N:     1,
			Size:  "1024x1024",
			Model: "dall-e-2",
		}

		// 尝试获取表单字段，但不解析整个表单文件部分
		if strings.Contains(contentType, "multipart/form-data") {
			// 尝试只解析表单字段，不读取文件
			c.Request.ParseMultipartForm(1 << 10) // 只解析1KB的数据
			if model := c.Request.FormValue("model"); model != "" {
				imageRequest.Model = model
			}
			// 解析stream参数
			if stream := c.Request.FormValue("stream"); stream != "" {
				imageRequest.Stream = stream == "true"
			}
		} else {
			// 对于url编码的表单
			c.Request.ParseForm()
			if model := c.Request.FormValue("model"); model != "" {
				imageRequest.Model = model
			}
			// 解析stream参数
			if stream := c.Request.FormValue("stream"); stream != "" {
				imageRequest.Stream = stream == "true"
			}
		}

		return imageRequest, nil
	} else {
		// 对于JSON请求，使用原有逻辑
		imageRequest := &relaymodel.ImageRequest{}
		err := common.UnmarshalBodyReusable(c, imageRequest)
		if err != nil {
			return nil, err
		}
		if imageRequest.N == 0 {
			imageRequest.N = 1
		}
		if imageRequest.Size == "" {
			imageRequest.Size = "1024x1024"
		}
		if imageRequest.Model == "" {
			imageRequest.Model = "dall-e-2"
		}
		return imageRequest, nil
	}
}

func getImageCostRatio(imageRequest *relaymodel.ImageRequest) (float64, error) {
	// 检查空指针
	if imageRequest == nil {
		return 0, errors.New("imageRequest is nil")
	}

	// 初始化基础倍率
	var imageCostRatio float64 = 1.0

	switch imageRequest.Model {
	case "dall-e-2":
		switch imageRequest.Size {
		case "1024x1024":
			imageCostRatio = 1.0 // 1x 基准价格
		case "512x512":
			imageCostRatio = 0.9 // 0.9x 基准价格
		case "256x256":
			imageCostRatio = 0.8 // 0.8x 基准价格
		default:
			return 0, fmt.Errorf("size not supported for DALL-E 2: %s", imageRequest.Size)
		}
	case "dall-e-3":
		switch imageRequest.Size {
		case "1024x1024":
			imageCostRatio = 1.0 // 1x 基准价格
		case "1024x1792", "1792x1024":
			imageCostRatio = 2.0 // 2x 基准价格
		default:
			return 0, fmt.Errorf("size not supported for DALL-E 3: %s", imageRequest.Size)
		}

		// HD质量的价格调整
		if imageRequest.Quality == "hd" && imageRequest.Model == "dall-e-3" {
			if imageRequest.Size == "1024x1024" {
				imageCostRatio = 2 // 2x 基准价格
			} else {
				imageCostRatio = 3 // 3x 基准价格
			}
		}
	default:
		return 1, nil // 处理所有其他模型
	}

	return imageCostRatio, nil
}

func getPromptTokens(textRequest *relaymodel.GeneralOpenAIRequest, relayMode int) int {
	switch relayMode {
	case constant.RelayModeChatCompletions:
		return openai.CountTokenMessages(textRequest.Messages, textRequest.Model)
	case constant.RelayModeCompletions:
		return openai.CountTokenInput(textRequest.Prompt, textRequest.Model)
	case constant.RelayModeModerations:
		return openai.CountTokenInput(textRequest.Input, textRequest.Model)
	}
	return 0
}

func getPreConsumedQuota(textRequest *relaymodel.GeneralOpenAIRequest, promptTokens int, ratio float64) int64 {
	preConsumedTokens := config.PreConsumedQuota + int64(promptTokens)
	if textRequest.MaxTokens != 0 {
		preConsumedTokens += int64(textRequest.MaxTokens)
	}
	return int64(float64(preConsumedTokens) * ratio)
}

func preConsumeQuota(ctx context.Context, textRequest *relaymodel.GeneralOpenAIRequest, promptTokens int, ratio float64, meta *util.RelayMeta) (int64, *relaymodel.ErrorWithStatusCode) {
	var preConsumedQuota int64

	billingModelName := meta.BillingModelName()
	if billingModelName == "" {
		billingModelName = textRequest.Model
	}
	// 先检查是否有固定价格
	modelPrice := common.GetModelPrice(billingModelName, false)
	if modelPrice != -1 {
		// 使用固定价格计费（按次计费）
		// groupRatio 已融合 等级折扣 × 渠道折扣 × 用户渠道折扣
		groupRatio := meta.CombinedGroupRatio()
		preConsumedQuota = int64(modelPrice * 500000 * groupRatio)
	} else {
		// 使用基于token的倍率计费
		preConsumedQuota = getPreConsumedQuota(textRequest, promptTokens, ratio)
	}

	userQuota, err := model.CacheGetUserQuota(ctx, meta.UserId)
	if err != nil {
		return preConsumedQuota, openai.ErrorWrapper(err, "get_user_quota_failed", http.StatusInternalServerError)
	}
	if userQuota-preConsumedQuota < 0 {
		return preConsumedQuota, openai.ErrorWrapper(errors.New("user quota is not enough"), "insufficient_user_quota", http.StatusForbidden)
	}
	err = model.CacheDecreaseUserQuota(meta.UserId, preConsumedQuota)
	if err != nil {
		return preConsumedQuota, openai.ErrorWrapper(err, "decrease_user_quota_failed", http.StatusInternalServerError)
	}
	if userQuota > 100*preConsumedQuota {
		// in this case, we do not pre-consume quota
		// because the user has enough quota
		preConsumedQuota = 0
		logger.Info(ctx, fmt.Sprintf("user %d has enough quota %d, trusted and no need to pre-consume", meta.UserId, userQuota))
	}
	if preConsumedQuota > 0 {
		err := model.PreConsumeTokenQuota(meta.TokenId, preConsumedQuota)
		if err != nil {
			return preConsumedQuota, openai.ErrorWrapper(err, "pre_consume_token_quota_failed", http.StatusForbidden)
		}
	}
	return preConsumedQuota, nil
}

// preConsumeImageQuota 图片请求专用的预扣费函数
// 与 preConsumeQuota 逻辑一致，但不依赖 GeneralOpenAIRequest
func preConsumeImageQuota(ctx context.Context, estimatedQuota int64, meta *util.RelayMeta) (int64, *relaymodel.ErrorWithStatusCode) {
	preConsumedQuota := estimatedQuota

	userQuota, err := model.CacheGetUserQuota(ctx, meta.UserId)
	if err != nil {
		return 0, openai.ErrorWrapper(err, "get_user_quota_failed", http.StatusInternalServerError)
	}
	if userQuota-preConsumedQuota < 0 {
		return 0, openai.ErrorWrapper(errors.New("user quota is not enough"), "insufficient_user_quota", http.StatusForbidden)
	}
	err = model.CacheDecreaseUserQuota(meta.UserId, preConsumedQuota)
	if err != nil {
		return 0, openai.ErrorWrapper(err, "decrease_user_quota_failed", http.StatusInternalServerError)
	}
	if userQuota > 100*preConsumedQuota {
		// 用户余额充足，信任用户，不做 token 级预扣
		preConsumedQuota = 0
		logger.Info(ctx, fmt.Sprintf("user %d has enough quota %d, trusted and no need to pre-consume for image request", meta.UserId, userQuota))
	}
	if preConsumedQuota > 0 {
		err := model.PreConsumeTokenQuota(meta.TokenId, preConsumedQuota)
		if err != nil {
			return preConsumedQuota, openai.ErrorWrapper(err, "pre_consume_token_quota_failed", http.StatusForbidden)
		}
	}
	return preConsumedQuota, nil
}

func postConsumeQuota(ctx context.Context, c *gin.Context, usage *relaymodel.Usage, meta *util.RelayMeta, textRequest *relaymodel.GeneralOpenAIRequest, ratio float64, preConsumedQuota int64, modelRatio float64, groupRatio float64, duration float64, title string, httpReferer string, firstWordLatency float64) {
	// 更新多Key使用统计
	updateMultiKeyUsage(ctx, meta, usage != nil)

	if usage == nil {
		// 打印用户和请求体信息
		logger.Error(ctx, fmt.Sprintf("usage is nil, which is unexpected. UserId: %d, RequestBody: %+v",
			meta.UserId, textRequest))
		return
	}

	var quota int64
	var logContent string
	promptTokens := usage.PromptTokens
	completionTokens := usage.CompletionTokens
	cachedTokens := usage.PromptTokensDetails.CachedTokens
	cacheWriteTokens := usage.PromptTokensDetails.CacheWriteTokens

	billingModelName := meta.BillingModelName()
	if billingModelName == "" {
		billingModelName = textRequest.Model
	}
	// 预先获取计费参数，避免后续重复调用
	modelPrice := common.GetModelPrice(billingModelName, false)
	completionRatio := common.GetCompletionRatio(billingModelName)
	// groupRatio 此处已是"融合后的组合折扣" = 等级折扣 × 渠道折扣 × 用户渠道折扣
	// 直接从 meta 取三个分量，用于日志/账单分项展示（比用除法反推更稳定）。
	tierRatio := common.GetGroupRatio(meta.Group)
	if modelPrice != -1 {
		// 使用固定价格计费（按次计费）
		quota = int64(modelPrice * 500000 * groupRatio)
		logContent = fmt.Sprintf("模型固定价格 %.2f$，等级折扣 %.2f，渠道折扣 %.2f，用户渠道折扣 %.2f", modelPrice, tierRatio, meta.ChannelDiscount, meta.UserChannelRatio)
	} else {
		// 使用基于token的倍率计费
		if cachedTokens > 0 || cacheWriteTokens > 0 {
			// 有缓存命中：从输入 token 中扣除缓存部分，缓存按 cacheRatio 折扣计费
			cacheRatio := common.GetCacheRatio(billingModelName)
			nonCachedPromptTokens := promptTokens - cachedTokens - cacheWriteTokens
			if nonCachedPromptTokens < 0 {
				nonCachedPromptTokens = 0
			}
			inputQuota := float64(nonCachedPromptTokens) * modelRatio * groupRatio
			cacheQuota := float64(cachedTokens) * modelRatio * cacheRatio * groupRatio
			cacheWriteQuota := float64(cacheWriteTokens) * modelRatio * common.GetCreateCacheRatio(billingModelName) * groupRatio
			outputQuota := float64(completionTokens) * modelRatio * completionRatio * groupRatio
			quota = int64(math.Ceil(inputQuota + cacheQuota + cacheWriteQuota + outputQuota))
		} else {
			quota = int64(math.Ceil((float64(promptTokens) + float64(completionTokens)*completionRatio) * ratio))
		}
		if ratio != 0 && quota <= 0 {
			quota = 1
		}
		totalTokens := promptTokens + completionTokens
		if totalTokens == 0 {
			// in this case, must be some error happened
			// we cannot just return, because we may have to return the pre-consumed quota
			quota = 0
		}
		logContent = fmt.Sprintf("模型倍率 %.2f，等级折扣 %.2f，渠道折扣 %.2f，用户渠道折扣 %.2f，补全倍率 %.2f", modelRatio, tierRatio, meta.ChannelDiscount, meta.UserChannelRatio, completionRatio)
	}

	quotaDelta := quota - preConsumedQuota
	err := model.PostConsumeTokenQuota(meta.TokenId, quotaDelta)
	if err != nil {
		logger.Error(ctx, "error consuming token remain quota: "+err.Error())
	}
	err = model.CacheUpdateUserQuota(ctx, meta.UserId)
	if err != nil {
		logger.Error(ctx, "error update user quota cache: "+err.Error())
	}
	if quota != 0 {
		// 获取渠道历史信息
		otherInfo := getChannelHistoryInfo(c)
		// 追加模型重定向信息
		otherInfo = appendModelMappingInfo(otherInfo, meta.OriginModelName, meta.ActualModelName)
		// 追加计费详情
		billingDetails := map[string]interface{}{
			"group_ratio":        groupRatio,
			"tier_ratio":         tierRatio,
			"channel_discount":   meta.ChannelDiscount,
			"user_channel_ratio": meta.UserChannelRatio,
		}
		// 多 Key 渠道：记录本次实际使用的 Key 索引
		if meta.IsMultiKey && meta.KeyIndex != nil {
			billingDetails["is_multi_key"] = true
			billingDetails["key_index"] = *meta.KeyIndex
		}
		if modelPrice != -1 {
			billingDetails["billing_type"] = "fixed_price"
			billingDetails["model_price"] = modelPrice
		} else {
			billingDetails["billing_type"] = "token"
			billingDetails["model_ratio"] = modelRatio
			billingDetails["completion_ratio"] = completionRatio
		}
		// 追加缓存 token 信息
		if cachedTokens > 0 {
			billingDetails["cached_tokens"] = cachedTokens
			billingDetails["cache_ratio"] = common.GetCacheRatio(billingModelName)
		}
		if cacheWriteTokens > 0 {
			billingDetails["cache_write_tokens"] = cacheWriteTokens
			billingDetails["create_cache_ratio"] = common.GetCreateCacheRatio(billingModelName)
		}
		otherInfo = appendBillingDetails(ctx, otherInfo, billingDetails)
		// 把重试历史（如有）也拼进 other，供管理员展开查看
		otherInfo = util.AppendRetryHistoryOther(c, otherInfo, duration)
		// 获取 X-Request-ID
		xRequestID := c.GetString("X-Request-ID")
		xResponseID := c.GetString("x_response_id")

		// 使用重定向后的实际模型名记录日志
		logModelName := textRequest.Model
		if name := meta.BillingModelName(); name != "" {
			logModelName = name
		}
		model.RecordConsumeLogWithOtherAndRequestID(ctx, meta.UserId, meta.ChannelId, promptTokens, completionTokens, logModelName, meta.TokenName, quota, logContent, duration, title, httpReferer, meta.IsStream, firstWordLatency, otherInfo, xRequestID, cachedTokens, xResponseID)
		model.UpdateUserUsedQuotaAndRequestCount(meta.UserId, quota)
		model.UpdateChannelUsedQuota(meta.ChannelId, quota)
	}
}

// updateMultiKeyUsage 更新多Key使用统计
func updateMultiKeyUsage(ctx context.Context, meta *util.RelayMeta, success bool) {
	// 只有多Key模式才需要更新统计
	if !meta.IsMultiKey {
		return
	}

	// 异步更新Key使用统计，避免影响主流程性能
	go func() {
		channel, err := model.GetChannelById(meta.ChannelId, true)
		if err != nil {
			logger.Error(ctx, fmt.Sprintf("Failed to get channel %d for multi-key usage update: %s",
				meta.ChannelId, err.Error()))
			return
		}

		keyIndex := 0
		if meta.KeyIndex != nil {
			keyIndex = *meta.KeyIndex
		}
		err = channel.HandleKeyUsed(keyIndex, success)
		if err != nil {
			logger.Error(ctx, fmt.Sprintf("Failed to update multi-key usage for channel %d, key %d: %s",
				meta.ChannelId, keyIndex, err.Error()))
		}
	}()
}

// getChannelHistoryInfo 从gin.Context中获取渠道历史信息并格式化为JSON字符串
func getChannelHistoryInfo(c *gin.Context) string {
	if channelHistoryInterface, exists := c.Get("admin_channel_history"); exists {
		if channelHistory, ok := channelHistoryInterface.([]int); ok && len(channelHistory) > 0 {
			// 使用JSON格式存储，确保用逗号分隔
			if channelHistoryBytes, err := json.Marshal(channelHistory); err == nil {
				return fmt.Sprintf("adminInfo:%s", string(channelHistoryBytes))
			}
		}
	}
	return ""
}

// appendBillingDetails 向 other 字段追加计费详情 JSON
func appendBillingDetails(ctx context.Context, other string, details map[string]interface{}) string {
	if len(details) == 0 {
		return other
	}
	detailsBytes, err := json.Marshal(details)
	if err != nil {
		logger.Error(ctx, "error marshalling billing details: "+err.Error())
		return other
	}
	billingInfo := fmt.Sprintf("billingDetails:%s", string(detailsBytes))
	if other != "" {
		return other + ";" + billingInfo
	}
	return billingInfo
}

// enrichBillingDetailsFromContext 把三段折扣分量、多 Key 索引等通用字段写入 billingDetails。
// 调用方填完 billing_type / model_ratio / model_price / completion_ratio / group_ratio 等业务字段后调用。
func enrichBillingDetailsFromContext(c *gin.Context, details map[string]interface{}) map[string]interface{} {
	if details == nil {
		details = map[string]interface{}{}
	}
	channelDiscount := 1.0
	if v := c.GetFloat64("channel_discount"); v > 0 {
		channelDiscount = v
	}
	userChannelRatio := 1.0
	if v := c.GetFloat64("user_channel_ratio"); v > 0 {
		userChannelRatio = v
	}
	details["channel_discount"] = channelDiscount
	details["user_channel_ratio"] = userChannelRatio
	// 若调用方已填入 group_ratio（组合后的），反推一下 tier_ratio 方便前端显示。
	if gr, ok := details["group_ratio"].(float64); ok && channelDiscount > 0 && userChannelRatio > 0 {
		details["tier_ratio"] = gr / (channelDiscount * userChannelRatio)
	}
	if c.GetBool("is_multi_key") {
		details["is_multi_key"] = true
		if idx, ok := c.Get("key_index"); ok {
			if idxInt, valid := idx.(int); valid {
				details["key_index"] = idxInt
			}
		}
	}
	return details
}

// appendModelMappingInfo 向 other 字段追加模型重定向标记
func appendModelMappingInfo(other string, originModel string, actualModel string) string {
	if originModel == "" || actualModel == "" || originModel == actualModel {
		return other
	}
	mappingInfo := fmt.Sprintf("is_model_mapped:true;origin_model_name:%s", originModel)
	if other != "" {
		return other + ";" + mappingInfo
	}
	return mappingInfo
}

// UpdateMultiKeyUsageFromContext 从gin.Context中获取信息并更新多Key使用统计
func UpdateMultiKeyUsageFromContext(c *gin.Context, success bool) {
	isMultiKey := c.GetBool("is_multi_key")
	if !isMultiKey {
		return
	}

	channelId := c.GetInt("channel_id")
	keyIndex := c.GetInt("key_index")

	// 异步更新Key使用统计
	go func() {
		channel, err := model.GetChannelById(channelId, true)
		if err != nil {
			logger.Error(c.Request.Context(), fmt.Sprintf("Failed to get channel %d for context multi-key usage update: %s",
				channelId, err.Error()))
			return
		}

		err = channel.HandleKeyUsed(keyIndex, success)
		if err != nil {
			logger.Error(c.Request.Context(), fmt.Sprintf("Failed to update context multi-key usage for channel %d, key %d: %s",
				channelId, keyIndex, err.Error()))
		}
	}()
}
