package vertexai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/relay/channel/anthropic"
	"github.com/songquanpeng/one-api/relay/channel/gemini"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

type Adaptor struct {
	AccountCredentials Credentials
	IsAPIKeyMode       bool   // 是否使用 API Key 模式
	APIKey             string // API Key（仅在 API Key 模式下使用）
}

// Init implements channel.Adaptor.
func (a *Adaptor) Init(meta *util.RelayMeta) {
	// 检查认证模式（使用统一的检测方法）
	a.IsAPIKeyMode = meta.IsVertexAIAPIKeyMode()

	if a.IsAPIKeyMode {
		// API Key 模式：直接使用 API Key
		a.APIKey = meta.ActualAPIKey
		return
	}

	// JSON 模式：解析服务账号凭证
	keyIndex := 0
	if meta.KeyIndex != nil {
		keyIndex = *meta.KeyIndex
	}

	// 检查配置迁移状态
	CheckAndMigrateConfig(meta)

	// 验证配置是否正确（跳过系统级调用的验证）
	if meta.ChannelId != 0 {
		if err := ValidateVertexAIConfig(meta, keyIndex); err != nil {
			logger.SysError(fmt.Sprintf("[Vertex AI] 配置验证失败: %v", err))
		}
	}

	// 尝试解析当前密钥的凭证
	if credentials, err := parseCredentialsFromKey(meta, keyIndex); err == nil {
		a.AccountCredentials = *credentials
		return
	}

	// 回退：尝试从ADC配置解析
	if meta.Config.VertexAIADC != "" {
		var credentials Credentials
		if err := json.Unmarshal([]byte(meta.Config.VertexAIADC), &credentials); err == nil {
			a.AccountCredentials = credentials
		} else {
			logger.SysError(fmt.Sprintf("[Vertex AI] ADC配置解析失败: %v", err))
		}
	}
}

// GetRequestURL implements channel.Adaptor.
func (a *Adaptor) GetRequestURL(meta *util.RelayMeta) (string, error) {
	modelName := meta.ActualModelName
	if modelName == "" {
		modelName = meta.OriginModelName
	}
	if modelName == "" {
		modelName = "gemini-pro"
	}

	// Claude on Vertex 分支：URL 走 publishers/anthropic，action 用 rawPredict/streamRawPredict
	if isClaudeModel(modelName) {
		return a.buildClaudeRequestURL(meta, modelName)
	}

	// 处理 thinking 适配参数后缀
	// -thinking, -thinking-<budget>, -nothinking 只是适配参数，不是实际模型名的一部分
	modelName = stripThinkingSuffix(modelName)

	// 获取区域：优先使用模型专用区域，其次使用默认区域
	region := a.getModelRegion(meta, modelName)

	// 确定请求动作 - 优先从请求路径提取（支持 Gemini 原生格式）
	suffix := a.extractActionFromPath(meta.RequestURLPath)
	if suffix == "" {
		// 回退到默认动作
		suffix = "generateContent"
		if meta.IsStream {
			suffix = "streamGenerateContent?alt=sse"
		}
	}

	if a.IsAPIKeyMode {
		// API Key 模式：不需要 project ID，使用简化的 URL
		var keyPrefix string
		if strings.Contains(suffix, "?") {
			keyPrefix = "&"
		} else {
			keyPrefix = "?"
		}

		if region == "global" {
			return fmt.Sprintf(
				"https://aiplatform.googleapis.com/v1/publishers/google/models/%s:%s%skey=%s",
				modelName, suffix, keyPrefix, a.APIKey,
			), nil
		}
		return fmt.Sprintf(
			"https://%s-aiplatform.googleapis.com/v1/publishers/google/models/%s:%s%skey=%s",
			region, modelName, suffix, keyPrefix, a.APIKey,
		), nil
	}

	// JSON 模式：需要 project ID
	keyIndex := 0
	if meta.KeyIndex != nil {
		keyIndex = *meta.KeyIndex
	}

	projectID := extractProjectIDFromKey(meta, keyIndex)
	if projectID == "" && a.AccountCredentials.ProjectID != "" {
		projectID = a.AccountCredentials.ProjectID
	}

	if projectID == "" {
		return "", fmt.Errorf("vertex AI project ID not found in Key field or credentials")
	}

	// 构建Vertex AI API URL
	if region == "global" {
		return fmt.Sprintf(
			"https://aiplatform.googleapis.com/v1/projects/%s/locations/global/publishers/google/models/%s:%s",
			projectID, modelName, suffix,
		), nil
	}
	return fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:%s",
		region, projectID, region, modelName, suffix,
	), nil
}

// getModelRegion 获取模型的区域，支持模型专用区域配置
func (a *Adaptor) getModelRegion(meta *util.RelayMeta, modelName string) string {
	// 优先检查模型专用区域映射
	if meta.Config.VertexModelRegion != nil {
		if region, ok := meta.Config.VertexModelRegion[modelName]; ok && region != "" {
			return region
		}
	}

	// 使用默认区域
	if meta.Config.Region != "" {
		return meta.Config.Region
	}

	return "global"
}

// extractActionFromPath 从请求路径中提取动作名称
// 例如: /v1beta/models/gemini-2.0-flash:generateContent -> generateContent
// 例如: /v1beta/models/gemini-2.0-flash:streamGenerateContent?alt=sse -> streamGenerateContent?alt=sse
func (a *Adaptor) extractActionFromPath(path string) string {
	if path == "" {
		return ""
	}

	// 先分离查询参数
	pathOnly := path
	queryString := ""
	if qIdx := strings.Index(path, "?"); qIdx != -1 {
		pathOnly = path[:qIdx]
		queryString = path[qIdx:] // 包含 ?
	}

	// 查找冒号后的动作部分
	colonIdx := strings.LastIndex(pathOnly, ":")
	if colonIdx == -1 {
		return ""
	}

	action := pathOnly[colonIdx+1:]
	// 去除前后空白
	action = strings.TrimSpace(action)

	// 如果原始请求有查询参数，保留它（但排除 key 参数）
	if queryString != "" {
		// 解析并过滤掉 key 参数（避免重复添加）
		filteredQuery := filterQueryParams(queryString, "key")
		if filteredQuery != "" {
			action = action + filteredQuery
		}
	}

	// 如果是流式请求且没有 alt=sse 参数，添加它
	if action == "streamGenerateContent" {
		action = "streamGenerateContent?alt=sse"
	}

	return action
}

// filterQueryParams 过滤掉指定的查询参数
func filterQueryParams(queryString string, excludeParams ...string) string {
	if queryString == "" {
		return ""
	}

	// 移除开头的 ?
	query := strings.TrimPrefix(queryString, "?")
	if query == "" {
		return ""
	}

	excludeSet := make(map[string]bool)
	for _, p := range excludeParams {
		excludeSet[p] = true
	}

	parts := strings.Split(query, "&")
	var filtered []string
	for _, part := range parts {
		if part == "" {
			continue
		}
		key := part
		if eqIdx := strings.Index(part, "="); eqIdx != -1 {
			key = part[:eqIdx]
		}
		if !excludeSet[key] {
			filtered = append(filtered, part)
		}
	}

	if len(filtered) == 0 {
		return ""
	}
	return "?" + strings.Join(filtered, "&")
}

// SetupRequestHeader implements channel.Adaptor.
func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Request, meta *util.RelayMeta) error {
	req.Header.Set("Content-Type", "application/json")

	// Claude on Vertex：显式清掉客户端传来的 anthropic 相关 header，
	// 避免把无意义的 x-api-key / anthropic-version 透传给 Google 上游（Vertex 用 body 里的 anthropic_version，不认 header）
	if isClaudeModel(meta.ActualModelName) {
		req.Header.Del("x-api-key")
		req.Header.Del("anthropic-version")
		req.Header.Del("anthropic-beta")
	}

	// API Key 模式不需要 Authorization 头，key 已经在 URL 中
	if a.IsAPIKeyMode {
		return nil
	}

	// JSON 模式：获取访问令牌并设置到请求头
	accessToken, err := GetAccessToken(a, meta)
	if err != nil {
		return fmt.Errorf("failed to get access token: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)

	// 设置项目头（可选）
	if a.AccountCredentials.ProjectID != "" {
		req.Header.Set("x-goog-user-project", a.AccountCredentials.ProjectID)
	}

	return nil
}

// ConvertRequest implements channel.Adaptor.
func (a *Adaptor) ConvertRequest(c *gin.Context, relayMode int, request *model.GeneralOpenAIRequest) (any, error) {
	if request == nil {
		return nil, fmt.Errorf("request is nil")
	}
	// Claude on Vertex：走 Anthropic messages 格式；后续 DoRequest 会再调用
	// rewriteBodyForVertexClaude 删除 model 字段并注入 anthropic_version。
	// 正常业务请求走 RelayClaudeNative 不经过这里，只有渠道测试等会命中。
	if isClaudeModel(request.Model) {
		claudeReq := anthropic.ConvertRequest(*request)
		if claudeReq.MaxTokens <= 0 {
			claudeReq.MaxTokens = 16
		}
		return claudeReq, nil
	}
	// 使用 Gemini 的转换函数将 OpenAI 格式转换为 Gemini 格式
	// Vertex AI 使用与 Gemini 相同的请求格式
	return gemini.ConvertRequest(*request)
}

// ConvertImageRequest implements channel.Adaptor.
func (a *Adaptor) ConvertImageRequest(request *model.ImageRequest) (any, error) {
	// 将图像请求转换为VertexAI格式
	// 对于不支持的图像模型，返回错误而不是panic
	return nil, fmt.Errorf("image model %s is not supported by VertexAI adapter", request.Model)
}

// DoRequest implements channel.Adaptor.
func (a *Adaptor) DoRequest(c *gin.Context, meta *util.RelayMeta, requestBody io.Reader) (*http.Response, error) {
	// Claude on Vertex：RelayClaudeNative 直接把原始 Anthropic 请求体塞过来，
	// 不走 ConvertRequest。这里需要注入 anthropic_version 并剔除 model 字段，
	// 否则 Vertex 的 Anthropic publisher 会返回 400。
	// URL 和参数适配都以重定向后的模型为准。
	claudeModel := meta.ActualModelName
	if claudeModel == "" {
		claudeModel = meta.OriginModelName
	}
	if isClaudeModel(claudeModel) {
		raw, readErr := io.ReadAll(requestBody)
		if readErr != nil {
			return nil, fmt.Errorf("failed to read request body for claude rewrite: %w", readErr)
		}
		var betaHeader string
		if c != nil && c.Request != nil {
			betaHeader = c.Request.Header.Get("anthropic-beta")
		}
		rewritten, rewriteErr := rewriteBodyForVertexClaude(raw, claudeModel, betaHeader)
		if rewriteErr != nil {
			return nil, rewriteErr
		}
		requestBody = bytes.NewReader(rewritten)
	}

	// 获取请求URL
	url, err := a.GetRequestURL(meta)
	if err != nil {
		return nil, fmt.Errorf("failed to get request URL: %w", err)
	}

	// 创建HTTP请求，绑定客户端上下文以支持取消
	// 这样当客户端断开连接时，请求会被正确取消，避免内存泄漏
	req, err := http.NewRequestWithContext(c.Request.Context(), "POST", url, requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// 设置请求头（包括认证）
	if err := a.SetupRequestHeader(c, req, meta); err != nil {
		return nil, fmt.Errorf("failed to setup request headers: %w", err)
	}

	// 使用标准 HTTPClient 执行请求，超时由 RELAY_TIMEOUT 环境变量控制
	return util.HTTPClient.Do(req)
}

// DoResponse implements channel.Adaptor.
func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, meta *util.RelayMeta) (usage *model.Usage, err *model.ErrorWithStatusCode) {
	// Claude on Vertex：响应体是 Anthropic 原生格式，需要用 anthropic handler。
	// 正常业务走 RelayClaudeNative 不经过这里，仅渠道测试 / 非 native 路径命中。
	if isClaudeModel(meta.ActualModelName) || isClaudeModel(meta.OriginModelName) {
		if meta.IsStream {
			err, usage = anthropic.StreamHandler(c, resp, meta)
		} else {
			err, usage = anthropic.Handler(c, resp, meta.PromptTokens, meta.ActualModelName)
		}
		return
	}
	// 使用 Gemini 的响应处理函数
	// Vertex AI 返回的响应格式与 Gemini 相同
	if meta.IsStream {
		var responseText string
		err, responseText = gemini.StreamHandler(c, resp, meta.ActualModelName)
		usage = openai.ResponseText2Usage(responseText, meta.ActualModelName, meta.PromptTokens)
	} else {
		err, usage = gemini.Handler(c, resp, meta.PromptTokens, meta.ActualModelName)
	}
	return
}

// GetModelList implements channel.Adaptor.
func (a *Adaptor) GetModelList() []string {
	// 返回支持的模型列表
	return ModelList
}

// GetModelDetails implements channel.Adaptor.
func (a *Adaptor) GetModelDetails() []model.APIModel {
	// 返回详细的模型信息
	return []model.APIModel{}
}

// GetChannelName implements channel.Adaptor.
func (a *Adaptor) GetChannelName() string {
	return "vertexai"
}

// HandleErrorResponse 处理Vertex AI错误响应
func (a *Adaptor) HandleErrorResponse(resp *http.Response) *model.ErrorWithStatusCode {
	// 返回nil让通用处理器处理，保留原始错误信息
	return nil
}

// buildClaudeRequestURL 拼 Vertex 上 Anthropic publisher 的请求 URL。
// Claude 只支持 JSON 凭证模式（Google Cloud API Key 不能访问 Anthropic publisher）。
// 与 Gemini 分支不同，这里**不**消费 meta.RequestURLPath 里的 action：
// Anthropic publisher 只接受 rawPredict / streamRawPredict，由 meta.IsStream 唯一决定，
// 客户端传来的任何自定义 action（例如 :countTokens）都会被忽略。
func (a *Adaptor) buildClaudeRequestURL(meta *util.RelayMeta, modelName string) (string, error) {
	if a.IsAPIKeyMode {
		return "", fmt.Errorf("claude on Vertex does not support API Key auth mode; use service-account JSON credentials")
	}

	region := a.getModelRegion(meta, modelName)
	suffix := claudeSuffix(meta.IsStream)
	// -thinking 只是适配层后缀，Vertex URL 只认基础模型名
	urlModel := mapClaudeModelForURL(anthropic.GetBaseModelName(modelName))

	// 获取 project ID（同 Gemini 路径逻辑）
	keyIndex := 0
	if meta.KeyIndex != nil {
		keyIndex = *meta.KeyIndex
	}
	projectID := extractProjectIDFromKey(meta, keyIndex)
	if projectID == "" && a.AccountCredentials.ProjectID != "" {
		projectID = a.AccountCredentials.ProjectID
	}
	if projectID == "" {
		return "", fmt.Errorf("vertex AI project ID not found in Key field or credentials")
	}

	if region == "global" {
		return fmt.Sprintf(
			"https://aiplatform.googleapis.com/v1/projects/%s/locations/global/publishers/anthropic/models/%s:%s",
			projectID, urlModel, suffix,
		), nil
	}
	return fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:%s",
		region, projectID, region, urlModel, suffix,
	), nil
}

// stripThinkingSuffix 移除模型名称中的 thinking 适配参数后缀
// 支持的格式：
//   - model-thinking: 移除 -thinking 后缀
//   - model-thinking-<budget>: 移除 -thinking-<budget> 后缀（如 -thinking-1024）
//   - model-nothinking: 移除 -nothinking 后缀
//
// 这些后缀只是用于触发 thinking 模式的适配参数，不是实际的 Vertex AI 模型名
func stripThinkingSuffix(modelName string) string {
	// 处理 -thinking-<budget> 格式（如 gemini-2.0-flash-thinking-1024）
	if idx := strings.Index(modelName, "-thinking-"); idx != -1 {
		return modelName[:idx]
	}

	// 处理 -thinking 后缀
	if strings.HasSuffix(modelName, "-thinking") {
		return strings.TrimSuffix(modelName, "-thinking")
	}

	// 处理 -nothinking 后缀
	if strings.HasSuffix(modelName, "-nothinking") {
		return strings.TrimSuffix(modelName, "-nothinking")
	}

	return modelName
}
