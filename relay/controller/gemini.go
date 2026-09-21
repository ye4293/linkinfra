package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/cloudflare"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/image"
	"github.com/songquanpeng/one-api/common/logger"
	dbmodel "github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/channel/gemini"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	relayconstant "github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/helper"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

// urlDownloadTask 表示一个URL下载任务
type urlDownloadTask struct {
	contentIdx       int
	partIdx          int
	url              string
	originalMimeType string
	inlineDataKey    string // 记录使用的字段名：inline_data 或 inlineData
	mimeTypeKey      string // 记录使用的 mimeType 字段名：mime_type 或 mimeType
}

// urlDownloadResult 表示URL下载结果
type urlDownloadResult struct {
	contentIdx int
	partIdx    int
	mimeType   string
	base64Data string
	mediaType  string
	err        error
}

// processGeminiInlineDataURLs 处理 Gemini 请求体中的 inlineData.data 字段
// 如果 data 字段包含 URL（http/https），则下载并转换为 base64 格式
// 如果已经是 base64 或 data URL，则保持不变
// 返回: 处理后的请求体、是否处理了URL、错误信息
func processGeminiInlineDataURLs(ctx context.Context, requestBody []byte) ([]byte, bool, error) {
	var request map[string]interface{}
	if err := json.Unmarshal(requestBody, &request); err != nil {
		return requestBody, false, err
	}

	contents, ok := request["contents"].([]interface{})
	if !ok {
		return requestBody, false, nil
	}

	// 第一步：收集所有需要下载的URL任务
	var downloadTasks []urlDownloadTask
	for i, content := range contents {
		contentMap, ok := content.(map[string]interface{})
		if !ok {
			continue
		}

		parts, ok := contentMap["parts"].([]interface{})
		if !ok {
			continue
		}

		for j, part := range parts {
			partMap, ok := part.(map[string]interface{})
			if !ok {
				continue
			}

			// 兼容两种命名格式：inline_data（下划线）和 inlineData（驼峰）
			var inlineData map[string]interface{}
			var inlineDataKey string

			if data, ok := partMap["inline_data"].(map[string]interface{}); ok {
				inlineData = data
				inlineDataKey = "inline_data"
			} else if data, ok := partMap["inlineData"].(map[string]interface{}); ok {
				inlineData = data
				inlineDataKey = "inlineData"
			} else {
				continue
			}

			data, ok := inlineData["data"].(string)
			if !ok || data == "" {
				continue
			}

			// 尝试判断是否为 URL
			var finalURL string

			// 1. 先检查是否直接就是 URL
			if strings.HasPrefix(data, "http://") || strings.HasPrefix(data, "https://") {
				finalURL = data
			} else {
				// 2. 尝试 base64 解码，看是否是编码后的 URL
				decodedData, err := base64.StdEncoding.DecodeString(data)
				if err == nil {
					decodedStr := string(decodedData)
					if strings.HasPrefix(decodedStr, "http://") || strings.HasPrefix(decodedStr, "https://") {
						finalURL = decodedStr
					}
				}
			}

			// 如果不是 URL（直接或解码后），跳过
			if finalURL == "" {
				continue
			}

			// 获取原始的 mimeType（同时兼容 mime_type 和 mimeType）
			var originalMimeType string
			var mimeTypeKey string
			if mt, ok := inlineData["mime_type"].(string); ok {
				originalMimeType = mt
				mimeTypeKey = "mime_type"
			} else if mt, ok := inlineData["mimeType"].(string); ok {
				originalMimeType = mt
				mimeTypeKey = "mimeType"
			} else {
				// 如果没有指定，根据 inlineDataKey 使用相应的默认格式
				if inlineDataKey == "inline_data" {
					mimeTypeKey = "mime_type"
				} else {
					mimeTypeKey = "mimeType"
				}
			}

			logger.Infof(ctx, "Found %s with URL, originalMimeType: %s", inlineDataKey, originalMimeType)

			// 添加到下载任务列表
			downloadTasks = append(downloadTasks, urlDownloadTask{
				contentIdx:       i,
				partIdx:          j,
				url:              finalURL,
				originalMimeType: originalMimeType,
				inlineDataKey:    inlineDataKey,
				mimeTypeKey:      mimeTypeKey,
			})
		}
	}

	// 如果没有需要下载的URL，直接返回
	if len(downloadTasks) == 0 {
		return requestBody, false, nil
	}

	logger.Infof(ctx, "Found %d URL(s) to download, starting concurrent download...", len(downloadTasks))

	// 第二步：并发下载所有URL
	results := make([]urlDownloadResult, len(downloadTasks))
	var wg sync.WaitGroup

	for idx, task := range downloadTasks {
		wg.Add(1)
		go func(index int, t urlDownloadTask) {
			defer wg.Done()

			logger.Infof(ctx, "Downloading URL [%d/%d]: %s", index+1, len(downloadTasks), t.url)

			// 使用现有的图片处理函数下载并转换
			mimeType, base64Data, mediaType, err := image.GetGeminiMediaInfo(t.url)
			if err != nil {
				logger.Warnf(ctx, "Failed to download URL [%d/%d]: %v, URL: %s", index+1, len(downloadTasks), err, t.url)
				results[index] = urlDownloadResult{
					contentIdx: t.contentIdx,
					partIdx:    t.partIdx,
					err:        err,
				}
				return
			}

			// 去除 base64Data 中可能存在的 data URL 前缀
			if strings.Contains(base64Data, ";base64,") {
				splitParts := strings.Split(base64Data, ";base64,")
				if len(splitParts) == 2 {
					base64Data = splitParts[1]
				}
			} else if strings.HasPrefix(base64Data, "data:") {
				if commaIdx := strings.Index(base64Data, ","); commaIdx != -1 {
					base64Data = base64Data[commaIdx+1:]
				}
			}

			results[index] = urlDownloadResult{
				contentIdx: t.contentIdx,
				partIdx:    t.partIdx,
				mimeType:   mimeType,
				base64Data: base64Data,
				mediaType:  mediaType,
				err:        nil,
			}

			logger.Infof(ctx, "Successfully downloaded URL [%d/%d]: mediaType=%s, mimeType=%s, size=%d bytes",
				index+1, len(downloadTasks), mediaType, mimeType, len(base64Data))
		}(idx, task)
	}

	// 等待所有下载完成
	wg.Wait()
	logger.Infof(ctx, "All %d URL downloads completed", len(downloadTasks))

	// 第三步：应用下载结果到原始数据
	successCount := 0
	for idx, result := range results {
		if result.err != nil {
			continue
		}

		task := downloadTasks[idx]
		contentMap := contents[result.contentIdx].(map[string]interface{})
		parts := contentMap["parts"].([]interface{})
		partMap := parts[result.partIdx].(map[string]interface{})

		// 使用保存的 key 来访问 inlineData
		inlineData := partMap[task.inlineDataKey].(map[string]interface{})

		// 更新 mimeType（如果原本没有设置），使用保存的 mimeTypeKey
		if task.originalMimeType == "" && result.mimeType != "" {
			inlineData[task.mimeTypeKey] = result.mimeType
		}

		// 更新 data 为 base64 格式
		inlineData["data"] = result.base64Data

		// 使用保存的 key 来更新 inlineData
		partMap[task.inlineDataKey] = inlineData
		parts[result.partIdx] = partMap
		contentMap["parts"] = parts
		contents[result.contentIdx] = contentMap

		successCount++
	}

	if successCount > 0 {
		logger.Infof(ctx, "Successfully processed %d/%d URLs", successCount, len(downloadTasks))
		request["contents"] = contents
		modifiedBody, err := json.Marshal(request)
		return modifiedBody, true, err
	}

	return requestBody, false, nil
}

// ensureGeminiContentsRole 确保 Gemini 请求体中的 contents 数组中每个元素都有 role 字段
// Vertex AI API 要求必须指定 role 字段（值为 "user" 或 "model"），而 Gemini 原生 API 可以省略
// 此函数用于在发送请求到 Vertex AI 之前自动补全缺失的 role 字段
func ensureGeminiContentsRole(requestBody []byte) ([]byte, error) {
	var request map[string]interface{}
	if err := json.Unmarshal(requestBody, &request); err != nil {
		return requestBody, err
	}

	contents, ok := request["contents"].([]interface{})
	if !ok {
		return requestBody, nil
	}

	modified := false
	for i, content := range contents {
		contentMap, ok := content.(map[string]interface{})
		if !ok {
			continue
		}

		// 如果没有 role 字段或者 role 为空，设置默认值 "user"
		if role, exists := contentMap["role"]; !exists || role == nil || role == "" {
			contentMap["role"] = "user"
			contents[i] = contentMap
			modified = true
		}
	}

	if modified {
		request["contents"] = contents
		return json.Marshal(request)
	}

	return requestBody, nil
}

// RelayGeminiNative 处理 Gemini 原生 API 请求
func RelayGeminiNative(c *gin.Context) *model.ErrorWithStatusCode {
	ctx := c.Request.Context()
	startTime := time.Now()

	// 获取基本信息
	tokenId := c.GetInt("token_id")
	userId := c.GetInt("id")
	group := c.GetString("group")
	channelId := c.GetInt("channel_id")
	modelName := c.GetString("original_model")

	//获取原生requestbody
	originRequestBody, err := common.GetRequestBody(c)
	//logger.Infof(ctx, "originRequestBody: %s", string(originRequestBody))
	if err != nil {
		return openai.ErrorWrapper(err, "failed_to_get_request_body", http.StatusInternalServerError)
	}
	meta := util.GetRelayMeta(c)

	// 如果是 Vertex AI 渠道，需要确保 contents 中的每个元素都有 role 字段
	// Vertex AI API 要求必须指定 role（"user" 或 "model"），而 Gemini 原生 API 可以省略
	if meta.APIType == relayconstant.APITypeVertexAI {
		processedBody, err := ensureGeminiContentsRole(originRequestBody)
		if err != nil {
			logger.Warnf(ctx, "Failed to process request body for Vertex AI role field: %v", err)
		} else {
			originRequestBody = processedBody
		}
	}
	//如果R2存储启用，则处理inlineData中的URL格式图片
	if config.CfR2storeEnabled {
		// 处理 inlineData 中的 URL 格式图片，自动下载并转换为 base64
		processedBody, processedURL, err := processGeminiInlineDataURLs(ctx, originRequestBody)
		if err != nil {
			logger.Warnf(ctx, "Failed to process inlineData URLs: %v", err)
		} else {
			originRequestBody = processedBody
			// 设置标记表示通过 URL 方式处理了图片
			if processedURL {
				c.Set("gemini_inline_data_url_processed", true)
				logger.Infof(ctx, "Gemini inlineData URL processing flag set")
			}
		}
	}
	adaptor := helper.GetAdaptor(meta.APIType)
	if adaptor == nil {
		return openai.ErrorWrapper(fmt.Errorf("invalid api type: %d", meta.APIType), "invalid_api_type", http.StatusBadRequest)
	}

	// 计算预消费配额
	groupRatio := util.GetBillingGroupRatio(c, group, modelName)
	modelRatio := common.GetModelRatio(modelName)
	ratio := modelRatio * groupRatio

	// 简单估算：每次请求预扣费
	preConsumedQuota, prePromptTokens, err := CalculateGeminiQuotaFromRequest(originRequestBody, modelName, ratio)
	if err != nil {
		return openai.ErrorWrapper(err, "failed_to_calculate_pre_consumed_quota", http.StatusInternalServerError)
	}

	userQuota, err := dbmodel.CacheGetUserQuota(ctx, userId)
	if err != nil {
		return openai.ErrorWrapper(err, "failed_to_get_user_quota", http.StatusInternalServerError)
	}

	if userQuota < preConsumedQuota {
		return openai.ErrorWrapper(fmt.Errorf("insufficient quota"), "insufficient_quota", http.StatusForbidden)
	}

	meta.PromptTokens = prePromptTokens
	//先写死透传

	adaptor.Init(meta)
	resp, err := adaptor.DoRequest(c, meta, bytes.NewBuffer(originRequestBody))
	if err != nil {
		return openai.ErrorWrapper(err, "failed_to_send_request", http.StatusBadGateway)
	}

	var usageMetadata *gemini.UsageMetadata
	var openaiErr *model.ErrorWithStatusCode

	if meta.IsStream {
		usageMetadata, openaiErr = doNativeGeminiStreamResponse(c, resp, meta)
	} else {
		usageMetadata, openaiErr = doNativeGeminiResponse(c, resp, meta)
	}

	if openaiErr != nil {
		return openaiErr
	}

	actualQuota, _ := CalculateGeminiQuotaFromUsageMetadata(usageMetadata, modelName, groupRatio)

	//logger.Infof(ctx, "Gemini actual quota: %d, total tokens: %d", actualQuota, usage.TotalTokens)
	// 记录消费日志
	duration := time.Since(startTime).Seconds()
	tokenName := c.GetString("token_name")
	promptTokens := usageMetadata.PromptTokenCount
	completionTokens := usageMetadata.CandidatesTokenCount + usageMetadata.ThoughtsTokenCount
	totalTokens := usageMetadata.TotalTokenCount
	cachedTokens := usageMetadata.CachedContentTokenCount

	// 计算首字延迟：优先使用总请求开始时间（包含重试），否则使用 meta 中的时间
	var firstWordLatency float64
	if totalStartTime, exists := c.Get("total_request_start_time"); exists {
		if startTime, ok := totalStartTime.(time.Time); ok && !meta.FirstResponseTime.IsZero() {
			// 使用总请求开始时间到首字响应时间的间隔
			firstWordLatency = meta.FirstResponseTime.Sub(startTime).Seconds()
		} else {
			firstWordLatency = meta.GetFirstWordLatency()
		}
	} else {
		firstWordLatency = meta.GetFirstWordLatency()
	}

	ctx = dbmodel.WithRankingUsage(ctx, meta.BillingModelName(), meta.Config.Provider, int64(promptTokens)+int64(completionTokens))
	go recordGeminiConsumption(ctx, userId, channelId, tokenId, modelName, tokenName, promptTokens, completionTokens, totalTokens, cachedTokens, actualQuota, c.Request.RequestURI, duration, meta.IsStream, c.Copy(), usageMetadata, firstWordLatency, groupRatio, modelRatio)
	return nil
}

// recordGeminiConsumption 记录 Gemini 消费日志
func recordGeminiConsumption(ctx context.Context, userId, channelId, tokenId int, modelName, tokenName string, promptTokens, completionTokens, totalTokens, cachedTokens int, quota int64, requestPath string, duration float64, isStream bool, c *gin.Context, usageMetadata *gemini.UsageMetadata, firstWordLatency float64, groupRatio float64, modelRatio float64) {
	err := dbmodel.PostConsumeTokenQuota(tokenId, quota)
	if err != nil {
		logger.Error(ctx,
			"error consuming token remain quota: "+err.Error())
	}

	err = dbmodel.CacheUpdateUserQuota(ctx, userId)
	if err != nil {
		logger.Error(ctx,
			"error update user quota cache: "+err.Error())
	}

	dbmodel.UpdateUserUsedQuotaAndRequestCount(userId, quota)
	dbmodel.UpdateChannelUsedQuota(channelId, quota)

	// 记录日志
	logContent := fmt.Sprintf("Gemini API %s", requestPath)
	referer := c.Request.Header.Get("HTTP-Referer")
	title := c.Request.Header.Get("X-Title")

	// 提取用量详情并格式化为统一格式
	usageDetails := extractGeminiNativeUsageDetails(usageMetadata)
	adminInfo := extractAdminInfoFromContext(c)
	other := buildOtherInfoWithUsageDetails(adminInfo, usageDetails)
	// 追加模型重定向信息
	originModel := c.GetString("original_model")
	modelMapping := c.GetStringMapString("model_mapping")
	if mappedModel, ok := modelMapping[originModel]; ok {
		other = appendModelMappingInfo(other, originModel, mappedModel)
	}
	// 追加计费详情
	billingDetails := map[string]interface{}{
		"billing_type":     "token",
		"model_ratio":      modelRatio,
		"completion_ratio": common.GetCompletionRatio(modelName),
		"group_ratio":      groupRatio,
	}
	billingDetails = enrichBillingDetailsFromContext(c, billingDetails)
	if cachedTokens > 0 {
		billingDetails["cached_tokens"] = cachedTokens
		billingDetails["cache_ratio"] = common.GetCacheRatio(modelName)
	}
	other = appendBillingDetails(ctx, other, billingDetails)
	other = util.AppendRetryHistoryOther(c, other, duration)

	dbmodel.RecordConsumeLogWithOtherAndRequestID(ctx, userId, channelId, promptTokens, completionTokens, modelName,
		tokenName, quota, logContent, duration, title, referer, isStream, firstWordLatency, other, c.GetHeader("X-Request-ID"), cachedTokens, c.GetString("x_response_id"))
}

// extractGeminiNativeUsageDetails 从 Gemini UsageMetadata 提取详细的使用信息（用于 native 接口）
func extractGeminiNativeUsageDetails(usageMetadata *gemini.UsageMetadata) *GeminiUsageDetails {
	if usageMetadata == nil {
		return nil
	}

	details := &GeminiUsageDetails{
		ReasoningTokens: usageMetadata.ThoughtsTokenCount,
		CachedTokens:    usageMetadata.CachedContentTokenCount,
	}

	// 从 promptTokensDetails 提取 input_text 和 input_image
	for _, d := range usageMetadata.PromptTokensDetails {
		switch d.Modality {
		case "TEXT":
			details.InputTextTokens = d.TokenCount
		case "IMAGE":
			details.InputImageTokens = d.TokenCount
		}
	}

	// 从 candidatesTokensDetails 提取 output_image
	for _, d := range usageMetadata.CandidatesTokensDetails {
		if d.Modality == "IMAGE" {
			details.OutputImageTokens = d.TokenCount
		}
	}

	// 计算 output_text = candidatesTokenCount - output_image
	// 如果没有 IMAGE，output_text 就等于 candidatesTokenCount
	details.OutputTextTokens = usageMetadata.CandidatesTokenCount - details.OutputImageTokens
	if details.OutputTextTokens < 0 {
		details.OutputTextTokens = 0
	}

	return details
}

func CalculateGeminiQuotaFromRequest(requestBody []byte, modelName string, ratio float64) (int64, int, error) {
	var geminiReq gemini.ChatRequest
	if err := json.Unmarshal(requestBody, &geminiReq); err != nil {
		return 0, 0, fmt.Errorf("failed to parse gemini request: %w", err)
	}

	// 估算输入 tokens
	estimatedTokens := 0

	// 计算内容的 tokens
	for _, content := range geminiReq.Contents {
		for _, part := range content.Parts {
			if part.Text != "" {
				estimatedTokens += openai.CountTokenText(part.Text, modelName)
			}
			// 图片大约 258 tokens (Gemini 固定值)
			if part.InlineData != nil && strings.HasPrefix(part.InlineData.MimeType, "image") {
				estimatedTokens += 258
			}
			// 视频按文件大小估算（这里简化处理）
			if part.InlineData != nil && strings.HasPrefix(part.InlineData.MimeType, "video") {
				estimatedTokens += 1000 // 视频大约1000+ tokens
			}
			// 音频文件
			if part.InlineData != nil && strings.HasPrefix(part.InlineData.MimeType, "audio") {
				estimatedTokens += 500
			}
			// PDF 文件
			if part.InlineData != nil && part.InlineData.MimeType == "application/pdf" {
				estimatedTokens += 500 // PDF 按页数估算，这里简化处理
			}
		}
	}

	// // 估算输出 tokens (根据 maxOutputTokens)
	// estimatedOutputTokens := 1000 // 默认估算
	// if geminiReq.GenerationConfig.MaxOutputTokens > 0 {
	// 	estimatedOutputTokens = geminiReq.GenerationConfig.MaxOutputTokens
	// }

	// // 总 tokens = 输入 + 预估输出
	// totalEstimatedTokens := estimatedTokens + estimatedOutputTokens

	// 计算预消费配额 (tokens * ratio / 1000000 * 2 * QuotaPerUnit)
	// ratio 是基于每 100万 tokens 的价格/2，需要除以 1000000 再乘以 2 还原
	// 预消费按 30% 估算，避免预扣太多
	quota := int64(float64(estimatedTokens) * ratio * 0.3 / 1000000 * 2 * config.QuotaPerUnit)

	return quota, estimatedTokens, nil
}

// countTextTokens 简单的 token 计数（粗略估算）
// 中文: 1个字 ≈ 1.5 tokens
// 英文: 1个词 ≈ 1.3 tokens
// 这里简化为: 每4个字符 ≈ 1 token

// GeminiTokenCost 表示 Gemini API 的费用明细（使用动态倍率计算）
type GeminiTokenCost struct {
	// 输入部分 token 数量
	InputTextTokens  int // 输入文字 token 数量
	InputImageTokens int // 输入图片 token 数量
	InputAudioTokens int // 输入音频 token 数量
	// 输出部分 token 数量
	OutputTextTokens  int // 输出文字 token 数量
	OutputImageTokens int // 输出图片 token 数量
	OutputAudioTokens int // 输出音频 token 数量
	ThinkingTokens    int // 思考 token 数量
	CachedTokens      int // 缓存 token 数量
	TotalTokens       int // 总 token 数量
	ModelName         string
}

// CalculateGeminiQuotaByRatio 使用动态倍率计算 Gemini API 的配额消耗
//
// ==================== 计费原理说明 ====================
//
// 【背景】
// 之前的计费方式是硬编码各模型的价格，新增模型需要修改代码。
// 新方案通过前端配置的倍率动态计算，支持灵活配置新模型价格。
//
// 【倍率定义规则】（在前端"价格设置"页面配置）
//
//   - ModelRatio（模型基础价格倍率）= 官方输入价格($/1M tokens) / 2
//     例如: Gemini 2.5 Flash 输入 $0.30/1M → ModelRatio = 0.30 / 2 = 0.15
//
//   - CompletionRatio（输出token价格倍率）= 官方输出价格 / 官方输入价格
//     例如: Gemini 2.5 Flash 输出 $2.50, 输入 $0.30 → CompletionRatio = 2.50 / 0.30 ≈ 8.33
//
//   - ImageInputRatio（图片输入倍率）= 图片输入价格 / 文字输入价格
//     如果模型图片输入价格与文字相同，则设为 1.0
//
//   - ImageOutputRatio（图片输出倍率）= 图片输出价格 / 文字输入价格
//     注意：这是相对于"输入"的倍率，不是相对于"输出文字"的倍率
//     例如: 图片输出 $6/1M, 文字输入 $0.10/1M → ImageOutputRatio = 60
//
//   - AudioInputRatio / AudioOutputRatio 同理
//
// 【配额计算公式】
//   - 输入文字配额 = (inputTextTokens - cachedTokens) × ModelRatio
//   - 缓存读取配额 = cachedTokens × ModelRatio × CacheRatio
//   - 输入图片配额 = inputImageTokens × ModelRatio × ImageInputRatio
//   - 输入音频配额 = inputAudioTokens × ModelRatio × AudioInputRatio
//   - 输出文字配额 = outputTextTokens × ModelRatio × CompletionRatio
//   - 输出图片配额 = outputImageTokens × ModelRatio × ImageOutputRatio
//     ⚠️ 注意：输出图片不乘 CompletionRatio，因为 ImageOutputRatio 已经是相对于输入的完整倍率
//   - 输出音频配额 = outputAudioTokens × ModelRatio × AudioOutputRatio（同上）
//   - 思考配额 = thinkingTokens × ModelRatio × CompletionRatio
//
// 【最终配额】
//
//	总配额 = (各部分之和) / 1000000 × 2 × groupRatio × QuotaPerUnit
//	- 除以 1000000：因为倍率是基于每百万 token 的价格
//	- 乘以 2：因为 ModelRatio = 官方价格 / 2，需要还原真实价格
//	- groupRatio：用户分组的价格倍率
//	- QuotaPerUnit：系统配额单位（如 500000 表示 $1 = 500000 配额）
//
// 【计算示例】Gemini 2.5 Flash, 1000输入+500输出, groupRatio=1.0, QuotaPerUnit=500000
//
//	输入配额 = 1000 × 0.15 = 150
//	输出配额 = 500 × 0.15 × 8.33 = 624.75
//	总配额 = (150 + 624.75) / 1000000 × 2 × 1.0 × 500000 ≈ 775
func CalculateGeminiQuotaByRatio(usageMetadata *gemini.UsageMetadata, modelName string, groupRatio float64) (int64, GeminiTokenCost) {
	cost := GeminiTokenCost{
		ModelName: modelName,
	}

	if usageMetadata == nil {
		return 0, cost
	}

	// 从 UsageMetadata 中提取各类型 token 数量
	cost.CachedTokens = usageMetadata.CachedContentTokenCount
	cost.TotalTokens = usageMetadata.TotalTokenCount
	cost.ThinkingTokens = usageMetadata.ThoughtsTokenCount

	// 解析输入 token 详情 (PromptTokensDetails)
	for _, detail := range usageMetadata.PromptTokensDetails {
		switch detail.Modality {
		case "TEXT":
			cost.InputTextTokens = detail.TokenCount
		case "IMAGE":
			cost.InputImageTokens = detail.TokenCount
		case "AUDIO":
			cost.InputAudioTokens = detail.TokenCount
		}
	}

	// 如果没有详细信息，则所有输入都算作文字
	if cost.InputTextTokens == 0 && cost.InputImageTokens == 0 && cost.InputAudioTokens == 0 {
		cost.InputTextTokens = usageMetadata.PromptTokenCount
	}

	// 解析输出 token 详情 (CandidatesTokensDetails)
	for _, detail := range usageMetadata.CandidatesTokensDetails {
		switch detail.Modality {
		case "TEXT":
			cost.OutputTextTokens = detail.TokenCount
		case "IMAGE":
			cost.OutputImageTokens = detail.TokenCount
		case "AUDIO":
			cost.OutputAudioTokens = detail.TokenCount
		}
	}

	// 如果没有详细信息，计算输出文字 = candidatesTokenCount - 输出图片 - 输出音频
	if cost.OutputTextTokens == 0 && cost.OutputImageTokens == 0 && cost.OutputAudioTokens == 0 {
		cost.OutputTextTokens = usageMetadata.CandidatesTokenCount
	} else if cost.OutputTextTokens == 0 {
		// 有图片或音频输出，但没有文字输出的详情
		cost.OutputTextTokens = usageMetadata.CandidatesTokenCount - cost.OutputImageTokens - cost.OutputAudioTokens
		if cost.OutputTextTokens < 0 {
			cost.OutputTextTokens = 0
		}
	}

	// ========== 获取各类型的倍率 ==========
	modelRatio := common.GetModelRatio(modelName)
	imageInputRatio := common.GetImageInputRatio(modelName)
	audioInputRatio := common.GetAudioInputRatio(modelName)
	completionRatio := common.GetCompletionRatio(modelName)
	imageOutputRatio := common.GetImageOutputRatio(modelName)
	audioOutputRatio := common.GetAudioOutputRatio(modelName)
	cacheRatio := common.GetCacheRatio(modelName)

	// ========== 计算各部分的等效 ratio tokens ==========
	// 输入部分：cachedTokens 是 promptTokenCount 的子集，需从输入文字中扣除
	nonCachedInputTextTokens := cost.InputTextTokens - cost.CachedTokens
	if nonCachedInputTextTokens < 0 {
		nonCachedInputTextTokens = 0
	}
	inputTextQuota := float64(nonCachedInputTextTokens) * modelRatio
	// 缓存读取部分：按 cacheRatio 折扣计费
	cacheQuota := float64(cost.CachedTokens) * modelRatio * cacheRatio
	inputImageQuota := float64(cost.InputImageTokens) * modelRatio * imageInputRatio
	inputAudioQuota := float64(cost.InputAudioTokens) * modelRatio * audioInputRatio

	// 输出文字部分：tokens × modelRatio × completionRatio
	outputTextQuota := float64(cost.OutputTextTokens) * modelRatio * completionRatio

	// 输出图片/音频部分：tokens × modelRatio × 对应OutputRatio
	// ImageOutputRatio/AudioOutputRatio 已经是相对于输入文字的倍率，不需要再乘 completionRatio
	outputImageQuota := float64(cost.OutputImageTokens) * modelRatio * imageOutputRatio
	outputAudioQuota := float64(cost.OutputAudioTokens) * modelRatio * audioOutputRatio

	// 思考 token：与输出文字相同的倍率
	thinkingQuota := float64(cost.ThinkingTokens) * modelRatio * completionRatio

	if cost.CachedTokens > 0 {
		logger.SysLog(fmt.Sprintf("[Gemini计费] 模型: %s, 缓存Token: %d, 非缓存输入Token: %d, CacheRatio=%.4f, 缓存配额=%.2f, 输入配额=%.2f",
			modelName, cost.CachedTokens, nonCachedInputTextTokens, cacheRatio, cacheQuota, inputTextQuota))
	}

	// ========== 计算最终配额 ==========
	// 公式: 总RatioTokens / 1000000 × 2 × groupRatio × QuotaPerUnit
	// 乘以 2 是因为 ModelRatio = 官方价格 / 2，需要还原真实价格
	totalRatioTokens := inputTextQuota + cacheQuota + inputImageQuota + inputAudioQuota +
		outputTextQuota + outputImageQuota + outputAudioQuota + thinkingQuota

	quota := int64(totalRatioTokens / 1000000 * 2 * groupRatio * config.QuotaPerUnit)

	return quota, cost
}

// CalculateGeminiQuotaFromUsageMetadata 根据 UsageMetadata 计算配额
// 使用动态倍率计算各类型 token 的费用
func CalculateGeminiQuotaFromUsageMetadata(usageMetadata *gemini.UsageMetadata, modelName string, groupRatio float64) (int64, GeminiTokenCost) {
	return CalculateGeminiQuotaByRatio(usageMetadata, modelName, groupRatio)
}

// processGeminiResponseImages 处理 Gemini 响应中的图片，上传到 R2 并替换为 URL
// 参数: ctx - 上下文, responseBody - 原始响应体字节数组
// 返回: 处理后的响应体字节数组, 错误信息
func processGeminiResponseImages(ctx context.Context, responseBody []byte) ([]byte, error) {
	// 解析响应体为 map
	var response map[string]interface{}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return responseBody, fmt.Errorf("failed to unmarshal response: %v", err)
	}

	// 获取 candidates 数组
	candidates, ok := response["candidates"].([]interface{})
	if !ok || len(candidates) == 0 {
		return responseBody, nil // 没有 candidates，直接返回
	}

	modified := false
	for i, candidateItem := range candidates {
		candidate, ok := candidateItem.(map[string]interface{})
		if !ok {
			continue
		}

		// 获取 content
		content, ok := candidate["content"].(map[string]interface{})
		if !ok {
			continue
		}

		// 获取 parts 数组
		parts, ok := content["parts"].([]interface{})
		if !ok {
			continue
		}

		for j, partItem := range parts {
			part, ok := partItem.(map[string]interface{})
			if !ok {
				continue
			}

			// 检查是否有 inlineData
			inlineData, ok := part["inlineData"].(map[string]interface{})
			if !ok {
				continue
			}

			// 获取 mimeType 和 data
			mimeType, _ := inlineData["mimeType"].(string)
			base64Data, ok := inlineData["data"].(string)
			if !ok || base64Data == "" {
				continue
			}

			logger.Infof(ctx, "Found image in response, uploading to R2 (mimeType: %s, size: %d bytes)",
				mimeType, len(base64Data))

			// 上传到 R2
			url, err := cloudflare.UploadImageToR2(ctx, base64Data, mimeType)
			if err != nil {
				logger.Warnf(ctx, "Failed to upload image to R2: %v, keeping base64", err)
				continue
			}

			// 将 URL 转换为 base64 编码
			urlBase64 := base64.StdEncoding.EncodeToString([]byte(url))

			// 用 base64 编码的 URL 替换原始数据
			inlineData["data"] = urlBase64
			part["inlineData"] = inlineData
			parts[j] = part
			content["parts"] = parts
			candidate["content"] = content
			candidates[i] = candidate

			logger.Infof(ctx, "Successfully replaced image with base64-encoded R2 URL: %s", url)
			modified = true
		}
	}

	if modified {
		logger.Infof(ctx, "Gemini response images processed and uploaded to R2")
		// 更新 response 中的 candidates
		response["candidates"] = candidates
		// 重新序列化为 JSON
		modifiedBody, err := json.Marshal(response)
		if err != nil {
			return responseBody, fmt.Errorf("failed to marshal modified response: %v", err)
		}
		return modifiedBody, nil
	}

	// 没有修改，返回原始响应体
	return responseBody, nil
}

// doNativeGeminiResponse 处理 Gemini 非流式响应
func doNativeGeminiResponse(c *gin.Context, resp *http.Response, meta *util.RelayMeta) (usageMetadata *gemini.UsageMetadata, err *model.ErrorWithStatusCode) {
	defer util.CloseResponseBodyGracefully(resp)

	// 读取响应体
	responseBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, openai.ErrorWrapper(readErr, "read_response_body_failed", http.StatusInternalServerError)
	}

	// 检查响应状态码
	if resp.StatusCode != http.StatusOK {
		return nil, &model.ErrorWithStatusCode{
			Error: model.Error{
				Message: string(responseBody),
				Type:    "gemini_api_error",
				Code:    fmt.Sprintf("status_%d", resp.StatusCode),
			},
			StatusCode: resp.StatusCode,
		}
	}

	// 解析 Gemini 原生响应
	var geminiResponse gemini.ChatResponse
	if unmarshalErr := json.Unmarshal(responseBody, &geminiResponse); unmarshalErr != nil {
		return nil, openai.ErrorWrapper(unmarshalErr, "unmarshal_response_failed", http.StatusInternalServerError)
	}
	if geminiResponse.ResponseId != "" {
		c.Set("x_response_id", geminiResponse.ResponseId)
	}
	//如果没有处理过inlineData中的URL格式图片，则直接返回
	if c.GetBool("gemini_inline_data_url_processed") == false {
		logger.Infof(c, "Gemini response images not processed, returning original response")
		util.IOCopyBytesGracefully(c, resp, responseBody)
		return geminiResponse.UsageMetadata, nil
	}

	// 处理响应中的图片，上传到 R2 并替换为 URL
	ctx := c.Request.Context()
	modifiedResponse, processErr := processGeminiResponseImages(ctx, responseBody)
	if processErr != nil {
		logger.Warnf(ctx, "Failed to process response images: %v, using original response", processErr)
		util.IOCopyBytesGracefully(c, resp, responseBody)
	} else {
		util.IOCopyBytesGracefully(c, resp, modifiedResponse)
	}

	return geminiResponse.UsageMetadata, nil

}

// doNativeGeminiStreamResponse 处理 Gemini 流式响应
// Gemini 流式响应格式为 SSE，每行以 "data: " 开头，后跟 JSON 对象
func doNativeGeminiStreamResponse(c *gin.Context, resp *http.Response, meta *util.RelayMeta) (usageMetadata *gemini.UsageMetadata, err *model.ErrorWithStatusCode) {
	defer util.CloseResponseBodyGracefully(resp)
	// 检查响应状态码
	if resp.StatusCode != http.StatusOK {
		responseBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, openai.ErrorWrapper(readErr, "read_error_response_failed", http.StatusInternalServerError)
		}
		return nil, &model.ErrorWithStatusCode{
			Error: model.Error{
				Message: string(responseBody),
				Type:    "gemini_api_error",
				Code:    fmt.Sprintf("status_%d", resp.StatusCode),
			},
			StatusCode: resp.StatusCode,
		}
	}
	// 设置 SSE 响应头
	//common.SetEventStreamHeaders(c)
	// 用于保存最后的 UsageMetadata
	var lastUsageMetadata = &gemini.UsageMetadata{}

	var imageCount int
	var SendResponseCount int
	responseText := strings.Builder{}

	helper.StreamScannerHandler(c, resp, meta, func(data string) bool {
		var geminiResponse gemini.ChatResponse
		//var data1 := `{"candidates": [{"content": {"role": "model","parts": [{"text": "**AI learns from data to find patterns and make predictions.**"}]},"finishReason": "STOP"}],"usageMetadata": {"promptTokenCount": 8,"candidatesTokenCount": 12,"totalTokenCount": 1191,"trafficType": "ON_DEMAND","promptTokensDetails": [{"modality": "TEXT","tokenCount": 8}],"candidatesTokensDetails": [{"modality": "TEXT","tokenCount": 12}],"thoughtsTokenCount": 1171},"modelVersion": "gemini-2.5-pro","createTime": "2025-12-11T20:48:01.865079Z","responseId": "AS47abfmNISn998PvLHjwAs"}`
		//err := json.Unmarshal([]byte(data1), &geminiResponse)
		err := json.Unmarshal([]byte(data), &geminiResponse)

		if err != nil {
			logger.Error(c, "error unmarshalling stream response: "+err.Error())
			return false
		}

		// 统计图片数量
		for _, candidate := range geminiResponse.Candidates {
			for _, part := range candidate.Content.Parts {
				if part.InlineData != nil && part.InlineData.MimeType != "" {
					imageCount++
				}
				if part.Text != "" {
					responseText.WriteString(part.Text)
				}
			}
		}

		// 更新使用量统计
		if geminiResponse.UsageMetadata != nil {
			lastUsageMetadata = geminiResponse.UsageMetadata
		}
		if geminiResponse.ResponseId != "" {
			c.Set("x_response_id", geminiResponse.ResponseId)
		}

		// 直接返回原始数据，不处理图片
		err = helper.StringData(c, data)
		if err != nil {
			logger.Error(c, err.Error())
		}
		SendResponseCount++
		return true
	})

	if SendResponseCount == 0 {
		return nil, openai.ErrorWrapper(errors.New("no response received from Gemini API"), "write_stream_failed", http.StatusInternalServerError)
	}

	if imageCount != 0 {
		if lastUsageMetadata.CandidatesTokenCount == 0 {
			lastUsageMetadata.CandidatesTokenCount = imageCount * 258
		}
	}

	// 如果usage.CompletionTokens为0，则使用本地统计的completion tokens
	if lastUsageMetadata != nil && lastUsageMetadata.CandidatesTokenCount == 0 {
		str := responseText.String()
		if len(str) > 0 {
			lastUsageMetadata.CandidatesTokenCount = openai.CountTokenText(responseText.String(), meta.OriginModelName)
		}
	}

	return lastUsageMetadata, nil
}

// FormatGeminiCost 格式化输出 Gemini 费用明细（用于日志或调试）
func FormatGeminiCost(cost GeminiTokenCost) string {
	var builder strings.Builder

	builder.WriteString(fmt.Sprintf("Model: %s | ", cost.ModelName))

	// 输入部分
	totalInputTokens := cost.InputTextTokens + cost.InputImageTokens + cost.InputAudioTokens
	builder.WriteString(fmt.Sprintf("Input: %d tokens (text:%d, image:%d, audio:%d) | ",
		totalInputTokens, cost.InputTextTokens, cost.InputImageTokens, cost.InputAudioTokens))

	if cost.CachedTokens > 0 {
		builder.WriteString(fmt.Sprintf("Cached: %d tokens | ", cost.CachedTokens))
	}

	// 输出部分
	totalOutputTokens := cost.OutputTextTokens + cost.OutputImageTokens + cost.OutputAudioTokens
	builder.WriteString(fmt.Sprintf("Output: %d tokens (text:%d, image:%d, audio:%d) | ",
		totalOutputTokens, cost.OutputTextTokens, cost.OutputImageTokens, cost.OutputAudioTokens))

	if cost.ThinkingTokens > 0 {
		builder.WriteString(fmt.Sprintf("Thinking: %d tokens | ", cost.ThinkingTokens))
	}

	builder.WriteString(fmt.Sprintf("Total: %d tokens", cost.TotalTokens))

	return builder.String()
}
