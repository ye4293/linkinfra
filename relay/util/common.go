package util

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
	relaymodel "github.com/songquanpeng/one-api/relay/model"

	"github.com/gin-gonic/gin"
)

func ShouldDisableChannel(err *relaymodel.Error, statusCode int) bool {
	if !config.AutomaticDisableChannelEnabled {
		return false
	}
	if err == nil {
		return false
	}

	// 检查401状态码 - 通常是认证问题，应该禁用
	if statusCode == http.StatusUnauthorized {
		return true
	}

	// 移除403状态码的直接检查，改为完全依靠关键字判断

	// 检查错误类型
	switch err.Type {
	case "insufficient_quota", "authentication_error", "permission_error", "forbidden":
		return true
	}

	// 检查错误代码
	if err.Code == "invalid_api_key" || err.Code == "account_deactivated" || err.Code == "Some resource has been exhausted" {
		return true
	}

	// 使用可配置的关键词进行检查（按行分割，忽略大小写）
	config.OptionMapRWMutex.RLock()
	autoDisableKeywords := config.AutoDisableKeywords
	config.OptionMapRWMutex.RUnlock()

	if autoDisableKeywords != "" {
		message := strings.ToLower(err.Message)
		keywords := strings.Split(autoDisableKeywords, "\n")

		for _, keyword := range keywords {
			keyword = strings.TrimSpace(strings.ToLower(keyword))
			if keyword != "" && strings.Contains(message, keyword) {
				return true
			}
		}
	}

	return false
}

func ShouldEnableChannel(err error, openAIErr *relaymodel.Error) bool {
	if !config.AutomaticEnableChannelEnabled {
		return false
	}
	if err != nil {
		return false
	}
	if openAIErr != nil {
		return false
	}
	return true
}

// OpenRouterMetadata OpenRouter 错误响应中的 metadata 字段
type OpenRouterMetadata struct {
	Raw          string `json:"raw"`
	ProviderName string `json:"provider_name"`
}

// OpenRouterError 扩展的 OpenRouter 错误结构
type OpenRouterError struct {
	relaymodel.Error
	Metadata *OpenRouterMetadata `json:"metadata,omitempty"`
}

type GeneralErrorResponse struct {
	Error    OpenRouterError `json:"error"`
	Message  string          `json:"message"`
	Msg      string          `json:"msg"`
	Err      string          `json:"err"`
	ErrorMsg string          `json:"error_msg"`
	Header   struct {
		Message string `json:"message"`
	} `json:"header"`
	Response struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
}

func (e GeneralErrorResponse) ToMessage() string {
	if e.Error.Message != "" {
		return e.Error.Message
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Msg != "" {
		return e.Msg
	}
	if e.Err != "" {
		return e.Err
	}
	if e.ErrorMsg != "" {
		return e.ErrorMsg
	}
	if e.Header.Message != "" {
		return e.Header.Message
	}
	if e.Response.Error.Message != "" {
		return e.Response.Error.Message
	}
	return ""
}

func RelayErrorHandler(resp *http.Response) (ErrorWithStatusCode *relaymodel.ErrorWithStatusCode) {
	ErrorWithStatusCode = &relaymodel.ErrorWithStatusCode{
		StatusCode: resp.StatusCode,
		Error: relaymodel.Error{
			Message: "",
			Type:    "upstream_error",
			Code:    "bad_response_status_code",
			Param:   strconv.Itoa(resp.StatusCode),
		},
	}

	// ✅ 关键修复：使用 defer 确保响应体一定会被关闭
	defer func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	if config.DebugEnabled {
		logger.SysLog(fmt.Sprintf("error happened, status code: %d, response: \n%s", resp.StatusCode, string(responseBody)))
	}

	var errResponse GeneralErrorResponse
	err = json.Unmarshal(responseBody, &errResponse)
	if err != nil {
		return
	}
	if errResponse.Error.Message != "" {
		// OpenAI format error, so we override the default one
		ErrorWithStatusCode.Error = errResponse.Error.Error
		// OpenRouter: 提取 metadata 中的实际错误信息
		if errResponse.Error.Metadata != nil && errResponse.Error.Metadata.ProviderName != "" {
			realMsg := extractOpenRouterRealError(errResponse.Error.Metadata)
			if realMsg != "" {
				ErrorWithStatusCode.Error.Message = fmt.Sprintf("[%s] %s", errResponse.Error.Metadata.ProviderName, realMsg)
			} else {
				ErrorWithStatusCode.Error.Message = fmt.Sprintf("[%s] %s", errResponse.Error.Metadata.ProviderName, errResponse.Error.Message)
			}
		}
	} else {
		ErrorWithStatusCode.Error.Message = errResponse.ToMessage()
	}
	if ErrorWithStatusCode.Error.Message == "" {
		// 提供更详细的错误信息
		switch resp.StatusCode {
		case 504:
			ErrorWithStatusCode.Error.Message = fmt.Sprintf("Gateway timeout (504): upstream server timed out. Please retry or check your API service.")
		case 502:
			ErrorWithStatusCode.Error.Message = fmt.Sprintf("Bad gateway (502): upstream server returned an invalid response.")
		case 503:
			ErrorWithStatusCode.Error.Message = fmt.Sprintf("Service unavailable (503): upstream server is temporarily unable to handle requests.")
		case 429:
			ErrorWithStatusCode.Error.Message = fmt.Sprintf("Too many requests (429): API rate limit reached. Please retry after a moment.")
		case 401:
			ErrorWithStatusCode.Error.Message = fmt.Sprintf("Unauthorized (401): API key is invalid or expired.")
		case 403:
			ErrorWithStatusCode.Error.Message = fmt.Sprintf("Forbidden (403): you do not have access to this resource or model.")
		case 404:
			ErrorWithStatusCode.Error.Message = fmt.Sprintf("Not found (404): the requested endpoint or model does not exist.")
		default:
			ErrorWithStatusCode.Error.Message = fmt.Sprintf("Upstream error (status %d).", resp.StatusCode)
		}
	}
	return
}

// extractOpenRouterRealError 从 OpenRouter metadata.raw 中提取实际的上游错误信息
func extractOpenRouterRealError(metadata *OpenRouterMetadata) string {
	if metadata.Raw == "" {
		return ""
	}
	raw := metadata.Raw
	// 尝试解析为标准 OpenAI 错误格式: {"error":{"message":"..."}}
	var errResp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &errResp); err == nil && errResp.Error.Message != "" {
		return errResp.Error.Message
	}
	// 尝试直接提取顶层 message 字段: {"message":"..."}
	var simple struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(raw), &simple); err == nil && simple.Message != "" {
		return simple.Message
	}
	// 无法解析为 JSON，返回截断的原始内容
	if len(raw) > 200 {
		return raw[:200] + "..."
	}
	return raw
}

// RelayErrorHandlerWithAdaptor 使用 adaptor 特定的错误处理逻辑
func RelayErrorHandlerWithAdaptor(resp *http.Response, adaptor interface{}) (ErrorWithStatusCode *relaymodel.ErrorWithStatusCode) {
	// ✅ 关键修复：确保响应体一定会被关闭，避免内存泄漏
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()

	// 首先尝试使用 adaptor 的错误处理方法（如果它实现了 ErrorHandler 接口）
	if errorHandler, ok := adaptor.(interface {
		HandleErrorResponse(resp *http.Response) *relaymodel.ErrorWithStatusCode
	}); ok {
		if adaptorError := errorHandler.HandleErrorResponse(resp); adaptorError != nil {
			return adaptorError
		}
	}

	// 如果 adaptor 无法处理，回退到通用处理器（注意：RelayErrorHandler 内部也会关闭响应体，但多次关闭是安全的）
	return RelayErrorHandler(resp)
}

func GetFullRequestURL(baseURL string, requestURL string, channelType int) string {
	fullRequestURL := fmt.Sprintf("%s%s", baseURL, requestURL)

	if strings.HasPrefix(baseURL, "https://gateway.ai.cloudflare.com") {
		switch channelType {
		case common.ChannelTypeOpenAI:
			fullRequestURL = fmt.Sprintf("%s%s", baseURL, strings.TrimPrefix(requestURL, "/v1"))
		case common.ChannelTypeAzure:
			fullRequestURL = fmt.Sprintf("%s%s", baseURL, strings.TrimPrefix(requestURL, "/openai/deployments"))
		}
	}
	if channelType == 24 { //google gemini
		fullRequestURL = fmt.Sprintf("%s/v1beta/openai/images/generations", baseURL)
	}
	logger.SysLog("fullRequestURL: " + fullRequestURL)

	return fullRequestURL
}

func PostConsumeQuota(ctx context.Context, tokenId int, quotaDelta int64, totalQuota int64, userId int, channelId int, modelRatio float64, groupRatio float64, modelName string, tokenName string, duration float64, title string, httpReferer string) {
	// quotaDelta is remaining quota to be consumed
	err := model.PostConsumeTokenQuota(tokenId, quotaDelta)
	if err != nil {
		logger.Error(ctx, "error consuming token remain quota: "+err.Error())
	}
	err = model.CacheUpdateUserQuota(ctx, userId)
	if err != nil {
		logger.Error(ctx, "error update user quota cache: "+err.Error())
	}
	// totalQuota is total quota consumed
	if totalQuota != 0 {
		logContent := fmt.Sprintf("模型倍率 %.2f，分组倍率 %.2f", modelRatio, groupRatio)
		model.RecordConsumeLog(ctx, userId, channelId, int(totalQuota), 0, modelName, tokenName, totalQuota, logContent, duration, title, httpReferer, false, 0.0)
		model.UpdateUserUsedQuotaAndRequestCount(userId, totalQuota)
		model.UpdateChannelUsedQuota(channelId, totalQuota)
	}
	if totalQuota <= 0 {
		logger.Error(ctx, fmt.Sprintf("totalQuota consumed is %d, something is wrong", totalQuota))
	}
}

// PostConsumeQuotaWithTokens 处理包含分离的输入和输出token的配额消费
func PostConsumeQuotaWithTokens(ctx context.Context, tokenId int, quotaDelta int64, totalQuota int64, userId int, channelId int, modelRatio float64, groupRatio float64, modelName string, tokenName string, duration float64, title string, httpReferer string, inputTokens int64, outputTokens int64) {
	// quotaDelta is remaining quota to be consumed
	err := model.PostConsumeTokenQuota(tokenId, quotaDelta)
	if err != nil {
		logger.Error(ctx, "error consuming token remain quota: "+err.Error())
	}
	err = model.CacheUpdateUserQuota(ctx, userId)
	if err != nil {
		logger.Error(ctx, "error update user quota cache: "+err.Error())
	}
	// totalQuota is total quota consumed
	if totalQuota != 0 {
		logContent := fmt.Sprintf("模型倍率 %.2f，分组倍率 %.2f", modelRatio, groupRatio)
		// 正确记录inputTokens和outputTokens
		model.RecordConsumeLog(ctx, userId, channelId, int(inputTokens), int(outputTokens), modelName, tokenName, totalQuota, logContent, duration, title, httpReferer, false, 0.0)
		model.UpdateUserUsedQuotaAndRequestCount(userId, totalQuota)
		model.UpdateChannelUsedQuota(channelId, totalQuota)
	}
	if totalQuota <= 0 {
		logger.Error(ctx, fmt.Sprintf("totalQuota consumed is %d, something is wrong", totalQuota))
	}
}

// PostConsumeQuotaWithDetailedTokens 处理包含详细token分类的配额消费（用于音频转录等）
func PostConsumeQuotaWithDetailedTokens(ctx context.Context, tokenId int, quotaDelta int64, totalQuota int64, userId int, channelId int, modelRatio float64, groupRatio float64, modelName string, tokenName string, duration float64, title string, httpReferer string, inputTokens int64, outputTokens int64, textInput int64, textOutput int64, audioInput int64, audioOutput int64) {
	// quotaDelta is remaining quota to be consumed
	err := model.PostConsumeTokenQuota(tokenId, quotaDelta)
	if err != nil {
		logger.Error(ctx, "error consuming token remain quota: "+err.Error())
	}
	err = model.CacheUpdateUserQuota(ctx, userId)
	if err != nil {
		logger.Error(ctx, "error update user quota cache: "+err.Error())
	}
	// totalQuota is total quota consumed
	if totalQuota != 0 {
		logContent := fmt.Sprintf("模型倍率 %.2f，分组倍率 %.2f", modelRatio, groupRatio)

		// 创建详细的token信息JSON
		otherInfo := fmt.Sprintf(`{"text_input":%d,"text_output":%d,"audio_input":%d,"audio_output":%d}`,
			textInput, textOutput, audioInput, audioOutput)

		// 正确记录inputTokens和outputTokens，并添加详细信息到other字段
		model.RecordConsumeLogWithOther(ctx, userId, channelId, int(inputTokens), int(outputTokens), modelName, tokenName, totalQuota, logContent, duration, title, httpReferer, false, 0.0, otherInfo)
		model.UpdateUserUsedQuotaAndRequestCount(userId, totalQuota)
		model.UpdateChannelUsedQuota(channelId, totalQuota)
	}
	if totalQuota <= 0 {
		logger.Error(ctx, fmt.Sprintf("totalQuota consumed is %d, something is wrong", totalQuota))
	}
}

func GetAzureAPIVersion(c *gin.Context) string {
	query := c.Request.URL.Query()
	apiVersion := query.Get("api-version")
	if apiVersion == "" {
		apiVersion = c.GetString(common.ConfigKeyAPIVersion)
	}
	// 如果还是空，使用默认版本
	if apiVersion == "" {
		apiVersion = "2025-04-01-preview"
	}
	return apiVersion
}

func ProcessString(input string) string {
	// 使用正则表达式匹配域名和IP
	re := regexp.MustCompile(`(https?://)?([a-zA-Z0-9.-]+\.[a-zA-Z]{2,}|[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3})(:[0-9]+)?`)

	// 替换函数
	replacer := func(match string) string {
		// 去除协议部分
		if strings.HasPrefix(match, "http://") || strings.HasPrefix(match, "https://") {
			match = match[strings.Index(match, "://")+3:]
		}

		// 去除可能存在的端口号
		parts := strings.Split(match, ":")
		host := parts[0]

		// 判断是域名还是IP
		if ip := net.ParseIP(host); ip != nil {
			// 如果是IP,替换为<host>
			return "<host>"
		} else {
			// 如果是域名,移除域名部分,保留路径
			pathIndex := strings.Index(match, "/")
			if pathIndex != -1 {
				return match[pathIndex:]
			} else {
				return ""
			}
		}
	}

	// 替换字符串中的域名和IP
	result := re.ReplaceAllStringFunc(input, replacer)

	return result
}
func CloseResponseBodyGracefully(httpResponse *http.Response) {
	if httpResponse == nil || httpResponse.Body == nil {
		return
	}
	err := httpResponse.Body.Close()
	if err != nil {
		logger.SysLog(fmt.Sprintf("failed to close response body: %s", err.Error()))
	}
}

func IOCopyBytesGracefully(c *gin.Context, src *http.Response, data []byte) {
	if c.Writer == nil {
		return
	}

	body := io.NopCloser(bytes.NewBuffer(data))

	// We shouldn't set the header before we parse the response body, because the parse part may fail.
	// And then we will have to send an error response, but in this case, the header has already been set.
	// So the httpClient will be confused by the response.
	// For example, Postman will report error, and we cannot check the response at all.
	if src != nil {
		for k, v := range src.Header {
			// avoid setting Content-Length
			if k == "Content-Length" {
				continue
			}
			c.Writer.Header().Set(k, v[0])
		}
	}

	// set Content-Length header manually BEFORE calling WriteHeader
	c.Writer.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))

	// Write header with status code (this sends the headers)
	if src != nil {
		c.Writer.WriteHeader(src.StatusCode)
	} else {
		c.Writer.WriteHeader(http.StatusOK)
	}

	_, err := io.Copy(c.Writer, body)
	if err != nil {
		logger.Error(c.Request.Context(), fmt.Sprintf("failed to copy response body: %s", err.Error()))
	}
}
