package anthropic

import (
	"bytes"
	"encoding/json"
)

// Claude API 结构体定义
// 基于 https://platform.claude.com/docs/en/api/messages/create
//
// 主要特性：
// - 支持多种内容块类型（文本、图片、文档、工具等）
// - 支持缓存控制（5分钟或1小时临时缓存）
// - 支持引用系统（多种引用类型）
// - 完善的token使用统计（包括缓存统计）
// - 支持工具调用和服务器工具
// - 支持思考过程展示
// - 支持结构化输出（Structured Outputs）
//
// 使用示例见文件末尾的注释

// Metadata 用户元数据
type Metadata struct {
	UserId string `json:"user_id,omitempty"`
}

// CacheControlEphemeral 缓存控制 - 临时缓存
type CacheControlEphemeral struct {
	Type string `json:"type"`          // "ephemeral"
	TTL  string `json:"ttl,omitempty"` // "5m" 或 "1h"
}

// CitationCharLocation 字符位置引用
type CitationCharLocation struct {
	CitedText      string `json:"cited_text"`
	DocumentIndex  int    `json:"document_index"`
	DocumentTitle  string `json:"document_title"`
	EndCharIndex   int    `json:"end_char_index"`
	StartCharIndex int    `json:"start_char_index"`
	Type           string `json:"type"` // "char_location"
}

// CitationPageLocation 页面位置引用
type CitationPageLocation struct {
	CitedText       string `json:"cited_text"`
	DocumentIndex   int    `json:"document_index"`
	DocumentTitle   string `json:"document_title"`
	EndPageNumber   int    `json:"end_page_number"`
	StartPageNumber int    `json:"start_page_number"`
	Type            string `json:"type"` // "page_location"
}

// CitationContentBlockLocation 内容块位置引用
type CitationContentBlockLocation struct {
	CitedText       string `json:"cited_text"`
	DocumentIndex   int    `json:"document_index"`
	DocumentTitle   string `json:"document_title"`
	EndBlockIndex   int    `json:"end_block_index"`
	StartBlockIndex int    `json:"start_block_index"`
	Type            string `json:"type"` // "content_block_location"
}

// CitationWebSearchResultLocation 网络搜索结果引用
type CitationWebSearchResultLocation struct {
	CitedText      string `json:"cited_text"`
	EncryptedIndex string `json:"encrypted_index"`
	Title          string `json:"title"`
	Type           string `json:"type"` // "web_search_result_location"
	URL            string `json:"url"`
}

// CitationSearchResultLocation 搜索结果引用
type CitationSearchResultLocation struct {
	CitedText         string `json:"cited_text"`
	EndBlockIndex     int    `json:"end_block_index"`
	SearchResultIndex int    `json:"search_result_index"`
	Source            string `json:"source"`
	StartBlockIndex   int    `json:"start_block_index"`
	Title             string `json:"title"`
	Type              string `json:"type"` // "search_result_location"
}

// TextBlockParam 文本内容块
type TextBlockParam struct {
	Text         string                 `json:"text"`
	Type         string                 `json:"type"` // "text"
	CacheControl *CacheControlEphemeral `json:"cache_control,omitempty"`
	Citations    []interface{}          `json:"citations,omitempty"` // CitationCharLocation, CitationPageLocation, etc.
}

// ImageSource 图片来源
type ImageSource struct {
	Type      string `json:"type"`                 // "base64" 或 "url"
	MediaType string `json:"media_type,omitempty"` // "image/jpeg", "image/png", "image/gif", "image/webp"
	Data      string `json:"data,omitempty"`       // base64 data
	URL       string `json:"url,omitempty"`        // image url
}

// URLImageSource URL 图片来源
type URLImageSource struct {
	Type string `json:"type"` // "url"
	URL  string `json:"url"`
}

// Base64ImageSource Base64 图片来源
type Base64ImageSource struct {
	Data      string `json:"data"`
	MediaType string `json:"media_type"` // "image/jpeg", "image/png", "image/gif", "image/webp"
	Type      string `json:"type"`       // "base64"
}

// ImageBlockParam 图片内容块
type ImageBlockParam struct {
	Source       interface{}            `json:"source"` // Base64ImageSource or URLImageSource
	Type         string                 `json:"type"`   // "image"
	CacheControl *CacheControlEphemeral `json:"cache_control,omitempty"`
}

// Base64PDFSource Base64 PDF 来源
type Base64PDFSource struct {
	Data      string `json:"data"`
	MediaType string `json:"media_type"` // "application/pdf"
	Type      string `json:"type"`       // "base64"
}

// PlainTextSource 纯文本来源
type PlainTextSource struct {
	Data      string `json:"data"`
	MediaType string `json:"media_type"` // "text/plain"
	Type      string `json:"type"`       // "text"
}

// ContentBlockSource 内容块来源
type ContentBlockSource struct {
	Content interface{} `json:"content"` // string or []ContentBlockSourceContent
	Type    string      `json:"type"`    // "content"
}

// URLPDFSource URL PDF 来源
type URLPDFSource struct {
	Type      string `json:"type"` // "url"
	URL       string `json:"url"`
	MediaType string `json:"media_type"` // "application/pdf"
}

// DocumentBlockParam 文档内容块
type DocumentBlockParam struct {
	Source       interface{}            `json:"source"` // Base64PDFSource, PlainTextSource, ContentBlockSource, or URLPDFSource
	Type         string                 `json:"type"`   // "document"
	CacheControl *CacheControlEphemeral `json:"cache_control,omitempty"`
	Citations    []interface{}          `json:"citations,omitempty"`
	Title        string                 `json:"title,omitempty"`
	Context      string                 `json:"context,omitempty"`
}

// ToolUseBlock 工具使用块
type ToolUseBlock struct {
	ID    string `json:"id"`
	Input any    `json:"input"`
	Name  string `json:"name"`
	Type  string `json:"type"` // "tool_use"
}

// ServerToolUseBlock 服务器工具使用块
type ServerToolUseBlock struct {
	ID    string `json:"id"`
	Input any    `json:"input"`
	Name  string `json:"name"` // "web_search"
	Type  string `json:"type"` // "server_tool_use"
}

// ThinkingBlock 思考块
type ThinkingBlock struct {
	Signature string `json:"signature"`
	Thinking  string `json:"thinking"`
	Type      string `json:"type"` // "thinking"
}

// RedactedThinkingBlock 红acted思考块
type RedactedThinkingBlock struct {
	Data string `json:"data"`
	Type string `json:"type"` // "redacted_thinking"
}

// WebSearchToolResultBlock 网络搜索工具结果块
type WebSearchToolResultBlock struct {
	Content   interface{} `json:"content"` // WebSearchToolResultError or []WebSearchResultBlock
	ToolUseID string      `json:"tool_use_id"`
	Type      string      `json:"type"` // "web_search_tool_result"
}

// ContentBlockParam 内容块参数联合类型
type ContentBlockParam struct {
	// Text block
	Text string `json:"text,omitempty"`
	Type string `json:"type"` // "text", "image", "document", "tool_use", "server_tool_use", "thinking", "redacted_thinking", "web_search_tool_result"

	// Image block
	Source interface{} `json:"source,omitempty"` // Base64ImageSource or URLImageSource

	// Document block
	Title   string `json:"title,omitempty"`
	Context string `json:"context,omitempty"`

	// Tool use blocks
	Id    string `json:"id,omitempty"`
	Name  string `json:"name,omitempty"`
	Input any    `json:"input,omitempty"`

	// Thinking blocks
	Signature string `json:"signature,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Data      string `json:"data,omitempty"`

	// Web search tool result
	Content   interface{} `json:"content,omitempty"`
	ToolUseID string      `json:"tool_use_id,omitempty"`

	// Common fields
	CacheControl *CacheControlEphemeral `json:"cache_control,omitempty"`
	Citations    []interface{}          `json:"citations,omitempty"`
}

// Message 消息结构
type Message struct {
	Role    string `json:"role"` // "user", "assistant"
	Content any    `json:"content"`
}

// JSONOutputFormat 结构化输出格式
// 基于 https://platform.claude.com/docs/en/build-with-claude/structured-outputs
// 用于控制 Claude 的响应格式，确保返回符合 schema 的 JSON
type JSONOutputFormat struct {
	Type   string `json:"type"`   // "json_schema"
	Schema any    `json:"schema"` // JSON Schema 定义
}

// OutputConfig 输出配置
// 包含 effort 和 format 两个子字段
type OutputConfig struct {
	// Effort 控制模型推理努力程度: "low", "medium", "high", "max"
	Effort string `json:"effort,omitempty"`
	// Format 结构化输出格式
	Format *JSONOutputFormat `json:"format,omitempty"`
}

// Tool 工具定义
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema InputSchema `json:"input_schema"`
	Strict      bool        `json:"strict,omitempty"` // 启用 strict tool use，保证 schema 验证
}

// InputSchema 工具输入模式
type InputSchema struct {
	Type       string   `json:"type"` // "object"
	Properties any      `json:"properties,omitempty"`
	Required   []string `json:"required,omitempty"`
}

// ToolChoice 工具选择
type ToolChoice struct {
	Type string `json:"type"`           // "auto", "any", "tool"；输入兼容字符串 "none" 并在 Request.UnmarshalJSON 中归一化为 nil
	Name string `json:"name,omitempty"` // 当 type 为 "tool" 时指定工具名称
}

// UnmarshalJSON 兼容字符串和对象两种 tool_choice 写法：
//   - "auto" / "any" / "none"
//   - {"type":"auto"} / {"type":"tool","name":"..."}
func (t *ToolChoice) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*t = ToolChoice{}
		return nil
	}

	if data[0] == '"' {
		var choice string
		if err := json.Unmarshal(data, &choice); err != nil {
			return err
		}
		*t = ToolChoice{Type: choice}
		return nil
	}

	type toolChoiceAlias ToolChoice
	var alias toolChoiceAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*t = ToolChoice(alias)
	return nil
}

// ThinkingConfig 扩展思考配置
type ThinkingConfig struct {
	Type         string `json:"type"`                    // "enabled", "disabled", "adaptive"
	BudgetTokens int    `json:"budget_tokens,omitempty"` // 思考 token 预算，最小 1024（仅 type="enabled" 时使用）
	Display      string `json:"display,omitempty"`       // 思考内容展示方式: "summarized", "omitted"（仅 type="adaptive" 时使用，4.7+ 默认 omitted）
}

// Request Claude API 请求结构
// 基于 https://platform.claude.com/docs/en/api/messages/create
type Request struct {
	Model         string          `json:"model"`                    // 模型名称
	Messages      []Message       `json:"messages,omitempty"`       // 消息列表
	System        interface{}     `json:"system,omitempty"`         // 系统提示（支持字符串或数组格式，数组格式用于 prompt caching）
	MaxTokens     int             `json:"max_tokens"`               // 最大输出token数
	StopSequences []string        `json:"stop_sequences,omitempty"` // 停止序列
	Stream        bool            `json:"stream,omitempty"`         // 是否流式输出
	Temperature   float64         `json:"temperature,omitempty"`    // 温度参数
	TopP          float64         `json:"top_p,omitempty"`          // Top-p 参数
	TopK          int             `json:"top_k,omitempty"`          // Top-k 参数
	Tools         []Tool          `json:"tools,omitempty"`          // 可用工具
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`    // 工具选择策略
	Metadata      *Metadata       `json:"metadata,omitempty"`       // 元数据
	OutputConfig  *OutputConfig   `json:"output_config,omitempty"`  // 输出配置（effort + format）
	Thinking      *ThinkingConfig `json:"thinking,omitempty"`       // 扩展思考配置
	ServiceTier   string          `json:"service_tier,omitempty"`   // 服务层级: "auto", "standard_only"
	InferenceGeo  string          `json:"inference_geo,omitempty"`  // 推理地理区域
}

// UnmarshalJSON 归一化历史兼容写法：
//   - tool_choice 字符串 "auto"/"any" 转为结构体
//   - tool_choice 字符串 "none" 视为未设置，避免后续转发为无效对象格式
func (r *Request) UnmarshalJSON(data []byte) error {
	type requestAlias Request
	var alias requestAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*r = Request(alias)
	if r.ToolChoice != nil && r.ToolChoice.Type == "none" && r.ToolChoice.Name == "" {
		r.ToolChoice = nil
	}
	return nil
}

// CacheCreation 缓存创建统计
type CacheCreation struct {
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens,omitempty"` // 1小时缓存创建的输入token数
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens,omitempty"` // 5分钟缓存创建的输入token数
}

// ServerToolUsage 服务器工具使用统计
type ServerToolUsage struct {
	WebSearchRequests int `json:"web_search_requests,omitempty"` // 网络搜索请求数
}

// Usage token 使用统计
type Usage struct {
	InputTokens                 int              `json:"input_tokens"`                          // 输入token数
	OutputTokens                int              `json:"output_tokens"`                         // 输出token数
	CacheCreationInputTokens    int              `json:"cache_creation_input_tokens,omitempty"` // 缓存创建输入token数
	CacheReadInputTokens        int              `json:"cache_read_input_tokens,omitempty"`     // 缓存读取输入token数
	ClaudeCacheCreation5mTokens int              `json:"claude_cache_creation_5_m_tokens,omitempty"`
	ClaudeCacheCreation1hTokens int              `json:"claude_cache_creation_1_h_tokens,omitempty"`
	CacheCreation               *CacheCreation   `json:"cache_creation,omitempty"`  // 缓存创建详情
	ServerToolUse               *ServerToolUsage `json:"server_tool_use,omitempty"` // 服务器工具使用统计
	InferenceGeo                string           `json:"inference_geo,omitempty"`   // 推理所在地理区域
	ServiceTier                 string           `json:"service_tier,omitempty"`    // 服务层级: "standard", "priority", "batch"
}

// RankingCacheTokens 返回未包含在原生 InputTokens 中的缓存输入总量。
func (u Usage) RankingCacheTokens() int64 {
	created := int64(u.CacheCreationInputTokens)
	if u.CacheCreation != nil {
		if detailed := int64(u.CacheCreation.Ephemeral5mInputTokens) + int64(u.CacheCreation.Ephemeral1hInputTokens); detailed > 0 {
			created = detailed
		}
	}
	return int64(u.CacheReadInputTokens) + created
}

// Error API 错误信息
type Error struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Response Claude API 响应结构
type Response struct {
	Id           string              `json:"id"`                      // 响应ID
	Type         string              `json:"type"`                    // "message"
	Role         string              `json:"role"`                    // "assistant"
	Content      []ContentBlockParam `json:"content"`                 // 内容块列表
	Model        string              `json:"model"`                   // 使用的模型
	StopReason   *string             `json:"stop_reason"`             // 停止原因: "end_turn", "max_tokens", "stop_sequence", "tool_use", "pause_turn", "refusal"
	StopSequence *string             `json:"stop_sequence,omitempty"` // 自定义停止序列
	Usage        *Usage              `json:"usage"`                   // token使用统计
	Error        *Error              `json:"error,omitempty"`         // 错误信息（如果有）
	Message      *Message            `json:"message,omitempty"`       // 消息
	ContentBlock *ContentBlockParam  `json:"content_block,omitempty"` // 内容块
	Delta        *Delta              `json:"delta,omitempty"`         // 增量内容
}

// WebSearchResultBlock 网络搜索结果块
type WebSearchResultBlock struct {
	EncryptedContent string `json:"encrypted_content"`
	PageAge          string `json:"page_age"`
	Title            string `json:"title"`
	Type             string `json:"type"` // "web_search_result"
	URL              string `json:"url"`
}

// WebSearchToolResultError 网络搜索工具结果错误
type WebSearchToolResultError struct {
	ErrorCode string `json:"error_code"` // "invalid_tool_input", "unavailable", "max_uses_exceeded", "too_many_requests", "query_too_long"
	Type      string `json:"type"`       // "web_search_tool_result_error"
}

// ContentBlockSourceContent 内容块来源内容
type ContentBlockSourceContent struct {
	Text         string                 `json:"text,omitempty"`
	Type         string                 `json:"type"` // "text"
	CacheControl *CacheControlEphemeral `json:"cache_control,omitempty"`
	Citations    []interface{}          `json:"citations,omitempty"`
}

// Delta 流式响应中的增量内容
type Delta struct {
	Type         string  `json:"type,omitempty"`          // "text_delta", "input_json_delta", "thinking_delta"
	Text         string  `json:"text,omitempty"`          // 文本增量
	Thinking     string  `json:"thinking,omitempty"`      // 思考增量（thinking模式）
	PartialJson  string  `json:"partial_json,omitempty"`  // 部分JSON（工具调用时）
	StopReason   *string `json:"stop_reason,omitempty"`   // 停止原因
	StopSequence *string `json:"stop_sequence,omitempty"` // 停止序列
	Usage        *Usage  `json:"usage,omitempty"`         // token使用统计
}

// StreamResponse 流式响应事件
type StreamResponse struct {
	Type         string             `json:"type"`                    // "message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop", "error"
	Message      *Response          `json:"message,omitempty"`       // 完整消息（message_start事件）
	Index        int                `json:"index,omitempty"`         // 内容块索引
	ContentBlock *ContentBlockParam `json:"content_block,omitempty"` // 内容块（content_block_start事件）
	Delta        *Delta             `json:"delta,omitempty"`         // 增量内容（content_block_delta事件）
	Error        *Error             `json:"error,omitempty"`         // 错误信息
	Usage        *Usage             `json:"usage,omitempty"`         // token使用统计
}

// 兼容性类型定义（为了向后兼容）
type Content ContentBlockParam

// 辅助函数：创建文本内容块
func NewTextContent(text string) ContentBlockParam {
	return ContentBlockParam{
		Type: "text",
		Text: text,
	}
}

// 辅助函数：创建带缓存控制的文本内容块
func NewTextContentWithCache(text string, ttl string) ContentBlockParam {
	return ContentBlockParam{
		Type: "text",
		Text: text,
		CacheControl: &CacheControlEphemeral{
			Type: "ephemeral",
			TTL:  ttl,
		},
	}
}

// 辅助函数：创建图片内容块 (Base64)
func NewImageContentFromBase64(data, mediaType string) ContentBlockParam {
	return ContentBlockParam{
		Type: "image",
		Source: Base64ImageSource{
			Type:      "base64",
			MediaType: mediaType,
			Data:      data,
		},
	}
}

// 辅助函数：创建图片内容块 (URL)
func NewImageContentFromURL(url string) ContentBlockParam {
	return ContentBlockParam{
		Type: "image",
		Source: URLImageSource{
			Type: "url",
			URL:  url,
		},
	}
}

// 辅助函数：创建文档内容块 (PDF Base64)
func NewDocumentContentFromPDF(data, title string) ContentBlockParam {
	return ContentBlockParam{
		Type:  "document",
		Title: title,
		Source: Base64PDFSource{
			Type:      "base64",
			MediaType: "application/pdf",
			Data:      data,
		},
	}
}

// 辅助函数：创建工具使用内容块
func NewToolUseContent(id, name string, input any) ContentBlockParam {
	return ContentBlockParam{
		Type:  "tool_use",
		Id:    id,
		Name:  name,
		Input: input,
	}
}

// 辅助函数：创建工具结果内容块
func NewToolResultContent(toolUseID string, content string) ContentBlockParam {
	return ContentBlockParam{
		Type:      "tool_result",
		ToolUseID: toolUseID,
		Content:   content,
	}
}

// 辅助函数：创建用户消息
func NewUserMessage(content ...ContentBlockParam) Message {
	return Message{
		Role:    "user",
		Content: content,
	}
}

// 辅助函数：创建助手消息
func NewAssistantMessage(content ...ContentBlockParam) Message {
	return Message{
		Role:    "assistant",
		Content: content,
	}
}

// 辅助函数：创建工具选择 (auto)
func NewToolChoiceAuto() *ToolChoice {
	return &ToolChoice{Type: "auto"}
}

// 辅助函数：创建工具选择 (any)
func NewToolChoiceAny() *ToolChoice {
	return &ToolChoice{Type: "any"}
}

// 辅助函数：创建工具选择 (specific tool)
func NewToolChoiceTool(name string) *ToolChoice {
	return &ToolChoice{
		Type: "tool",
		Name: name,
	}
}

// 辅助函数：创建带 JSON Schema 格式的输出配置
// 用于 Structured Outputs 功能
// schema 参数可以是 map[string]any 或其他符合 JSON Schema 规范的结构
func NewJSONSchemaOutputConfig(schema any) *OutputConfig {
	return &OutputConfig{
		Format: &JSONOutputFormat{
			Type:   "json_schema",
			Schema: schema,
		},
	}
}

// 辅助函数：创建带 effort 的输出配置
// effort 可选值: "low", "medium", "high", "max"
func NewEffortOutputConfig(effort string) *OutputConfig {
	return &OutputConfig{
		Effort: effort,
	}
}

// 辅助函数：创建带 strict 模式的工具
func NewStrictTool(name, description string, inputSchema InputSchema) Tool {
	return Tool{
		Name:        name,
		Description: description,
		InputSchema: inputSchema,
		Strict:      true,
	}
}

/*
使用示例：

1. 基本文本对话：

request := Request{
    Model: "claude-sonnet-4-5-20250929",
    Messages: []Message{
        NewUserMessage(NewTextContent("Hello, Claude!")),
    },
    MaxTokens: 1024,
}

2. 带缓存的系统提示：

request := Request{
    Model: "claude-sonnet-4-5-20250929",
    System: "You are a helpful assistant.",
    Messages: []Message{
        NewUserMessage(NewTextContentWithCache("Analyze this document", "1h")),
        NewUserMessage(NewDocumentContentFromPDF(pdfData, "Important Document")),
    },
    MaxTokens: 1024,
}

3. 工具调用：

tools := []Tool{{
    Name: "get_weather",
    Description: "Get current weather for a location",
    InputSchema: InputSchema{
        Type: "object",
        Properties: map[string]interface{}{
            "location": map[string]interface{}{
                "type": "string",
                "description": "City name",
            },
        },
        Required: []string{"location"},
    },
}}

request := Request{
    Model: "claude-sonnet-4-5-20250929",
    Messages: []Message{
        NewUserMessage(NewTextContent("What's the weather in Paris?")),
    },
    Tools: tools,
    ToolChoice: NewToolChoiceAuto(),
    MaxTokens: 1024,
}

4. 图片分析：

request := Request{
    Model: "claude-sonnet-4-5-20250929",
    Messages: []Message{
        NewUserMessage(
            NewTextContent("What's in this image?"),
            NewImageContentFromBase64(base64ImageData, "image/jpeg"),
        ),
    },
    MaxTokens: 1024,
}

5. 流式响应处理：

for event := range streamEvents {
    switch event.Type {
    case "content_block_delta":
        if event.Delta != nil && event.Delta.Text != "" {
            fmt.Print(event.Delta.Text)
        }
    case "message_stop":
        if event.Usage != nil {
            fmt.Printf("Total tokens: %d\n", event.Usage.InputTokens + event.Usage.OutputTokens)
        }
    }
}

6. 结构化输出 (Structured Outputs) - 保证返回符合 schema 的 JSON：

request := Request{
    Model: "claude-sonnet-4-5-20250929",
    Messages: []Message{
        NewUserMessage(NewTextContent("Extract info from: John Smith (john@example.com) wants Enterprise plan")),
    },
    MaxTokens: 1024,
    OutputConfig: &OutputConfig{
        Format: &JSONOutputFormat{
            Type: "json_schema",
            Schema: map[string]any{
                "type": "object",
                "properties": map[string]any{
                    "name":           map[string]any{"type": "string"},
                    "email":          map[string]any{"type": "string"},
                    "plan_interest":  map[string]any{"type": "string"},
                    "demo_requested": map[string]any{"type": "boolean"},
                },
                "required":             []string{"name", "email", "plan_interest", "demo_requested"},
                "additionalProperties": false,
            },
        },
    },
}

8. 输出配置 - 设置推理努力程度：

request := Request{
    Model: "claude-sonnet-4-5-20250929",
    Messages: []Message{
        NewUserMessage(NewTextContent("What is 2+2?")),
    },
    MaxTokens: 1024,
    OutputConfig: NewEffortOutputConfig("low"),
}

9. Thinking type 设为 adaptive，模型自行决定是否思考：

request := Request{
    Model: "claude-sonnet-4-5-20250929",
    Messages: []Message{
        NewUserMessage(NewTextContent("Solve this complex math problem...")),
    },
    MaxTokens: 16384,
    Thinking: &ThinkingConfig{
        Type: "adaptive",
    },
}

7. 严格工具调用 (Strict Tool Use) - 保证工具输入符合 schema：

tools := []Tool{{
    Name: "search_flights",
    Description: "Search for flights",
    Strict: true, // 启用严格模式
    InputSchema: InputSchema{
        Type: "object",
        Properties: map[string]interface{}{
            "destination": map[string]interface{}{
                "type": "string",
            },
            "date": map[string]interface{}{
                "type": "string",
                "format": "date",
            },
        },
        Required: []string{"destination", "date"},
    },
}}

request := Request{
    Model: "claude-sonnet-4-5-20250929",
    Messages: []Message{
        NewUserMessage(NewTextContent("Find flights to Paris next month")),
    },
    Tools: tools,
    MaxTokens: 1024,
}
*/
