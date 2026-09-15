package model

type ResponseFormat struct {
	Type       string         `json:"type,omitempty"`        // "json_object" 或 "json_schema"
	JSONSchema map[string]any `json:"json_schema,omitempty"` // 当 Type 为 "json_schema" 时使用
}

// StreamOptions 定义流式响应的选项
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// SearchParameters 定义搜索参数
type SearchParameters struct {
	FromDate         *string  `json:"from_date,omitempty"`          // 开始日期，ISO-8601 YYYY-MM-DD 格式
	MaxSearchResults *int     `json:"max_search_results,omitempty"` // 最大搜索结果数量，默认15，范围1-30
	Mode             *string  `json:"mode,omitempty"`               // 搜索模式：off、on、auto，默认auto
	ReturnCitations  *bool    `json:"return_citations,omitempty"`   // 是否返回引用，默认true
	Sources          []string `json:"sources,omitempty"`            // 搜索来源列表
	ToDate           *string  `json:"to_date,omitempty"`            // 结束日期，ISO-8601 YYYY-MM-DD 格式
}

// WebSearchOptions 定义Web搜索选项
type WebSearchOptions struct {
	SearchContextSize *string `json:"search_context_size,omitempty"` // 搜索上下文大小，默认"medium"，映射到max_search
	UserLocation      string  `json:"user_location"`                 // 用户位置，仅为兼容性而包含，必需字段
}

type GeneralOpenAIRequest struct {
	Messages            []Message         `json:"messages,omitempty"`
	Model               string            `json:"model,omitempty"`
	FrequencyPenalty    float64           `json:"frequency_penalty,omitempty"`
	MaxTokens           int               `json:"max_tokens,omitempty"`
	MaxInputTokens      int               `json:"max_input_tokens,omitempty"`
	N                   int               `json:"n,omitempty"`
	PresencePenalty     float64           `json:"presence_penalty,omitempty"`
	ResponseFormat      *ResponseFormat   `json:"response_format,omitempty"`
	Seed                float64           `json:"seed,omitempty"`
	Stream              bool              `json:"stream,omitempty"`
	StreamOptions       *StreamOptions    `json:"stream_options,omitempty"`     // 新增字段
	SearchParameters    *SearchParameters `json:"search_parameters,omitempty"`  // 搜索参数
	WebSearchOptions    *WebSearchOptions `json:"web_search_options,omitempty"` // Web搜索选项
	Temperature         float64           `json:"temperature,omitempty"`
	TopP                float64           `json:"top_p,omitempty"`
	TopK                int               `json:"top_k,omitempty"`
	Tools               []Tool            `json:"tools,omitempty"`
	ToolChoice          any               `json:"tool_choice,omitempty"`
	FunctionCall        any               `json:"function_call,omitempty"`
	Functions           any               `json:"functions,omitempty"`
	User                string            `json:"user,omitempty"`
	Prompt              any               `json:"prompt,omitempty"`
	Input               any               `json:"input,omitempty"`
	EncodingFormat      string            `json:"encoding_format,omitempty"`
	Dimensions          int               `json:"dimensions,omitempty"`
	Instruction         string            `json:"instruction,omitempty"`
	Size                string            `json:"size,omitempty"`
	Stop                any               `json:"stop,omitempty"`
	AspectRatio         string            `json:"aspect_ratio,omitempty"`
	NumOutputs          int               `json:"num_outputs,omitempty"`
	OutputFormat        string            `json:"output_format,omitempty"`
	OutputQuality       int               `json:"output_quality,omitempty"`
	Modalities          []string          `json:"modalities,omitempty"`
	Audio               *AudioConfig      `json:"audio,omitempty"`
	ReasoningEffort     string            `json:"reasoning_effort,omitempty"`
	MaxCompletionTokens int               `json:"max_completion_tokens,omitempty"`
	ThinkingTokens      int               `json:"thinking_token,omitempty"`
	ReasoningContent    string            `json:"reasoning_content,omitempty"`
	// Thinking preserves MiMo's thinking configuration (for example {"type":"disabled"}).
	Thinking any `json:"thinking,omitempty"`
	// EnableThinking 控制 Qwen3 / DeepSeek 等思考模型是否开启思维链。
	// 阿里百炼 compatible-mode、OpenRouter reasoning 等均识别此字段。
	EnableThinking *bool `json:"enable_thinking,omitempty"`
	// ThinkingBudget 限制思维链最大 token 数，0 表示不传（由上游按模型默认处理）。
	ThinkingBudget int `json:"thinking_budget,omitempty"`
}

// 新增音频配置结构体
type AudioConfig struct {
	Voice  string `json:"voice,omitempty"`
	Format string `json:"format,omitempty"`
}

func (r GeneralOpenAIRequest) ParseInput() []string {
	if r.Input == nil {
		return nil
	}
	var input []string
	switch r.Input.(type) {
	case string:
		input = []string{r.Input.(string)}
	case []any:
		input = make([]string, 0, len(r.Input.([]any)))
		for _, item := range r.Input.([]any) {
			if str, ok := item.(string); ok {
				input = append(input, str)
			}
		}
	}
	return input
}

type GeneralVideoResponse struct {
	TaskId        string  `json:"task_id"`
	TaskStatus    string  `json:"task_status"`
	Message       string  `json:"message"`
	VideoDuration float64 `json:"video_duration,omitempty"` // 输入视频时长（秒），仅编辑/延长时返回
}

type VideoResultItem struct {
	Url string `json:"url"`
}

// VideoUsage 视频费用信息
type VideoUsage struct {
	CostInUsd float64 `json:"cost_in_usd"`
}

type GeneralFinalVideoResponse struct {
	TaskId        string            `json:"task_id"`
	VideoResult   string            `json:"video_result,omitempty"`
	VideoResults  []VideoResultItem `json:"video_results,omitempty"`
	VideoId       string            `json:"video_id"`
	TaskStatus    string            `json:"task_status"`
	Message       string            `json:"message"`
	Duration      string            `json:"duration"`
	Usage         *VideoUsage       `json:"usage,omitempty"`
	VideoDuration float64           `json:"video_duration,omitempty"` // 输入视频时长（秒），仅编辑/延伸场景
}

type GeneralImageResponseAsync struct {
	TaskId     string `json:"task_id"`
	TaskStatus string `json:"task_status"`
	Message    string `json:"message"`
}

type GeneralFinalImageResponseAsync struct {
	TaskId     string   `json:"task_id"`
	ImageId    string   `json:"image_id"`
	TaskStatus string   `json:"task_status"`
	Message    string   `json:"message"`
	ImageUrls  []string `json:"image_urls"`
}
