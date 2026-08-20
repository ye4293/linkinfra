package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	dbmodel "github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/helper"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

// ensureGeminiContentsRole 确保 Gemini 请求体中的 contents 数组中每个元素都有 role 字段
// Vertex AI API 要求必须指定 role 字段（值为 "user" 或 "model"），而 Gemini 原生 API 可以省略
// 此函数用于在发送请求到 Vertex AI 之前自动补全缺失的 role 字段

// RelayOpenaiResponseNative 处理 openai 原生 API 请求
func RelayOpenaiResponseNative(c *gin.Context) *model.ErrorWithStatusCode {
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

	if err != nil {
		return openai.ErrorWrapper(err, "failed_to_get_request_body", http.StatusInternalServerError)
	}
	meta := util.GetRelayMeta(c)
	meta.ActualModelName = meta.OriginModelName
	isModelMapped := false
	if len(meta.ModelMapping) > 0 {
		if mappedModel, ok := meta.ModelMapping[meta.OriginModelName]; ok && mappedModel != "" {
			meta.ActualModelName = mappedModel
			isModelMapped = true
		}
	}
	adaptor := helper.GetAdaptor(meta.APIType)
	if adaptor == nil {
		return openai.ErrorWrapper(fmt.Errorf("invalid api type: %d", meta.APIType), "invalid_api_type", http.StatusBadRequest)
	}
	//logger.SysLog(fmt.Sprintf("openai response request: %s", string(originRequestBody)))
	var openaiResponseRequest openai.OpeanaiResaponseRequest
	if err := json.Unmarshal(originRequestBody, &openaiResponseRequest); err != nil {
		return openai.ErrorWrapper(fmt.Errorf("failed to parse claude request: %w", err), "failed_to_parse_request", http.StatusInternalServerError)
	}

	// 如果模型发生了重定向，替换请求体中的 model 字段
	if isModelMapped {
		openaiResponseRequest.Model = meta.ActualModelName
		newBody, err := json.Marshal(openaiResponseRequest)
		if err != nil {
			return openai.ErrorWrapper(err, "json_marshal_failed", http.StatusInternalServerError)
		}
		originRequestBody = newBody
		logger.Infof(ctx, "model mapping applied: %s -> %s", meta.OriginModelName, meta.ActualModelName)
	}

	meta.IsStream = openaiResponseRequest.Stream
	// 计算预消费配额
	groupRatio := util.GetBillingGroupRatio(c, group)
	modelRatio := common.GetModelRatio(modelName)
	ratio := modelRatio * groupRatio

	// 简单估算：每次请求预扣费
	preConsumedQuota, prePromptTokens, err := CalculateClaudeQuotaFromRequest(originRequestBody, modelName, ratio)
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

	var usageMetadata *openai.ResponseUsage
	var openaiErr *model.ErrorWithStatusCode

	// AWS adaptor 的 DoRequest 返回 nil, nil，因为 AWS SDK 直接处理请求
	// 这种情况下应该使用 DoResponse 来处理
	if meta.IsStream {
		usageMetadata, openaiErr = doNativeOpenaiResponseStream(c, resp, meta)
	} else {
		usageMetadata, openaiErr = doNativeOpenaiResponse(c, resp, meta)
	}

	if openaiErr != nil {
		return openaiErr
	}

	actualQuota, _ := CalculateResponseQuotaFromUsageMetadata(usageMetadata, modelName, groupRatio)

	// 记录消费日志
	duration := time.Since(startTime).Seconds()
	tokenName := c.GetString("token_name")
	promptTokens := usageMetadata.InputTokens
	completionTokens := usageMetadata.OutputTokens
	totalTokens := usageMetadata.TotalTokens
	//cachedTokens := usageMetadata.CacheCreationInputTokens + usageMetadata.CacheReadInputTokens

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

	go recordOpenaiResponseConsumption(ctx, userId, channelId, tokenId, modelName, tokenName, promptTokens, completionTokens, totalTokens, 0, actualQuota, c.Request.RequestURI, duration, meta.IsStream, c.Copy(), usageMetadata, firstWordLatency, groupRatio, modelRatio)

	return nil
}

// recordOpenaiResponseConsumption 记录 OpenAI Response API 消费日志
func recordOpenaiResponseConsumption(ctx context.Context, userId, channelId, tokenId int, modelName, tokenName string, promptTokens, completionTokens, totalTokens, cachedTokens int, quota int64, requestPath string, duration float64, isStream bool, c *gin.Context, usageMetadata *openai.ResponseUsage, firstWordLatency float64, groupRatio float64, modelRatio float64) {
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
	logContent := fmt.Sprintf("openai response API %s", requestPath)
	referer := c.Request.Header.Get("HTTP-Referer")
	title := c.Request.Header.Get("X-Title")

	// 提取用量详情并格式化为统一格式
	usageDetails := extractOpenaiReseponseNativeUsageDetails(usageMetadata)
	// 提取渠道历史信息 (adminInfo)
	adminInfo := extractAdminInfoFromContext(c)
	// 构建 other 字段，包含 adminInfo 和 usageDetails
	other := buildOpenaiResponseOtherInfoWithUsageDetails(adminInfo, usageDetails)
	// 追加模型重定向信息
	originModel := c.GetString("original_model")
	modelMapping := c.GetStringMapString("model_mapping")
	if mappedModel, ok := modelMapping[originModel]; ok && mappedModel != "" {
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
	if usageMetadata != nil && usageMetadata.InputTokensDetails != nil && usageMetadata.InputTokensDetails.CachedTokens > 0 {
		billingDetails["cached_tokens"] = usageMetadata.InputTokensDetails.CachedTokens
		billingDetails["cache_ratio"] = common.GetCacheRatio(modelName)
	}
	if usageMetadata != nil && usageMetadata.InputTokensDetails != nil && usageMetadata.InputTokensDetails.CacheWriteTokens > 0 {
		billingDetails["cache_write_tokens"] = usageMetadata.InputTokensDetails.CacheWriteTokens
		billingDetails["create_cache_ratio"] = common.GetCreateCacheRatio(modelName)
	}
	other = appendBillingDetails(ctx, other, billingDetails)
	other = util.AppendRetryHistoryOther(c, other, duration)

	dbmodel.RecordConsumeLogWithOtherAndRequestID(ctx, userId, channelId, promptTokens, completionTokens, modelName,
		tokenName, quota, logContent, duration, title, referer, isStream, firstWordLatency, other, c.GetHeader("X-Request-ID"), 0, c.GetString("x_response_id"))
}

// buildClaudeOtherInfoWithUsageDetails 构建包含 adminInfo 和 Claude usageDetails 的 otherInfo 字符串
func buildOpenaiResponseOtherInfoWithUsageDetails(adminInfo string, usageDetails *OpenaiReseponseUsageDetails) string {
	var parts []string

	if adminInfo != "" {
		parts = append(parts, adminInfo)
	}

	if usageDetails != nil {
		if detailsBytes, err := json.Marshal(usageDetails); err == nil {
			parts = append(parts, fmt.Sprintf("usageDetails:%s", string(detailsBytes)))
		}
	}

	return strings.Join(parts, ";")
}

// OpenaiReseponseUsageDetails 用于存储从 Openai Response Usage 提取的详细使用信息
type OpenaiReseponseUsageDetails struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CacheTokens      int `json:"cache_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
}

// extractOpenaiResponseNativeUsageDetails 从 Openai Response Usage 提取详细的使用信息（用于 native 接口）
func extractOpenaiReseponseNativeUsageDetails(usageMetadata *openai.ResponseUsage) *OpenaiReseponseUsageDetails {
	if usageMetadata == nil {
		return nil
	}

	details := &OpenaiReseponseUsageDetails{
		InputTokens:  usageMetadata.InputTokens,
		OutputTokens: usageMetadata.OutputTokens,
		TotalTokens:  usageMetadata.TotalTokens,
	}
	// 创建缓存和推理缓存
	if usageMetadata.InputTokensDetails != nil {
		details.CacheTokens = usageMetadata.InputTokensDetails.CachedTokens
		details.CacheWriteTokens = usageMetadata.InputTokensDetails.CacheWriteTokens
	}
	if usageMetadata.OutputTokensDetails != nil {
		details.ReasoningTokens = usageMetadata.OutputTokensDetails.ReasoningTokens
	}
	return details
}

func CalculateOpenaiResponseQuotaFromRequest(requestBody []byte, modelName string, ratio float64) (int64, int, error) {
	return 0, 0, nil
}

// ClaudeTokenCost Claude API 的费用明细（使用动态倍率计算）
type OpenaiResponseTokenCost struct {
	CacheWriteTokens int
	// 输入部分 token 数量
	InputTextTokens int // 输入文字 token 数量
	// 输出部分 token 数量
	OutputTextTokens int // 输出文字 token 数量
	// 缓存相关 token 数量
	CacheTokens     int // 缓存token 数量（总计）
	ReasoningTokens int // 推理token 数量
	TotalTokens     int // 总 token 数量
	ModelName       string
}

// CalculateOpenaiResponseQuotaByRatio 使用动态倍率计算 Openai Response API 的配额消耗
//
// ==================== 计费原理说明 ====================
//
// 【背景】
// 通过前端配置的倍率动态计算，支持灵活配置新模型价格。
//
// 【倍率定义规则】（在前端"价格设置"页面配置）
//
//   - ModelRatio（模型基础价格倍率）= 官方输入价格($/1M tokens) / 2
//     例如: Claude 3.5 Sonnet 输入 $3/1M → ModelRatio = 3 / 2 = 1.5
//
//   - CompletionRatio（输出token价格倍率）= 官方输出价格 / 官方输入价格
//     例如: Claude 3.5 Sonnet 输出 $15, 输入 $3 → CompletionRatio = 15 / 3 = 5
//
// 【缓存计费规则】
//   - 5分钟缓存创建：输入价格 × 1.25
//   - 1小时缓存创建：输入价格 × 2.0
//   - 缓存读取：输入价格 × 0.1
//
// 【配额计算公式】
//   - 输入配额 = inputTokens × ModelRatio
//   - 输出配额 = outputTokens × ModelRatio × CompletionRatio
//   - 5分钟缓存创建配额 = cache5mTokens × ModelRatio × 1.25
//   - 1小时缓存创建配额 = cache1hTokens × ModelRatio × 2.0
//   - 缓存读取配额 = cacheReadTokens × ModelRatio × 0.1
//
// 【最终配额】
//
//	总配额 = (各部分之和) / 1000000 × 2 × groupRatio × QuotaPerUnit
func CalculateOpenaiResponseQuotaByRatio(usageMetadata *openai.ResponseUsage, modelName string, groupRatio float64) (int64, OpenaiResponseTokenCost) {
	cost := OpenaiResponseTokenCost{
		ModelName: modelName,
	}

	if usageMetadata == nil {
		return 0, cost
	}

	// 提取 token 数量
	cost.InputTextTokens = usageMetadata.InputTokens
	cost.OutputTextTokens = usageMetadata.OutputTokens
	cost.TotalTokens = usageMetadata.TotalTokens

	// 创建缓存和推理缓存
	if usageMetadata.InputTokensDetails != nil {
		cost.CacheTokens = usageMetadata.InputTokensDetails.CachedTokens
		cost.CacheWriteTokens = usageMetadata.InputTokensDetails.CacheWriteTokens
	}
	if usageMetadata.OutputTokensDetails != nil {
		cost.ReasoningTokens = usageMetadata.OutputTokensDetails.ReasoningTokens
	}

	// ========== 获取各类型的倍率 ==========
	modelRatio := common.GetModelRatio(modelName)
	completionRatio := common.GetCompletionRatio(modelName)
	cacheRatio := common.GetCacheRatio(modelName)
	createCacheRatio := common.GetCreateCacheRatio(modelName)

	// 打印倍率信息
	logger.SysLog(fmt.Sprintf("[openairesponse计费] 模型: %s, 倍率配置: ModelRatio=%.4f, CompletionRatio=%.4f, CacheRatio=%.4f",
		modelName, modelRatio, completionRatio, cacheRatio))

	// 打印 token 数量
	logger.SysLog(fmt.Sprintf("[openairesponse计费] Token数量: 输入=%d, 输出=%d, 输入缓存=%d, 推理缓存=%d, 总计=%d",
		cost.InputTextTokens, cost.OutputTextTokens,
		cost.CacheTokens, cost.ReasoningTokens,
		cost.TotalTokens))

	// ========== 计算各部分的等效 ratio tokens ==========
	// 真正的输入 token = 总输入 token - 缓存 token
	realInputTokens := cost.InputTextTokens - cost.CacheTokens - cost.CacheWriteTokens
	if realInputTokens < 0 {
		realInputTokens = 0
	}

	// 输入部分（非缓存）：tokens × modelRatio
	inputTextQuota := float64(realInputTokens) * modelRatio

	// 缓存部分：cacheTokens × modelRatio × cacheRatio
	cacheQuota := float64(cost.CacheTokens) * modelRatio * cacheRatio
	cacheWriteQuota := float64(cost.CacheWriteTokens) * modelRatio * createCacheRatio

	// 输出部分：tokens × modelRatio × completionRatio
	outputTextQuota := float64(cost.OutputTextTokens) * modelRatio * completionRatio

	// 图片部分计费
	imageGenerationCallQuota := float64(0)

	// websearchtoolcall计费
	webSearchToolCallQuota := float64(0)

	// 计算最终配额
	quota := int64((inputTextQuota + cacheQuota + cacheWriteQuota + outputTextQuota + imageGenerationCallQuota + webSearchToolCallQuota) / 1000000 * 2 * groupRatio * config.QuotaPerUnit)

	return quota, cost
}

// CalculateResponseQuotaFromUsageMetadata 根据 UsageMetadata 计算配额
// 使用动态倍率计算各类型 token 的费用
func CalculateResponseQuotaFromUsageMetadata(usageMetadata *openai.ResponseUsage, modelName string, groupRatio float64) (int64, OpenaiResponseTokenCost) {
	return CalculateOpenaiResponseQuotaByRatio(usageMetadata, modelName, groupRatio)
}

// doNativeGeminiResponse 处理 Gemini 非流式响应
// parseUpstreamErrorMessage 解析上游错误响应，提取具体的错误消息
// 支持多种格式：{"error":{"message":"..."}} 或 {"message":"..."} 或纯文本

func doNativeOpenaiResponse(c *gin.Context, resp *http.Response, meta *util.RelayMeta) (usageMetadata *openai.ResponseUsage, err *model.ErrorWithStatusCode) {
	defer util.CloseResponseBodyGracefully(resp)

	// 读取响应体
	responseBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, openai.ErrorWrapper(readErr, "read_response_body_failed", http.StatusInternalServerError)
	}

	// 检查响应状态码
	if resp.StatusCode != http.StatusOK {
		// 获取系统内部的 requestID
		requestID := c.GetHeader("X-Request-ID")
		message, errType := parseUpstreamErrorMessage(responseBody, requestID)
		return nil, &model.ErrorWithStatusCode{
			Error: model.Error{
				Message: message,
				Type:    errType,
				Code:    fmt.Sprintf("status_%d", resp.StatusCode),
			},
			StatusCode: resp.StatusCode,
		}
	}

	// 解析 openai response 原生响应
	var openaiResponse openai.OpenaiResaponseResponse
	if unmarshalErr := json.Unmarshal(responseBody, &openaiResponse); unmarshalErr != nil {
		return nil, openai.ErrorWrapper(unmarshalErr, "unmarshal_response_failed", http.StatusInternalServerError)
	}
	util.IOCopyBytesGracefully(c, resp, responseBody)
	logger.Info(c.Request.Context(), fmt.Sprintf("OpenAI Response : %v", openaiResponse))
	// 缓存 response_id 到 Redis
	dbmodel.CacheResponseIdToChannel(openaiResponse.ID, c.GetInt("channel_id"), c.GetInt("key_index"), "OpenAI Response Cache")
	c.Set("x_response_id", openaiResponse.ID)

	return openaiResponse.Usage, nil
}

// doNativeOpenaiResponseStream 处理 openai response 流式响应
// claude 流式响应格式为 SSE，每行以 "data: " 开头，后跟 JSON 对象
func doNativeOpenaiResponseStream(c *gin.Context, resp *http.Response, meta *util.RelayMeta) (usageMetadata *openai.ResponseUsage, err *model.ErrorWithStatusCode) {
	defer util.CloseResponseBodyGracefully(resp)

	// 检查响应状态码 - 如果不是200，读取错误信息并返回
	if resp.StatusCode != http.StatusOK {
		responseBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, openai.ErrorWrapper(readErr, "read_error_response_failed", http.StatusInternalServerError)
		}
		// 获取系统内部的 requestID
		requestID := c.GetHeader("X-Request-ID")
		message, errType := parseUpstreamErrorMessage(responseBody, requestID)
		return nil, &model.ErrorWithStatusCode{
			Error: model.Error{
				Message: message,
				Type:    errType,
				Code:    fmt.Sprintf("status_%d", resp.StatusCode),
			},
			StatusCode: resp.StatusCode,
		}
	}

	// 用于保存最后的 UsageMetadata 和文本内容
	var lastUsageMetadata = &openai.ResponseUsage{}
	var openaiErr *model.ErrorWithStatusCode
	var fullText strings.Builder // 累积完整文本
	webSearchToolCallCount := 0
	helper.StreamScannerHandler(c, resp, meta, func(data string) bool {
		var streamResponse openai.OpenaiResponseStreamResponse
		err := json.Unmarshal([]byte(data), &streamResponse)
		if err != nil {
			openaiErr = openai.ErrorWrapper(err, "unmarshal_response_failed", http.StatusInternalServerError)
			return false
		}
		helper.OpenaiResponseChunkData(c, streamResponse, data)
		switch streamResponse.Type {
		case "response.completed":
			if streamResponse.Response != nil {
				if streamResponse.Response.Usage != nil {
					lastUsageMetadata = streamResponse.Response.Usage
				}

				// 缓存 response_id 到 Redis
				dbmodel.CacheResponseIdToChannel(streamResponse.Response.ID, c.GetInt("channel_id"), c.GetInt("key_index"), "OpenAI Response Cache Stream")
				c.Set("x_response_id", streamResponse.Response.ID)

				if len(streamResponse.Response.Output) > 0 {
					for _, output := range streamResponse.Response.Output {
						if output.Type == "image_generation_call" {
							c.Set("image_generation_call", true)
							c.Set("image_generation_call_quality", output.Quality)
							c.Set("image_generation_call_size", output.Size)
						}
					}
				}
			}
		case "response.output_text.delta":
			// 处理输出文本
			fullText.WriteString(streamResponse.Delta)
		case "response.output_item.done":
			// 函数调用处理
			if streamResponse.Item != nil {
				switch streamResponse.Item.Type {
				case "web_search_call":
					webSearchToolCallCount++
					c.Set("web_search_tool_call_count", webSearchToolCallCount)
				}
			}
		}
		return true
	})
	if lastUsageMetadata.OutputTokens == 0 {
		// 计算输出文本的 token 数量
		tempStr := fullText.String()
		if len(tempStr) > 0 {
			// 非正常结束，使用输出文本的 token 数量
			completionTokens := openai.CountTokenText(tempStr, meta.ActualModelName)
			lastUsageMetadata.OutputTokens = completionTokens
			lastUsageMetadata.TotalTokens = lastUsageMetadata.InputTokens + lastUsageMetadata.OutputTokens
		}
	}
	//特殊情况预估输入token 用于补全输入token

	if lastUsageMetadata.InputTokens == 0 && lastUsageMetadata.OutputTokens != 0 {
	}

	if openaiErr != nil {
		return lastUsageMetadata, openaiErr
	}

	return lastUsageMetadata, nil
}
