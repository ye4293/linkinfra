package anthropic

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/songquanpeng/one-api/common/render"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/image"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

func stopReasonClaude2OpenAI(reason *string) string {
	if reason == nil {
		return ""
	}
	switch *reason {
	case "end_turn":
		return "stop"
	case "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "pause_turn":
		// 长时间运行的 turn 被暂停，可将响应原样传回以继续
		return "stop"
	case "refusal":
		// 流式分类器干预，处理潜在的策略违规
		return "stop"
	default:
		return *reason
	}
}

func ConvertRequest(textRequest model.GeneralOpenAIRequest) *Request {
	claudeTools := make([]Tool, 0, len(textRequest.Tools))

	for _, tool := range textRequest.Tools {
		if params, ok := tool.Function.Parameters.(map[string]any); ok {
			var required []string
			if reqVal, ok := params["required"]; ok {
				if reqArr, ok := reqVal.([]interface{}); ok {
					for _, r := range reqArr {
						if s, ok := r.(string); ok {
							required = append(required, s)
						}
					}
				} else if reqStrArr, ok := reqVal.([]string); ok {
					required = reqStrArr
				}
			}
			typeStr := "object"
			if t, ok := params["type"].(string); ok {
				typeStr = t
			}
			claudeTools = append(claudeTools, Tool{
				Name:        tool.Function.Name,
				Description: tool.Function.Description,
				InputSchema: InputSchema{
					Type:       typeStr,
					Properties: params["properties"],
					Required:   required,
				},
			})
		}
	}

	// 处理 MaxTokens：优先使用 MaxCompletionTokens（OpenAI 新格式），其次使用 MaxTokens
	maxTokens := textRequest.MaxTokens
	if textRequest.MaxCompletionTokens > 0 {
		maxTokens = textRequest.MaxCompletionTokens
	}

	// 判断是否是 thinking 模型
	isThinking := IsThinkingModel(textRequest.Model)
	// 获取实际模型名称（去除 -thinking 后缀）
	actualModel := GetBaseModelName(textRequest.Model)

	claudeRequest := Request{
		Model:       actualModel,
		MaxTokens:   maxTokens,
		Temperature: textRequest.Temperature,
		TopP:        textRequest.TopP,
		TopK:        textRequest.TopK,
		Stream:      textRequest.Stream,
		Tools:       claudeTools,
	}

	// 转换 OpenAI response_format 到 Claude output_config
	if textRequest.ResponseFormat != nil && textRequest.ResponseFormat.Type == "json_schema" {
		claudeRequest.OutputConfig = &OutputConfig{
			Format: &JSONOutputFormat{
				Type:   "json_schema",
				Schema: textRequest.ResponseFormat.JSONSchema,
			},
		}
	}

	// 为 thinking 模型添加 thinking 配置
	if isThinking && config.ClaudeThinkingEnabled {
		// 1. 如果用户未传 max_tokens，使用配置的默认值
		if claudeRequest.MaxTokens == 0 {
			claudeRequest.MaxTokens = common.GetClaudeDefaultMaxTokens(textRequest.Model)
		}

		if IsAdaptiveThinkingModel(actualModel) {
			// 4.7+ 模型：使用 adaptive thinking（不支持 budget_tokens）
			// display 设为 summarized，确保响应中包含思考内容（4.7 默认 omitted）
			claudeRequest.Thinking = &ThinkingConfig{
				Type:    "adaptive",
				Display: "summarized",
			}
		} else {
			// 4.6 及更早模型：使用 enabled thinking + budget_tokens
			budgetRatio := config.ClaudeThinkingBudgetRatio
			if textRequest.ReasoningEffort != "" {
				budgetRatio = common.GetClaudeThinkingBudgetRatio(textRequest.ReasoningEffort)
			}
			thinkingBudget := int(float64(claudeRequest.MaxTokens) * budgetRatio)
			if thinkingBudget < 1024 {
				thinkingBudget = 1024
			}
			claudeRequest.Thinking = &ThinkingConfig{
				Type:         "enabled",
				BudgetTokens: thinkingBudget,
			}
		}
		// thinking 模式要求 temperature 必须为 1，且不能设置 top_p 和 top_k
		// https://docs.anthropic.com/en/docs/build-with-claude/extended-thinking#important-considerations
		claudeRequest.Temperature = 1.0
		claudeRequest.TopP = 0
		claudeRequest.TopK = 0
	}

	// 将 reasoning_effort 映射到 output_config.effort（所有模型通用，不限于 thinking 模型）
	if textRequest.ReasoningEffort != "" {
		effort := MapReasoningEffortToOutputEffort(textRequest.ReasoningEffort)
		if claudeRequest.OutputConfig == nil {
			claudeRequest.OutputConfig = &OutputConfig{}
		}
		claudeRequest.OutputConfig.Effort = effort
	}

	// Opus 4.7+ 模型不支持 temperature/top_p/top_k（API 会 400：temperature is deprecated for this model）
	// 注意：4.6 走 adaptive 但仍接受 temperature，不要用 IsAdaptiveThinkingModel 判断
	if IsNoSamplingModel(actualModel) {
		claudeRequest.Temperature = 0
		claudeRequest.TopP = 0
		claudeRequest.TopK = 0
	}
	if stop, ok := textRequest.Stop.(string); ok && stop != "" {
		claudeRequest.StopSequences = []string{stop}
	}
	if len(claudeTools) > 0 {
		claudeToolChoice := &ToolChoice{Type: "auto"} // default value https://docs.anthropic.com/en/docs/build-with-claude/tool-use#controlling-claudes-output
		if choice, ok := textRequest.ToolChoice.(map[string]any); ok {
			if function, ok := choice["function"].(map[string]any); ok {
				claudeToolChoice.Type = "tool"
				if name, ok := function["name"].(string); ok {
					claudeToolChoice.Name = name
				}
			}
		} else if toolChoiceType, ok := textRequest.ToolChoice.(string); ok {
			// OpenAI tool_choice 到 Claude tool_choice 的映射：
			// "auto" -> "auto" (模型自行决定)
			// "required" -> "any" (必须调用工具)
			// "any" -> "any" (兼容已使用 Claude 格式的请求)
			// "none" -> 不设置 tool_choice (当前保持 auto，Claude 会自行判断)
			switch toolChoiceType {
			case "required", "any":
				claudeToolChoice.Type = "any"
			case "auto":
				claudeToolChoice.Type = "auto"
			case "none":
				// 对于 none，不设置 tool_choice，让 Claude 自行判断
				// 但由于有 tools，仍然可能调用，如果完全不想调用需要不传 tools
				claudeToolChoice = nil
			}
		}
		if claudeToolChoice != nil {
			claudeRequest.ToolChoice = claudeToolChoice
		}
	}
	if claudeRequest.MaxTokens == 0 {
		claudeRequest.MaxTokens = 4096
	}
	// legacy model name mapping
	if claudeRequest.Model == "claude-instant-1" {
		claudeRequest.Model = "claude-instant-1.1"
	} else if claudeRequest.Model == "claude-2" {
		claudeRequest.Model = "claude-2.1"
	}
	// 标记是否已经出现过 user/assistant 消息（用于判断 system/developer 是否在对话开头）
	conversationStarted := false
	for _, message := range textRequest.Messages {
		if message.Role == "system" || message.Role == "developer" {
			if !conversationStarted {
				// 对话开始前的 system/developer 消息，合并到顶层 system 参数
				if claudeRequest.System == nil {
					claudeRequest.System = message.StringContent()
				} else if existing, ok := claudeRequest.System.(string); ok {
					claudeRequest.System = existing + "\n" + message.StringContent()
				}
				continue
			}
			// 对话中间穿插的 system/developer 消息，转为 user role
			// 不能转 assistant，否则会与前一条真实 assistant 回复合并，导致语义错误
			message.Role = "user"
		}

		if message.Role == "user" || message.Role == "assistant" || message.Role == "tool" {
			conversationStarted = true
		}

		claudeMessage := Message{
			Role: message.Role,
		}
		// tool role 统一转为 user（Claude 不支持 tool role）
		if message.Role == "tool" {
			claudeMessage.Role = "user"
		}
		// content 为 nil（无 content 字段）时也走此分支，确保 tool_calls 能被转换
		if message.IsStringContent() || message.Content == nil {
			var contentBlocks []ContentBlockParam
			if message.Role == "tool" {
				contentBlocks = append(contentBlocks, ContentBlockParam{
					Type:      "tool_result",
					ToolUseID: message.ToolCallId,
					Content:   message.StringContent(),
				})
			} else if text := message.StringContent(); text != "" {
				contentBlocks = append(contentBlocks, ContentBlockParam{
					Type: "text",
					Text: text,
				})
			}
			for i := range message.ToolCalls {
				inputParam := make(map[string]any)
				if args, ok := message.ToolCalls[i].Function.Arguments.(string); ok {
					_ = json.Unmarshal([]byte(args), &inputParam)
				}
				contentBlocks = append(contentBlocks, ContentBlockParam{
					Type:  "tool_use",
					Id:    message.ToolCalls[i].Id,
					Name:  message.ToolCalls[i].Function.Name,
					Input: inputParam,
				})
			}
			claudeMessage.Content = contentBlocks
		} else {
			var contents []ContentBlockParam
			openaiContent := message.ParseContent()
			for _, part := range openaiContent {
				var content ContentBlockParam
				if part.Type == model.ContentTypeText {
					content.Type = "text"
					content.Text = part.Text
				} else if part.Type == model.ContentTypeImageURL {
					content.Type = "image"
					mimeType, data, err := image.GetImageFromUrl(part.ImageURL.Url)
					if err != nil {
						logger.SysError(fmt.Sprintf("Error getting image from URL: %v", err))
						continue
					}
					content.Source = Base64ImageSource{
						Type:      "base64",
						MediaType: mimeType,
						Data:      data,
					}
				}
				contents = append(contents, content)
			}
			claudeMessage.Content = contents
		}

		// 合并连续相同 role 的消息（Claude 要求 user/assistant 严格交替）
		if len(claudeRequest.Messages) > 0 {
			lastMsg := &claudeRequest.Messages[len(claudeRequest.Messages)-1]
			if lastMsg.Role == claudeMessage.Role {
				if lastBlocks, ok := lastMsg.Content.([]ContentBlockParam); ok {
					if newBlocks, ok := claudeMessage.Content.([]ContentBlockParam); ok {
						lastMsg.Content = append(lastBlocks, newBlocks...)
						continue
					}
				}
			}
		}

		claudeRequest.Messages = append(claudeRequest.Messages, claudeMessage)
	}

	// Claude 要求第一条消息必须是 user，如果不是则补一条占位消息
	if len(claudeRequest.Messages) > 0 && claudeRequest.Messages[0].Role != "user" {
		placeholder := Message{
			Role:    "user",
			Content: []ContentBlockParam{{Type: "text", Text: "..."}},
		}
		claudeRequest.Messages = append([]Message{placeholder}, claudeRequest.Messages...)
	}

	return &claudeRequest
}

// https://docs.anthropic.com/claude/reference/messages-streaming
func StreamResponseClaude2OpenAI(claudeResponse *StreamResponse) (*openai.ChatCompletionsStreamResponse, *Response) {
	var response *Response
	var responseText string
	var reasoningContent string
	var stopReason string
	tools := make([]model.Tool, 0)

	switch claudeResponse.Type {
	case "message_start":
		return nil, claudeResponse.Message
	case "content_block_start":
		if claudeResponse.ContentBlock != nil {
			switch claudeResponse.ContentBlock.Type {
			case "text":
				responseText = claudeResponse.ContentBlock.Text
			case "thinking":
				// thinking block 开始
				reasoningContent = claudeResponse.ContentBlock.Thinking
			case "tool_use":
				tools = append(tools, model.Tool{
					Id:   claudeResponse.ContentBlock.Id,
					Type: "function",
					Function: model.Function{
						Name:      claudeResponse.ContentBlock.Name,
						Arguments: "",
					},
				})
			}
		}
	case "content_block_delta":
		if claudeResponse.Delta != nil {
			switch claudeResponse.Delta.Type {
			case "text_delta":
				responseText = claudeResponse.Delta.Text
			case "thinking_delta":
				// thinking delta，将内容作为 reasoning_content
				reasoningContent = claudeResponse.Delta.Thinking
			case "input_json_delta":
				tools = append(tools, model.Tool{
					Function: model.Function{
						Arguments: claudeResponse.Delta.PartialJson,
					},
				})
			}
		}
	case "message_delta":
		if claudeResponse.Delta != nil {
			response = &Response{
				Usage: claudeResponse.Usage,
			}
		}
		if claudeResponse.Delta != nil && claudeResponse.Delta.StopReason != nil {
			stopReason = *claudeResponse.Delta.StopReason
		}
	}
	var choice openai.ChatCompletionsStreamResponseChoice
	choice.Delta.Content = responseText
	// 设置 reasoning_content
	if reasoningContent != "" {
		choice.Delta.ReasoningContent = reasoningContent
	}
	if len(tools) > 0 {
		choice.Delta.Content = nil // compatible with other OpenAI derivative applications, like LobeOpenAICompatibleFactory ...
		choice.Delta.ToolCalls = tools
	}
	choice.Delta.Role = "assistant"
	finishReason := stopReasonClaude2OpenAI(&stopReason)
	if finishReason != "null" {
		choice.FinishReason = &finishReason
	}
	var openaiResponse openai.ChatCompletionsStreamResponse
	openaiResponse.Object = "chat.completion.chunk"
	openaiResponse.Choices = []openai.ChatCompletionsStreamResponseChoice{choice}
	return &openaiResponse, response
}

func ResponseClaude2OpenAI(claudeResponse *Response) *openai.TextResponse {
	var responseText string
	var reasoningContent string
	var thoughtSignature string

	tools := make([]model.Tool, 0)

	// 遍历所有 content blocks，提取文本和 thinking 内容
	for _, v := range claudeResponse.Content {
		switch v.Type {
		case "text":
			responseText += v.Text
		case "thinking":
			// 提取 thinking 内容作为 reasoning_content
			if v.Thinking != "" {
				if reasoningContent != "" {
					reasoningContent += "\n"
				}
				reasoningContent += v.Thinking
			}
			// 保存签名
			if v.Signature != "" {
				thoughtSignature = v.Signature
			}
		case "tool_use":
			args, _ := json.Marshal(v.Input)
			tools = append(tools, model.Tool{
				Id:   v.Id,
				Type: "function", // compatible with other OpenAI derivative applications
				Function: model.Function{
					Name:      v.Name,
					Arguments: string(args),
				},
			})
		}
	}

	choice := openai.TextResponseChoice{
		Index: 0,
		Message: model.Message{
			Role:             "assistant",
			Content:          responseText,
			Name:             nil,
			ToolCalls:        tools,
			ReasoningContent: reasoningContent,
			ThoughtSignature: thoughtSignature,
		},
		FinishReason: stopReasonClaude2OpenAI(claudeResponse.StopReason),
	}
	fullTextResponse := openai.TextResponse{
		Id:      claudeResponse.Id,
		Model:   claudeResponse.Model,
		Object:  "chat.completion",
		Created: helper.GetTimestamp(),
		Choices: []openai.TextResponseChoice{choice},
	}
	return &fullTextResponse
}

func StreamHandler(c *gin.Context, resp *http.Response, relayMeta *util.RelayMeta) (*model.ErrorWithStatusCode, *model.Usage) {
	createdTime := helper.GetTimestamp()

	// 获取请求开始时间用于计算首字延迟
	var startTime time.Time
	if requestStartTime, exists := c.Get("request_start_time"); exists {
		if t, ok := requestStartTime.(time.Time); ok {
			startTime = t
		} else {
			startTime = time.Now() // fallback
		}
	} else {
		startTime = time.Now() // fallback
	}

	var firstWordTime *time.Time

	scanner := bufio.NewScanner(resp.Body)
	scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if atEOF && len(data) == 0 {
			return 0, nil, nil
		}
		if i := strings.Index(string(data), "\n"); i >= 0 {
			return i + 1, data[0:i], nil
		}
		if atEOF {
			return len(data), data, nil
		}
		return 0, nil, nil
	})

	common.SetEventStreamHeaders(c)

	var usage model.Usage
	var modelName string
	var id string
	var lastToolCallChoice openai.ChatCompletionsStreamResponseChoice

	for scanner.Scan() {
		data := scanner.Text()
		if len(data) < 6 || !strings.HasPrefix(data, "data:") {
			continue
		}
		data = strings.TrimPrefix(data, "data:")
		data = strings.TrimSpace(data)

		var claudeResponse StreamResponse
		err := json.Unmarshal([]byte(data), &claudeResponse)
		if err != nil {
			logger.Error(c.Request.Context(),
				"error unmarshalling stream response: "+err.Error())
			continue
		}

		response, meta := StreamResponseClaude2OpenAI(&claudeResponse)
		if meta != nil {
			usage.RankingExtraInputTokens = max(usage.RankingExtraInputTokens, meta.Usage.RankingCacheTokens())
			usage.PromptTokens += meta.Usage.InputTokens
			usage.CompletionTokens += meta.Usage.OutputTokens
			if len(meta.Id) > 0 { // only message_start has an id, otherwise it's a finish_reason event.
				modelName = meta.Model
				id = meta.Id
				c.Set("x_response_id", id)
				continue
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
			continue
		}

		// 检查是否有内容并记录首字时间
		for _, choice := range response.Choices {
			content := ""
			if choice.Delta.Content != nil {
				if contentStr, ok := choice.Delta.Content.(string); ok {
					content = contentStr
				}
			}
			// 也检查 reasoning_content
			reasoningContent := choice.Delta.ReasoningContent
			if (content != "" || reasoningContent != "") && firstWordTime == nil {
				// 记录首字时间
				now := time.Now()
				firstWordTime = &now
			}
		}

		response.Id = id
		response.Model = modelName
		response.Created = createdTime

		for _, choice := range response.Choices {
			if len(choice.Delta.ToolCalls) > 0 {
				lastToolCallChoice = choice
			}
		}
		err = render.ObjectData(c, response)
		if err != nil {
			logger.Error(c.Request.Context(),
				err.Error())
		}
	}

	if err := scanner.Err(); err != nil {
		logger.Error(c.Request.Context(),
			"error reading stream: "+err.Error())
	}

	// 如果需要包含 usage 信息，在流结束时发送一个包含 usage 的最终 chunk
	if relayMeta != nil && relayMeta.ShouldIncludeUsage {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		// 防御性处理：如果 id 或 modelName 为空，使用默认值
		responseId := id
		if responseId == "" {
			responseId = helper.GetUUID()
		}
		responseName := modelName
		if responseName == "" {
			responseName = relayMeta.ActualModelName
		}
		finalResponse := &openai.ChatCompletionsStreamResponse{
			Id:      responseId,
			Object:  "chat.completion.chunk",
			Created: createdTime,
			Model:   responseName,
			Choices: []openai.ChatCompletionsStreamResponseChoice{},
			Usage:   &usage,
		}
		if err := render.ObjectData(c, finalResponse); err != nil {
			logger.Error(c.Request.Context(),
				"error sending final usage chunk: "+err.Error())
		}
	}

	render.Done(c)

	err := resp.Body.Close()
	if err != nil {
		return openai.ErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil
	}

	// 计算首字延迟并存储到 context 中
	if firstWordTime != nil {
		firstWordLatency := firstWordTime.Sub(startTime).Seconds()
		c.Set("first_word_latency", firstWordLatency)
	}

	return nil, &usage
}

func Handler(c *gin.Context, resp *http.Response, promptTokens int, modelName string) (*model.ErrorWithStatusCode, *model.Usage) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return openai.ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError), nil
	}
	err = resp.Body.Close()
	if err != nil {
		return openai.ErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), nil
	}
	var claudeResponse Response
	err = json.Unmarshal(responseBody, &claudeResponse)
	if err != nil {
		return openai.ErrorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError), nil
	}
	if claudeResponse.Error != nil && claudeResponse.Error.Type != "" {
		return &model.ErrorWithStatusCode{
			Error: model.Error{
				Message: claudeResponse.Error.Message,
				Type:    claudeResponse.Error.Type,
				Param:   "",
				Code:    claudeResponse.Error.Type,
			},
			StatusCode: resp.StatusCode,
		}, nil
	}
	fullTextResponse := ResponseClaude2OpenAI(&claudeResponse)
	fullTextResponse.Model = modelName
	if claudeResponse.Id != "" {
		c.Set("x_response_id", claudeResponse.Id)
	}
	usage := model.Usage{
		RankingExtraInputTokens: claudeResponse.Usage.RankingCacheTokens(),
		PromptTokens:            claudeResponse.Usage.InputTokens,
		CompletionTokens:        claudeResponse.Usage.OutputTokens,
		TotalTokens:             claudeResponse.Usage.InputTokens + claudeResponse.Usage.OutputTokens,
	}
	fullTextResponse.Usage = usage
	jsonResponse, err := json.Marshal(fullTextResponse)
	if err != nil {
		return openai.ErrorWrapper(err, "marshal_response_body_failed", http.StatusInternalServerError), nil
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = c.Writer.Write(jsonResponse)
	return nil, &usage
}
