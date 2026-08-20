package openai

import "github.com/songquanpeng/one-api/relay/model"

type TextContent struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

type ImageContent struct {
	Type     string          `json:"type,omitempty"`
	ImageURL *model.ImageURL `json:"image_url,omitempty"`
}

type ChatRequest struct {
	Model     string          `json:"model"`
	Messages  []model.Message `json:"messages"`
	MaxTokens int             `json:"max_tokens"`
}

type TextRequest struct {
	Model     string          `json:"model"`
	Messages  []model.Message `json:"messages"`
	Prompt    string          `json:"prompt"`
	MaxTokens int             `json:"max_tokens"`
	//Stream   bool      `json:"stream"`
}

// ImageRequest docs: https://platform.openai.com/docs/api-reference/images/create
type ImageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt" binding:"required"`
	N              int    `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	Quality        string `json:"quality,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
	Style          string `json:"style,omitempty"`
	User           string `json:"user,omitempty"`
	AspectRatio    string `json:"aspect_ratio,omitempty"`
	NumOutputs     int    `json:"num_outputs,omitempty"`
	Seed           int    `json:"seed,omitempty"`
	OutputFormat   string `json:"output_format,omitempty"`
	OutputQuality  int    `json:"output_quality,omitempty"`
}

type WhisperJSONResponse struct {
	Text string `json:"text,omitempty"`
}

type WhisperVerboseJSONResponse struct {
	Task     string    `json:"task,omitempty"`
	Language string    `json:"language,omitempty"`
	Duration float64   `json:"duration,omitempty"`
	Text     string    `json:"text,omitempty"`
	Segments []Segment `json:"segments,omitempty"`
}

type Segment struct {
	Id               int     `json:"id"`
	Seek             int     `json:"seek"`
	Start            float64 `json:"start"`
	End              float64 `json:"end"`
	Text             string  `json:"text"`
	Tokens           []int   `json:"tokens"`
	Temperature      float64 `json:"temperature"`
	AvgLogprob       float64 `json:"avg_logprob"`
	CompressionRatio float64 `json:"compression_ratio"`
	NoSpeechProb     float64 `json:"no_speech_prob"`
}

type TextToSpeechRequest struct {
	Model          string  `json:"model" binding:"required"`
	Input          string  `json:"input" binding:"required"`
	Voice          string  `json:"voice" binding:"required"`
	Speed          float64 `json:"speed"`
	ResponseFormat string  `json:"response_format"`
	StreamFormat   string  `json:"stream_format,omitempty"`
}

type UsageOrResponseText struct {
	*model.Usage
	ResponseText string
}

type SlimTextResponse struct {
	Id          string               `json:"id"` // response ID (chatcmpl-xxx 或 cmpl-xxx)
	Choices     []TextResponseChoice `json:"choices"`
	model.Usage `json:"usage"`
	Error       model.Error `json:"error"`
}

type TextResponseChoice struct {
	Index         int `json:"index"`
	model.Message `json:"message"`
	FinishReason  string `json:"finish_reason"`
}

type TextResponse struct {
	Id          string               `json:"id"`
	Model       string               `json:"model,omitempty"`
	Object      string               `json:"object"`
	Created     int64                `json:"created"`
	Choices     []TextResponseChoice `json:"choices"`
	model.Usage `json:"usage"`
}

type EmbeddingResponseItem struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

type EmbeddingResponse struct {
	Object      string                  `json:"object"`
	Data        []EmbeddingResponseItem `json:"data"`
	Model       string                  `json:"model"`
	model.Usage `json:"usage"`
}

type ImageResponse struct {
	Created int `json:"created,omitempty"`
	Data    []struct {
		Url     string `json:"url,omitempty"`
		B64Json string `json:"b64_json,omitempty"`
	} `json:"data,omitempty"`
	Usage struct {
		InputTokens        int `json:"input_tokens,omitempty"`
		InputTokensDetails struct {
			ImageTokens int `json:"image_tokens,omitempty"`
			TextTokens  int `json:"text_tokens,omitempty"`
		} `json:"input_tokens_details,omitempty"`
		OutputTokens        int `json:"output_tokens,omitempty"`
		OutputTokensDetails struct {
			TextTokens  int `json:"text_tokens,omitempty"`
			ImageTokens int `json:"image_tokens,omitempty"`
		} `json:"output_tokens_details,omitempty"`
		TotalTokens int `json:"total_tokens,omitempty"`
	} `json:"usage,omitempty"`
}

type ChatCompletionsStreamResponseChoice struct {
	Index        int           `json:"index"`
	Delta        model.Message `json:"delta"`
	FinishReason *string       `json:"finish_reason,omitempty"`
}

type ChatCompletionsStreamResponse struct {
	Id      string                                `json:"id"`
	Object  string                                `json:"object"`
	Created int64                                 `json:"created"`
	Model   string                                `json:"model"`
	Choices []ChatCompletionsStreamResponseChoice `json:"choices"`
	Usage   *model.Usage                          `json:"usage"`
}

type CompletionsStreamResponse struct {
	Id      string `json:"id"`      // response ID (cmpl-xxx)
	Object  string `json:"object"`  // 对象类型
	Created int64  `json:"created"` // 时间戳
	Model   string `json:"model"`   // 模型名称
	Choices []struct {
		Text         string `json:"text"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// SoraVideoRequest Sora 视频生成请求 (JSON 格式)
type SoraVideoRequest struct {
	Model          string `json:"model" binding:"required"`  // 模型名称 (sora-2, sora-2-pro)
	Prompt         string `json:"prompt" binding:"required"` // 视频描述
	Size           string `json:"size,omitempty"`            // 分辨率 (720x1280, 1280x720, 1024x1792, 1792x1024)
	Seconds        string `json:"seconds,omitempty"`         // 视频时长（秒）- 官方字段名，string 类型
	AspectRatio    string `json:"aspect_ratio,omitempty"`    // 宽高比
	Loop           bool   `json:"loop,omitempty"`            // 是否循环
	InputReference string `json:"input_reference,omitempty"` // 输入参考（URL/base64/dataURL）
}

// SoraRemixRequest Sora 视频 remix 请求
type SoraRemixRequest struct {
	Model   string `json:"model,omitempty"`             // 模型名称（用于路由识别，发送时会去掉）
	VideoID string `json:"video_id" binding:"required"` // 原视频ID
	Prompt  string `json:"prompt" binding:"required"`   // 新的描述
}

// SoraVideoResponse Sora 视频生成响应
type SoraVideoResponse struct {
	ID                 string `json:"id"`
	Object             string `json:"object"`
	Created            int64  `json:"created,omitempty"`
	CreatedAt          int64  `json:"created_at,omitempty"` // Remix 响应使用
	Model              string `json:"model"`
	Status             string `json:"status"`             // queued, processing, completed, failed
	Progress           int    `json:"progress,omitempty"` // Remix 响应使用
	Prompt             string `json:"prompt,omitempty"`
	Size               string `json:"size,omitempty"`
	Seconds            string `json:"seconds,omitempty"` // 视频时长（秒），string 类型
	VideoURL           string `json:"video_url,omitempty"`
	RemixedFromVideoID string `json:"remixed_from_video_id,omitempty"` // Remix 响应使用
	Error              *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
	StatusCode int `json:"status_code,omitempty"` // HTTP status code
}

// OpeanaiResaponseReques OpenAI Responses API 请求体
// docs: https://platform.openai.com/docs/api-reference/responses/create
type OpeanaiResaponseRequest struct {
	Background         bool           `json:"background,omitempty"`           // 是否后台运行
	Conversation       interface{}    `json:"conversation,omitempty"`         // 对话上下文
	FrequencyPenalty   float64        `json:"frequency_penalty,omitempty"`    // 频率惩罚
	Include            []string       `json:"include,omitempty"`              // 包含的字段
	Input              interface{}    `json:"input,omitempty"`                // 输入内容（可以是字符串或消息数组）
	Instructions       string         `json:"instructions,omitempty"`         // 系统指令
	LogitBias          map[string]int `json:"logit_bias,omitempty"`           // Logit 偏置
	MaxOutputTokens    int            `json:"max_output_tokens,omitempty"`    // 最大输出 token 数
	MaxToolCalls       int            `json:"max_tool_calls,omitempty"`       // 最大工具调用次数
	Metadata           interface{}    `json:"metadata,omitempty"`             // 元数据
	Modalities         []string       `json:"modalities,omitempty"`           // 模态（text, audio）
	Model              string         `json:"model,omitempty"`                // 模型名称
	ParallelToolCalls  bool           `json:"parallel_tool_calls,omitempty"`  // 是否并行调用工具
	PresencePenalty    float64        `json:"presence_penalty,omitempty"`     // 存在惩罚
	PreviousResponseID string         `json:"previous_response_id,omitempty"` // 上一次响应 ID（用于续写 / compact）
	Reasoning          interface{}    `json:"reasoning,omitempty"`            // 推理配置
	ResponseFormat     interface{}    `json:"response_format,omitempty"`      // 响应格式
	Seed               int            `json:"seed,omitempty"`                 // 随机种子
	Stop               interface{}    `json:"stop,omitempty"`                 // 停止序列
	Store              bool           `json:"store,omitempty"`                // 是否存储
	Stream             bool           `json:"stream,omitempty"`               // 是否流式返回
	StreamOptions      interface{}    `json:"stream_options,omitempty"`       // 流式选项
	Temperature        float64        `json:"temperature,omitempty"`          // 温度参数
	ToolChoice         interface{}    `json:"tool_choice,omitempty"`          // 工具选择策略
	Tools              []interface{}  `json:"tools,omitempty"`                // 可用工具列表
	TopP               float64        `json:"top_p,omitempty"`                // Top-p 采样参数
	User               string         `json:"user,omitempty"`                 // 用户标识

	// Audio 相关字段
	Audio *AudioConfig `json:"audio,omitempty"` // 音频配置
}

// AudioConfig 音频配置
type AudioConfig struct {
	Voice  string `json:"voice,omitempty"`  // 语音类型 (alloy, echo, fable, onyx, nova, shimmer)
	Format string `json:"format,omitempty"` // 音频格式 (wav, mp3, flac, opus, pcm16)
}

// OpeanaiResaponseResponse OpenAI Responses API 响应对象
// docs: https://platform.openai.com/docs/api-reference/responses/object
type OpenaiResaponseResponse struct {
	ID                string                 `json:"id"`                           // 响应的唯一标识符
	Object            string                 `json:"object"`                       // 对象类型，值为 "response"
	Status            string                 `json:"status"`                       // 响应状态 (in_progress, completed, failed, cancelled, expired, incomplete)
	StatusDetails     interface{}            `json:"status_details,omitempty"`     // 状态详情
	Output            []ResponsesOutput      `json:"output,omitempty"`             // 响应输出内容
	ConversationID    string                 `json:"conversation_id,omitempty"`    // 对话ID
	CreatedAt         int64                  `json:"created_at"`                   // 创建时间（Unix时间戳）
	CompletedAt       int64                  `json:"completed_at,omitempty"`       // 完成时间（Unix时间戳）
	FailedAt          int64                  `json:"failed_at,omitempty"`          // 失败时间（Unix时间戳）
	CancelledAt       int64                  `json:"cancelled_at,omitempty"`       // 取消时间（Unix时间戳）
	IncompleteAt      int64                  `json:"incomplete_at,omitempty"`      // 未完成时间（Unix时间戳）
	ExpiresAt         int64                  `json:"expires_at,omitempty"`         // 过期时间（Unix时间戳）
	IncompleteDetails *IncompleteDetails     `json:"incomplete_details,omitempty"` // 未完成详情
	Metadata          map[string]interface{} `json:"metadata,omitempty"`           // 元数据
	Model             string                 `json:"model,omitempty"`              // 使用的模型
	Instructions      string                 `json:"instructions,omitempty"`       // 系统指令
	Usage             *ResponseUsage         `json:"usage,omitempty"`              // Token 使用情况
}

// IncompleteDetails 未完成响应的详细信息
type IncompleteDetails struct {
	Reason string `json:"reason,omitempty"` // 未完成的原因 (max_output_tokens, max_tool_calls)
}

// ResponseUsage 响应的 token 使用统计
type ResponseUsage struct {
	InputTokens         int                  `json:"input_tokens"`                    // 输入 token 数
	OutputTokens        int                  `json:"output_tokens"`                   // 输出 token 数
	TotalTokens         int                  `json:"total_tokens"`                    // 总 token 数
	InputTokensDetails  *InputTokensDetails  `json:"input_tokens_details,omitempty"`  // 输入 token 详情
	OutputTokensDetails *OutputTokensDetails `json:"output_tokens_details,omitempty"` // 输出 token 详情
}

// InputTokensDetails 输入 token 详细信息
type InputTokensDetails struct {
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	CachedTokens     int `json:"cached_tokens,omitempty"` // 缓存的 token 数
	TextTokens       int `json:"text_tokens,omitempty"`   // 文本 token 数
	AudioTokens      int `json:"audio_tokens,omitempty"`  // 音频 token 数
	ImageTokens      int `json:"image_tokens,omitempty"`  // 图像 token 数
}

// OutputTokensDetails 输出 token 详细信息
type OutputTokensDetails struct {
	TextTokens      int `json:"text_tokens,omitempty"`      // 文本 token 数
	AudioTokens     int `json:"audio_tokens,omitempty"`     // 音频 token 数
	ReasoningTokens int `json:"reasoning_tokens,omitempty"` // 推理 token 数
}

// OpenaiResponseStreamEvent OpenAI Responses API 流式事件（SSE格式）
// docs: https://platform.openai.com/docs/api-reference/responses-streaming
type OpenaiResponseStreamEvent struct {
	Event string `json:"event"` // 事件类型
	Data  string `json:"data"`  // JSON 编码的事件数据
}

// ResponseStreamResponse 响应相关的流式事件数据
type OpenaiResponseStreamResponse struct {
	Type     string                   `json:"type"`     // 事件类型：response.created, response.done, response.failed, response.incomplete, response.cancelled
	Response *OpenaiResaponseResponse `json:"response"` // 响应对象
	Delta    string                   `json:"delta,omitempty"`
	Item     *ResponsesOutput         `json:"item,omitempty"`
}
type ResponsesOutput struct {
	Type    string                   `json:"type"`
	ID      string                   `json:"id"`
	Status  string                   `json:"status"`
	Role    string                   `json:"role"`
	Content []ResponsesOutputContent `json:"content"`
	Quality string                   `json:"quality"`
	Size    string                   `json:"size"`
}
type ResponsesOutputContent struct {
	Type        string        `json:"type"`
	Text        string        `json:"text"`
	Annotations []interface{} `json:"annotations"`
}

// ResponseStreamOutputItem 输出项相关的流式事件数据
type ResponseStreamOutputItem struct {
	Type         string      `json:"type"`           // 事件类型：output_item.added, output_item.done
	ResponseID   string      `json:"response_id"`    // 响应ID
	OutputItemID string      `json:"output_item_id"` // 输出项ID
	OutputItem   interface{} `json:"output_item"`    // 输出项数据
}

// ResponseStreamContentPart 内容部分相关的流式事件数据
type ResponseStreamContentPart struct {
	Type          string      `json:"type"`            // 事件类型：content_part.added, content_part.done
	ResponseID    string      `json:"response_id"`     // 响应ID
	OutputItemID  string      `json:"output_item_id"`  // 输出项ID
	ContentPartID string      `json:"content_part_id"` // 内容部分ID
	ContentPart   interface{} `json:"content_part"`    // 内容部分数据
}

// ResponseStreamTextDelta 文本增量事件数据
type ResponseStreamTextDelta struct {
	Type          string `json:"type"`            // 事件类型：text.delta
	ResponseID    string `json:"response_id"`     // 响应ID
	OutputItemID  string `json:"output_item_id"`  // 输出项ID
	ContentPartID string `json:"content_part_id"` // 内容部分ID
	Delta         string `json:"delta"`           // 文本增量
}

// ResponseStreamTextDone 文本完成事件数据
type ResponseStreamTextDone struct {
	Type          string `json:"type"`            // 事件类型：text.done
	ResponseID    string `json:"response_id"`     // 响应ID
	OutputItemID  string `json:"output_item_id"`  // 输出项ID
	ContentPartID string `json:"content_part_id"` // 内容部分ID
	Text          string `json:"text"`            // 完整文本
}

// ResponseStreamAudioTranscriptDelta 音频转录增量事件数据
type ResponseStreamAudioTranscriptDelta struct {
	Type          string `json:"type"`            // 事件类型：audio_transcript.delta
	ResponseID    string `json:"response_id"`     // 响应ID
	OutputItemID  string `json:"output_item_id"`  // 输出项ID
	ContentPartID string `json:"content_part_id"` // 内容部分ID
	Delta         string `json:"delta"`           // 音频转录增量
}

// ResponseStreamAudioTranscriptDone 音频转录完成事件数据
type ResponseStreamAudioTranscriptDone struct {
	Type          string `json:"type"`            // 事件类型：audio_transcript.done
	ResponseID    string `json:"response_id"`     // 响应ID
	OutputItemID  string `json:"output_item_id"`  // 输出项ID
	ContentPartID string `json:"content_part_id"` // 内容部分ID
	Transcript    string `json:"transcript"`      // 完整音频转录
}

// ResponseStreamAudioDelta 音频数据增量事件
type ResponseStreamAudioDelta struct {
	Type          string `json:"type"`            // 事件类型：audio.delta
	ResponseID    string `json:"response_id"`     // 响应ID
	OutputItemID  string `json:"output_item_id"`  // 输出项ID
	ContentPartID string `json:"content_part_id"` // 内容部分ID
	Delta         string `json:"delta"`           // 音频数据增量（base64编码）
}

// ResponseStreamAudioDone 音频数据完成事件
type ResponseStreamAudioDone struct {
	Type          string `json:"type"`            // 事件类型：audio.done
	ResponseID    string `json:"response_id"`     // 响应ID
	OutputItemID  string `json:"output_item_id"`  // 输出项ID
	ContentPartID string `json:"content_part_id"` // 内容部分ID
}

// ResponseStreamRefusalDelta 拒绝增量事件数据
type ResponseStreamRefusalDelta struct {
	Type          string `json:"type"`            // 事件类型：refusal.delta
	ResponseID    string `json:"response_id"`     // 响应ID
	OutputItemID  string `json:"output_item_id"`  // 输出项ID
	ContentPartID string `json:"content_part_id"` // 内容部分ID
	Delta         string `json:"delta"`           // 拒绝内容增量
}

// ResponseStreamRefusalDone 拒绝完成事件数据
type ResponseStreamRefusalDone struct {
	Type          string `json:"type"`            // 事件类型：refusal.done
	ResponseID    string `json:"response_id"`     // 响应ID
	OutputItemID  string `json:"output_item_id"`  // 输出项ID
	ContentPartID string `json:"content_part_id"` // 内容部分ID
	Refusal       string `json:"refusal"`         // 完整拒绝内容
}

// ResponseStreamFunctionCallArgumentsDelta 函数调用参数增量事件
type ResponseStreamFunctionCallArgumentsDelta struct {
	Type          string `json:"type"`            // 事件类型：function_call_arguments.delta
	ResponseID    string `json:"response_id"`     // 响应ID
	OutputItemID  string `json:"output_item_id"`  // 输出项ID
	ContentPartID string `json:"content_part_id"` // 内容部分ID
	Delta         string `json:"delta"`           // 函数参数增量
}

// ResponseStreamFunctionCallArgumentsDone 函数调用参数完成事件
type ResponseStreamFunctionCallArgumentsDone struct {
	Type          string `json:"type"`            // 事件类型：function_call_arguments.done
	ResponseID    string `json:"response_id"`     // 响应ID
	OutputItemID  string `json:"output_item_id"`  // 输出项ID
	ContentPartID string `json:"content_part_id"` // 内容部分ID
	Arguments     string `json:"arguments"`       // 完整函数参数
}

// ResponseStreamToolCallEvent 工具调用事件（文件搜索、网络搜索、代码解释器等）
type ResponseStreamToolCallEvent struct {
	Type          string      `json:"type"`                // 事件类型
	ResponseID    string      `json:"response_id"`         // 响应ID
	OutputItemID  string      `json:"output_item_id"`      // 输出项ID
	ContentPartID string      `json:"content_part_id"`     // 内容部分ID
	ToolCall      interface{} `json:"tool_call,omitempty"` // 工具调用结果（仅在 done 事件中）
}

// ResponseStreamReasoningDelta 推理增量事件数据
type ResponseStreamReasoningDelta struct {
	Type          string `json:"type"`            // 事件类型：reasoning.delta
	ResponseID    string `json:"response_id"`     // 响应ID
	OutputItemID  string `json:"output_item_id"`  // 输出项ID
	ContentPartID string `json:"content_part_id"` // 内容部分ID
	Delta         string `json:"delta"`           // 推理内容增量
}

// ResponseStreamReasoningDone 推理完成事件数据
type ResponseStreamReasoningDone struct {
	Type          string `json:"type"`            // 事件类型：reasoning.done
	ResponseID    string `json:"response_id"`     // 响应ID
	OutputItemID  string `json:"output_item_id"`  // 输出项ID
	ContentPartID string `json:"content_part_id"` // 内容部分ID
	Reasoning     string `json:"reasoning"`       // 完整推理内容
}

// ResponseStreamError 错误事件数据
type ResponseStreamError struct {
	Type  string      `json:"type"`  // 事件类型：error
	Error interface{} `json:"error"` // 错误信息
}
