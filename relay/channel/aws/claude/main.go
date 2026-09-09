// Package aws provides the AWS adaptor for the relay service.
package aws

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/gin-gonic/gin"
	"github.com/jinzhu/copier"
	"github.com/pkg/errors"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/ctxkey"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/relay/cache"
	"github.com/songquanpeng/one-api/relay/channel/anthropic"
	"github.com/songquanpeng/one-api/relay/channel/aws/utils"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	relaymodel "github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

// https://docs.aws.amazon.com/bedrock/latest/userguide/model-ids.html
var AwsModelIDMap = map[string]string{
	// Legacy Claude models
	"claude-instant-1.2": "anthropic.claude-instant-v1",
	"claude-2.0":         "anthropic.claude-v2",
	"claude-2.1":         "anthropic.claude-v2:1",
	// Claude 3 models
	"claude-3-sonnet-20240229":   "anthropic.claude-3-sonnet-20240229-v1:0",
	"claude-3-opus-20240229":     "anthropic.claude-3-opus-20240229-v1:0",
	"claude-3-haiku-20240307":    "anthropic.claude-3-haiku-20240307-v1:0",
	"claude-3-5-sonnet-20240620": "anthropic.claude-3-5-sonnet-20240620-v1:0",
	"claude-3-5-sonnet-20241022": "anthropic.claude-3-5-sonnet-20241022-v2:0",
	"claude-3-5-haiku-20241022":  "anthropic.claude-3-5-haiku-20241022-v1:0",
	"claude-3-7-sonnet-20250219": "anthropic.claude-3-7-sonnet-20250219-v1:0",
	// Claude 4 models
	"claude-sonnet-4-20250514":   "anthropic.claude-sonnet-4-20250514-v1:0",
	"claude-opus-4-20250514":     "anthropic.claude-opus-4-20250514-v1:0",
	"claude-opus-4-1-20250805":   "anthropic.claude-opus-4-1-20250805-v1:0",
	"claude-sonnet-4-5-20250929": "anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-haiku-4-5-20251001":  "anthropic.claude-haiku-4-5-20251001-v1:0",
	"claude-opus-4-5-20251101":   "anthropic.claude-opus-4-5-20251101-v1:0",
	"claude-opus-4-6":            "anthropic.claude-opus-4-6-v1",
	"claude-sonnet-4-6":          "anthropic.claude-sonnet-4-6",
	"claude-opus-4-7":            "anthropic.claude-opus-4-7",
	"claude-opus-4-8":            "anthropic.claude-opus-4-8",

	// Claude models with thinking (extended thinking) - 使用相同的模型ID，通过请求参数启用思考模式
	"claude-3-7-sonnet-20250219-thinking": "anthropic.claude-3-7-sonnet-20250219-v1:0",
	"claude-sonnet-4-20250514-thinking":   "anthropic.claude-sonnet-4-20250514-v1:0",
	"claude-opus-4-20250514-thinking":     "anthropic.claude-opus-4-20250514-v1:0",
	"claude-opus-4-1-20250805-thinking":   "anthropic.claude-opus-4-1-20250805-v1:0",
	"claude-sonnet-4-5-20250929-thinking": "anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-haiku-4-5-20251001-thinking":  "anthropic.claude-haiku-4-5-20251001-v1:0",
	"claude-opus-4-5-20251101-thinking":   "anthropic.claude-opus-4-5-20251101-v1:0",
	"claude-opus-4-6-thinking":            "anthropic.claude-opus-4-6-v1",
	"claude-sonnet-4-6-thinking":          "anthropic.claude-sonnet-4-6",
	"claude-opus-4-7-thinking":            "anthropic.claude-opus-4-7",
	"claude-opus-4-8-thinking":            "anthropic.claude-opus-4-8",
}

// GetAwsModelID 获取 AWS 模型ID，如果没有映射则返回原始模型名
func GetAwsModelID(requestModel string) string {
	if awsModelID, ok := AwsModelIDMap[requestModel]; ok {
		return awsModelID
	}
	return requestModel
}

func awsModelID(requestModel string) (string, error) {
	// 1. 先检查是否在预定义映射中
	if awsModelID, ok := AwsModelIDMap[requestModel]; ok {
		return awsModelID, nil
	}

	// 2. 如果是 ARN 格式（通过模型重定向配置的 inference profile），直接使用
	//    去掉 -thinking 后缀，thinking 模式通过请求参数启用，ARN 本身不区分
	if strings.HasPrefix(requestModel, "arn:") {
		return strings.TrimSuffix(requestModel, "-thinking"), nil
	}

	// 3. 如果不在映射中，检查是否已经是 AWS 模型 ID 格式
	//    支持格式：anthropic.xxx, us.anthropic.xxx, eu.anthropic.xxx, apac.anthropic.xxx, global.anthropic.xxx
	if strings.Contains(requestModel, "anthropic.") {
		// 去掉 -thinking 后缀，AWS Bedrock 不识别该后缀，thinking 模式通过请求参数启用
		return strings.TrimSuffix(requestModel, "-thinking"), nil
	}

	// 4. 都不匹配则返回错误
	return "", errors.Errorf("model %s not found in mapping and is not a valid AWS model ID", requestModel)
}

// GetAwsRegionPrefix 从区域ID中提取区域前缀（如 us-east-1 -> us）
func GetAwsRegionPrefix(awsRegionId string) string {
	parts := strings.Split(awsRegionId, "-")
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

// AwsModelCanCrossRegion 检查模型是否支持指定区域的跨区域调用
func AwsModelCanCrossRegion(awsModelId, awsRegionPrefix string) bool {
	regionSet, exists := AwsModelCanCrossRegionMap[awsModelId]
	return exists && regionSet[awsRegionPrefix]
}

// AwsModelCrossRegion 为模型添加跨区域前缀
func AwsModelCrossRegion(awsModelId, awsRegionPrefix string) string {
	modelPrefix, found := AwsRegionCrossModelPrefixMap[awsRegionPrefix]
	if !found {
		return awsModelId
	}
	return modelPrefix + "." + awsModelId
}

// getAwsModelIdWithRegion 获取 AWS 模型 ID 并应用跨区域前缀（如果需要）
func getAwsModelIdWithRegion(c *gin.Context, requestModel string) (string, error) {
	awsModelId, err := awsModelID(requestModel)
	if err != nil {
		return "", err
	}

	// 获取 region 并应用跨区域前缀（如果需要）
	if region, exists := c.Get("aws_region"); exists {
		if regionStr, ok := region.(string); ok && regionStr != "" {
			regionPrefix := GetAwsRegionPrefix(regionStr)
			if AwsModelCanCrossRegion(awsModelId, regionPrefix) {
				awsModelId = AwsModelCrossRegion(awsModelId, regionPrefix)
			}
		}
	}

	return awsModelId, nil
}

func truncateBytes(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}

// parseNativeClaudeRequest 从请求体解析原生 Claude 请求
func parseNativeClaudeRequest(c *gin.Context) (map[string]any, error) {
	requestBody, err := common.GetRequestBody(c)
	if err != nil {
		return nil, errors.Wrap(err, "get request body")
	}

	var awsClaudeReq map[string]any
	if err := json.Unmarshal(requestBody, &awsClaudeReq); err != nil {
		return nil, errors.Wrap(err, "unmarshal request body")
	}
	if awsClaudeReq == nil {
		awsClaudeReq = make(map[string]any)
	}
	return awsClaudeReq, nil
}

func buildNativeClaudeRequestBody(c *gin.Context) ([]byte, error) {
	awsClaudeReq, err := parseNativeClaudeRequest(c)
	if err != nil {
		return nil, errors.Wrap(err, "parse native claude request")
	}

	delete(awsClaudeReq, "model")
	delete(awsClaudeReq, "stream")
	awsClaudeReq["anthropic_version"] = "bedrock-2023-05-31"

	betaHeader := c.GetHeader("anthropic-beta")
	bodyBeta := awsClaudeReq["anthropic_beta"]
	delete(awsClaudeReq, "anthropic_beta")

	betaFlags := anthropic.MergeBetaFlags(betaHeader, awsClaudeReq, anthropic.BedrockAllowedBetaFlags)
	logger.Infof(c, "[Bedrock Beta] header=%q, bodyBeta=%v, filtered=%v", betaHeader, bodyBeta, betaFlags)
	if len(betaFlags) > 0 {
		betaJSON, marshalErr := anthropic.MarshalBetaFlags(betaFlags)
		if marshalErr != nil {
			return nil, errors.Wrap(marshalErr, "marshal anthropic-beta")
		}
		awsClaudeReq["anthropic_beta"] = json.RawMessage(betaJSON)
	}

	// Opus 4.7+ 适配：
	// 1) thinking.type=enabled 不被接受，需改写为 adaptive；无论 type 如何，4.7 都不接受 budget_tokens
	// 2) temperature / top_p / top_k 均已废弃，API 会 400
	// 4.6 adaptive 仍接受 temperature，所以这里只对 4.7+ 动手
	// 其他模型维持原逻辑：thinking 存在时强制 temperature=1
	requestModel := c.GetString(ctxkey.RequestModel)
	if anthropic.IsNoSamplingModel(requestModel) {
		if thinking, ok := awsClaudeReq["thinking"].(map[string]any); ok {
			if t, _ := thinking["type"].(string); t == "enabled" {
				thinking["type"] = "adaptive"
			}
			// caller 即使已经用 adaptive 也可能顺带传了 budget_tokens，统一清理
			delete(thinking, "budget_tokens")
		}
		delete(awsClaudeReq, "temperature")
		delete(awsClaudeReq, "top_p")
		delete(awsClaudeReq, "top_k")
	} else if thinking, exists := awsClaudeReq["thinking"]; exists && thinking != nil {
		// thinking 模式要求 temperature 必须为 1（4.6 adaptive 下设 1 也无害）
		awsClaudeReq["temperature"] = 1.0
	}

	requestBody, err := json.Marshal(awsClaudeReq)
	if err != nil {
		return nil, errors.Wrap(err, "marshal native claude request")
	}
	return requestBody, nil
}

func Handler(c *gin.Context, awsCli *bedrockruntime.Client, meta *util.RelayMeta) (*relaymodel.ErrorWithStatusCode, *relaymodel.Usage) {
	awsModelId, err := getAwsModelIdWithRegion(c, c.GetString(ctxkey.RequestModel))
	if err != nil {
		return utils.WrapErr(errors.Wrap(err, "getAwsModelIdWithRegion")), nil
	}

	awsReq := &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(awsModelId),
		Accept:      aws.String("application/json"),
		ContentType: aws.String("application/json"),
	}

	// 尝试从 context 获取转换后的请求（OpenAI 格式转换）
	// 如果没有，则从请求体直接解析（原生 Claude 格式）
	var requestBody []byte
	claudeReq_, ok := c.Get(ctxkey.ConvertedRequest)
	if ok {
		claudeReq := claudeReq_.(*anthropic.Request)
		awsClaudeReq := &Request{
			AnthropicVersion: "bedrock-2023-05-31",
		}
		if err = copier.Copy(awsClaudeReq, claudeReq); err != nil {
			return utils.WrapErr(errors.Wrap(err, "copy request")), nil
		}
		betaHeader := c.GetHeader("anthropic-beta")
		betaFlags := anthropic.MergeBetaFlags(betaHeader, nil, anthropic.BedrockAllowedBetaFlags)
		logger.Infof(c, "[Bedrock Beta] OpenAI path (non-stream): header=%q, filtered=%v", betaHeader, betaFlags)
		if len(betaFlags) > 0 {
			betaJSON, marshalErr := anthropic.MarshalBetaFlags(betaFlags)
			if marshalErr != nil {
				return utils.WrapErr(errors.Wrap(marshalErr, "marshal anthropic-beta")), nil
			}
			awsClaudeReq.AnthropicBeta = betaJSON
		}

		// thinking 模式要求 temperature 必须为 1（仅 4.6 adaptive 与更早 enabled，4.7+ 不接受 temperature）
		if anthropic.IsNoSamplingModel(c.GetString(ctxkey.RequestModel)) {
			// copier.Copy turns temperature=0 into a non-nil pointer; omit it entirely.
			awsClaudeReq.Temperature = nil
			awsClaudeReq.TopP = 0
			awsClaudeReq.TopK = 0
		} else if awsClaudeReq.Thinking != nil {
			temperatureOne := 1.0
			awsClaudeReq.Temperature = &temperatureOne
		}

		// 直接序列化请求，let omitempty handle zero values
		requestBody, err = json.Marshal(awsClaudeReq)
		if err != nil {
			return utils.WrapErr(errors.Wrap(err, "marshal request")), nil
		}
	} else {
		requestBody, err = buildNativeClaudeRequestBody(c)
		if err != nil {
			return utils.WrapErr(err), nil
		}
	}
	awsReq.Body = requestBody
	logger.Infof(c, "[Bedrock Beta] final request body (first 500): %s", truncateBytes(requestBody, 500))

	awsResp, err := invokeModelWithSamplingFallback(c.Request.Context(), awsCli, awsReq)
	if err != nil {
		return utils.WrapErr(errors.Wrap(err, "InvokeModel")), nil
	}

	claudeResponse := new(anthropic.Response)
	err = json.Unmarshal(awsResp.Body, claudeResponse)
	if err != nil {
		return utils.WrapErr(errors.Wrap(err, "unmarshal response")), nil
	}

	openaiResp := anthropic.ResponseClaude2OpenAI(claudeResponse)
	openaiResp.Model = c.GetString(ctxkey.RequestModel)
	usage := relaymodel.Usage{
		PromptTokens:     claudeResponse.Usage.InputTokens,
		CompletionTokens: claudeResponse.Usage.OutputTokens,
		TotalTokens:      claudeResponse.Usage.InputTokens + claudeResponse.Usage.OutputTokens,
	}
	openaiResp.Usage = usage

	c.JSON(http.StatusOK, openaiResp)
	return nil, &usage
}

func StreamHandler(c *gin.Context, awsCli *bedrockruntime.Client, meta *util.RelayMeta) (*relaymodel.ErrorWithStatusCode, *relaymodel.Usage) {
	createdTime := helper.GetTimestamp()
	awsModelId, err := getAwsModelIdWithRegion(c, c.GetString(ctxkey.RequestModel))
	if err != nil {
		return utils.WrapErr(errors.Wrap(err, "getAwsModelIdWithRegion")), nil
	}

	awsReq := &bedrockruntime.InvokeModelWithResponseStreamInput{
		ModelId:     aws.String(awsModelId),
		Accept:      aws.String("application/json"),
		ContentType: aws.String("application/json"),
	}

	// 尝试从 context 获取转换后的请求（OpenAI 格式转换）
	// 如果没有，则从请求体直接解析（原生 Claude 格式）
	var requestBody []byte
	claudeReq_, ok := c.Get(ctxkey.ConvertedRequest)
	if ok {
		claudeReq := claudeReq_.(*anthropic.Request)
		awsClaudeReq := &Request{
			AnthropicVersion: "bedrock-2023-05-31",
		}
		if err = copier.Copy(awsClaudeReq, claudeReq); err != nil {
			return utils.WrapErr(errors.Wrap(err, "copy request")), nil
		}
		betaHeader := c.GetHeader("anthropic-beta")
		betaFlags := anthropic.MergeBetaFlags(betaHeader, nil, anthropic.BedrockAllowedBetaFlags)
		logger.Infof(c, "[Bedrock Beta] OpenAI path (stream): header=%q, filtered=%v", betaHeader, betaFlags)
		if len(betaFlags) > 0 {
			betaJSON, marshalErr := anthropic.MarshalBetaFlags(betaFlags)
			if marshalErr != nil {
				return utils.WrapErr(errors.Wrap(marshalErr, "marshal anthropic-beta")), nil
			}
			awsClaudeReq.AnthropicBeta = betaJSON
		}

		// thinking 模式要求 temperature 必须为 1（仅 4.6 adaptive 与更早 enabled，4.7+ 不接受 temperature）
		if anthropic.IsNoSamplingModel(c.GetString(ctxkey.RequestModel)) {
			// copier.Copy turns temperature=0 into a non-nil pointer; omit it entirely.
			awsClaudeReq.Temperature = nil
			awsClaudeReq.TopP = 0
			awsClaudeReq.TopK = 0
		} else if awsClaudeReq.Thinking != nil {
			temperatureOne := 1.0
			awsClaudeReq.Temperature = &temperatureOne
		}

		// 直接序列化请求，let omitempty handle zero values
		requestBody, err = json.Marshal(awsClaudeReq)
		if err != nil {
			return utils.WrapErr(errors.Wrap(err, "marshal request")), nil
		}
	} else {
		requestBody, err = buildNativeClaudeRequestBody(c)
		if err != nil {
			return utils.WrapErr(err), nil
		}
	}
	awsReq.Body = requestBody
	logger.Infof(c, "[Bedrock Beta] final stream request body (first 500): %s", truncateBytes(requestBody, 500))

	awsResp, err := invokeStreamWithSamplingFallback(c.Request.Context(), awsCli, awsReq)
	if err != nil {
		return utils.WrapErr(errors.Wrap(err, "InvokeModelWithResponseStream")), nil
	}
	stream := awsResp.GetStream()
	defer stream.Close()

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	var usage relaymodel.Usage
	var id string
	var lastToolCallChoice openai.ChatCompletionsStreamResponseChoice

	var modelName string
	c.Stream(func(w io.Writer) bool {
		event, ok := <-stream.Events()
		if !ok {
			// 如果需要包含 usage 信息，在流结束时发送一个包含 usage 的最终 chunk
			if meta != nil && meta.ShouldIncludeUsage {
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
				// 防御性处理：如果 id 或 modelName 为空，使用默认值
				responseId := id
				if responseId == "" {
					responseId = helper.GetUUID()
				}
				responseName := modelName
				if responseName == "" {
					responseName = meta.ActualModelName
				}
				finalResponse := &openai.ChatCompletionsStreamResponse{
					Id:      responseId,
					Object:  "chat.completion.chunk",
					Created: createdTime,
					Model:   responseName,
					Choices: []openai.ChatCompletionsStreamResponseChoice{},
					Usage:   &usage,
				}
				jsonStr, marshalErr := json.Marshal(finalResponse)
				if marshalErr != nil {
					logger.Error(c.Request.Context(), "error marshalling final usage chunk: "+marshalErr.Error())
				} else {
					c.Render(-1, common.CustomEvent{Data: "data: " + string(jsonStr)})
				}
			}
			c.Render(-1, common.CustomEvent{Data: "data: [DONE]"})
			return false
		}

		switch v := event.(type) {
		case *types.ResponseStreamMemberChunk:
			claudeResp := new(anthropic.StreamResponse)
			err := json.NewDecoder(bytes.NewReader(v.Value.Bytes)).Decode(claudeResp)
			if err != nil {
				logger.Error(c.Request.Context(), "error unmarshalling stream response: "+err.Error())
				return false
			}

			response, respMeta := anthropic.StreamResponseClaude2OpenAI(claudeResp)
			if respMeta != nil {
				usage.PromptTokens += respMeta.Usage.InputTokens
				usage.CompletionTokens += respMeta.Usage.OutputTokens
				if len(respMeta.Id) > 0 { // only message_start has an id, otherwise it's a finish_reason event.
					id = respMeta.Id
					modelName = respMeta.Model
					c.Set("x_response_id", id)
					return true
				} else { // finish_reason case
					if len(lastToolCallChoice.Delta.ToolCalls) > 0 {
						lastArgs := &lastToolCallChoice.Delta.ToolCalls[len(lastToolCallChoice.Delta.ToolCalls)-1].Function
						if len(lastArgs.Arguments.(string)) == 0 { // compatible with OpenAI sending an empty object `{}` when no arguments.
							lastArgs.Arguments = "{}"
							response.Choices[len(response.Choices)-1].Delta.Content = nil
							response.Choices[len(response.Choices)-1].Delta.ToolCalls = lastToolCallChoice.Delta.ToolCalls
						}
					}
				}
			}
			if response == nil {
				return true
			}
			response.Id = id
			response.Model = c.GetString(ctxkey.OriginalModel)
			response.Created = createdTime

			for _, choice := range response.Choices {
				if len(choice.Delta.ToolCalls) > 0 {
					lastToolCallChoice = choice
				}
			}
			jsonStr, err := json.Marshal(response)
			if err != nil {
				logger.Error(c.Request.Context(), "error marshalling stream response: "+err.Error())
				return true
			}
			c.Render(-1, common.CustomEvent{Data: "data: " + string(jsonStr)})
			return true
		case *types.UnknownUnionMember:
			logger.Error(c.Request.Context(), "AWS stream unknown union member tag: "+v.Tag)
			return false
		default:
			logger.Error(c.Request.Context(), "AWS stream union is nil or unknown type")
			return false
		}
	})

	return nil, &usage
}

// NativeHandler 处理 Claude Native 非流式请求，返回 Claude 原生格式
func NativeHandler(c *gin.Context, awsCli *bedrockruntime.Client, meta *util.RelayMeta) (*relaymodel.ErrorWithStatusCode, *relaymodel.Usage) {
	awsModelId, err := getAwsModelIdWithRegion(c, c.GetString(ctxkey.RequestModel))
	if err != nil {
		return utils.WrapErr(errors.Wrap(err, "getAwsModelIdWithRegion")), nil
	}

	awsReq := &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(awsModelId),
		Accept:      aws.String("application/json"),
		ContentType: aws.String("application/json"),
	}

	// 原生 Claude 请求，直接从请求体解析
	awsReq.Body, err = buildNativeClaudeRequestBody(c)
	if err != nil {
		return utils.WrapErr(err), nil
	}

	awsResp, err := invokeModelWithSamplingFallback(c.Request.Context(), awsCli, awsReq)
	if err != nil {
		return utils.WrapErr(errors.Wrap(err, "InvokeModel")), nil
	}

	// 解析响应以获取 usage
	claudeResponse := new(anthropic.Response)
	if jsonErr := json.Unmarshal(awsResp.Body, claudeResponse); jsonErr != nil {
		return utils.WrapErr(errors.Wrap(jsonErr, "unmarshal response")), nil
	}

	usage := relaymodel.Usage{
		PromptTokens:     claudeResponse.Usage.InputTokens,
		CompletionTokens: claudeResponse.Usage.OutputTokens,
		TotalTokens:      claudeResponse.Usage.InputTokens + claudeResponse.Usage.OutputTokens,
	}

	if claudeResponse.Id != "" {
		c.Set("x_response_id", claudeResponse.Id)
	}
	if claudeResponse.Usage != nil {
		// 保留原始 anthropic.Usage 供上层计费/日志使用（包含 cache 字段）
		c.Set("claude_usage_metadata", claudeResponse.Usage)
		logger.Info(c.Request.Context(), fmt.Sprintf("[Claude Cache Debug] aws NativeHandler 准备调用handleClaudeCache - ResponseID: %s, InputTokens: %d, OutputTokens: %d", claudeResponse.Id, claudeResponse.Usage.InputTokens, claudeResponse.Usage.OutputTokens))
		cache.HandleClaudeCache(c, claudeResponse.Id, claudeResponse.Usage)
	} else {
		logger.Info(c.Request.Context(), fmt.Sprintf("[Claude Cache Debug] aws NativeHandler Usage为空，跳过缓存处理 - ResponseID: %s", claudeResponse.Id))
	}

	// 直接返回 Claude 原生格式响应
	c.Header("Content-Type", "application/json")
	if _, writeErr := c.Writer.Write(awsResp.Body); writeErr != nil {
		logger.Error(c.Request.Context(), "error writing response: "+writeErr.Error())
	}
	return nil, &usage
}

// NativeStreamHandler 处理 Claude Native 流式请求，返回 Claude 原生格式
func NativeStreamHandler(c *gin.Context, awsCli *bedrockruntime.Client, meta *util.RelayMeta) (*relaymodel.ErrorWithStatusCode, *relaymodel.Usage) {
	awsModelId, err := getAwsModelIdWithRegion(c, c.GetString(ctxkey.RequestModel))
	if err != nil {
		return utils.WrapErr(errors.Wrap(err, "getAwsModelIdWithRegion")), nil
	}

	awsReq := &bedrockruntime.InvokeModelWithResponseStreamInput{
		ModelId:     aws.String(awsModelId),
		Accept:      aws.String("application/json"),
		ContentType: aws.String("application/json"),
	}

	// 原生 Claude 请求，直接从请求体解析
	awsReq.Body, err = buildNativeClaudeRequestBody(c)
	if err != nil {
		return utils.WrapErr(err), nil
	}

	awsResp, err := invokeStreamWithSamplingFallback(c.Request.Context(), awsCli, awsReq)
	if err != nil {
		return utils.WrapErr(errors.Wrap(err, "InvokeModelWithResponseStream")), nil
	}
	stream := awsResp.GetStream()
	defer stream.Close()

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")

	var usage relaymodel.Usage
	// 保留完整的 anthropic.Usage（含 cache 字段），流结束时回填到 gin.Context
	var claudeUsage *anthropic.Usage

	for {
		event, ok := <-stream.Events()
		if !ok {
			break
		}

		switch v := event.(type) {
		case *types.ResponseStreamMemberChunk:
			// 解析 Claude 响应以获取 usage 和事件类型
			claudeResp := new(anthropic.StreamResponse)
			eventType := "message"
			if jsonErr := json.NewDecoder(bytes.NewReader(v.Value.Bytes)).Decode(claudeResp); jsonErr == nil {
				eventType = claudeResp.Type
				// 安全地获取 usage 信息，避免 nil 指针
				if claudeResp.Type == "message_start" && claudeResp.Message != nil && claudeResp.Message.Usage != nil {
					usage.PromptTokens = claudeResp.Message.Usage.InputTokens
					claudeUsage = claudeResp.Message.Usage
					if claudeResp.Message.Id != "" {
						c.Set("x_response_id", claudeResp.Message.Id)
					}
					// 判断是否创建或读取了缓存，并记录到 redis 中
					logger.Info(c.Request.Context(), fmt.Sprintf("[Claude Cache Debug] aws NativeStreamHandler 准备调用handleClaudeCache(流式) - ResponseID: %s, InputTokens: %d", claudeResp.Message.Id, usage.TotalTokens))
					cache.HandleClaudeCache(c, claudeResp.Message.Id, claudeResp.Message.Usage)
				}
				if claudeResp.Type == "message_delta" && claudeResp.Usage != nil {
					usage.CompletionTokens = claudeResp.Usage.OutputTokens
					usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
					if claudeUsage != nil {
						claudeUsage.OutputTokens = claudeResp.Usage.OutputTokens
					}
				}
			} else {
				// JSON 解析失败时记录错误
				logger.Error(c.Request.Context(), "error parsing claude stream response: "+jsonErr.Error())
			}

			// 直接透传 Claude 原生格式的 SSE 事件
			// 格式: event: <type>\ndata: <json>\n\n
			if _, writeErr := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", eventType, string(v.Value.Bytes)); writeErr != nil {
				logger.Error(c.Request.Context(), "error writing stream response: "+writeErr.Error())
			}
			c.Writer.Flush()
		case *types.UnknownUnionMember:
			logger.Error(c.Request.Context(), "unknown tag: "+v.Tag)
		default:
			logger.Error(c.Request.Context(), "union is nil or unknown type")
		}
	}

	if claudeUsage != nil {
		c.Set("claude_usage_metadata", claudeUsage)
	}

	return nil, &usage
}
