package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	dbmodel "github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/channel"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

// systemOneRequest 保留原始 JSON 字段，避免结构化状态、问题或扩展参数丢失。
func systemOneRequest(body []byte, mapping map[string]string) ([]byte, string, string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, "", "", err
	}
	var name string
	if err := json.Unmarshal(fields["model"], &name); err != nil || strings.TrimSpace(name) == "" {
		return nil, "", "", fmt.Errorf("model is required")
	}
	state := bytes.TrimSpace(fields["state"])
	if len(state) == 0 || (state[0] != '"' && state[0] != '{' && state[0] != '[') {
		return nil, "", "", fmt.Errorf("state must be a string, object or array")
	}
	var questions map[string]json.RawMessage
	if err := json.Unmarshal(fields["questions"], &questions); err != nil || len(questions) == 0 {
		return nil, "", "", fmt.Errorf("questions must be a non-empty object")
	}
	if raw, ok := fields["stream"]; ok {
		var stream bool
		if err := json.Unmarshal(raw, &stream); err != nil || stream {
			return nil, "", "", fmt.Errorf("systemone does not support streaming")
		}
	}
	actual, mapped := util.GetMappedModelName(name, mapping)
	if mapped {
		fields["model"], _ = json.Marshal(actual)
		var err error
		body, err = json.Marshal(fields)
		if err != nil {
			return nil, "", "", err
		}
	}
	return body, name, actual, nil
}

// SystemOneRequestURL 同时供原生转发与管理后台渠道测试使用。
func SystemOneRequestURL(baseURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid TypeSafe channel base URL")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	switch {
	case strings.HasSuffix(u.Path, "/v1/systemone"):
	case strings.HasSuffix(u.Path, "/v1"):
		u.Path += "/systemone"
	default:
		u.Path += "/v1/systemone"
	}
	return u.String(), nil
}

// ParseSystemOneUsage 校验原生响应并提取通用 token 用量。
func ParseSystemOneUsage(body []byte) (*model.Usage, error) {
	var response struct {
		Model   string                     `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   *struct {
			Input  *int `json:"input_tokens"`
			Output *int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("invalid systemone response JSON")
	}
	if response.Model == "" || len(response.Answers) == 0 || response.Usage == nil || response.Usage.Input == nil || response.Usage.Output == nil {
		return nil, fmt.Errorf("systemone response is missing model, answers or token usage")
	}
	input, output := *response.Usage.Input, *response.Usage.Output
	if input < 0 || output < 0 || input > int(^uint(0)>>1)-output {
		return nil, fmt.Errorf("invalid systemone token usage")
	}
	return &model.Usage{PromptTokens: input, CompletionTokens: output, TotalTokens: input + output}, nil
}

// RelaySystemOneHelper 使用原生协议请求上游，复用通用结算和消费日志。
func RelaySystemOneHelper(c *gin.Context) *model.ErrorWithStatusCode {
	c.Writer.Header().Del("Retry-After")
	start := time.Now()
	ctx := context.WithoutCancel(c.Request.Context())
	body, err := common.GetRequestBody(c)
	if err != nil {
		return openai.ErrorWrapper(err, "invalid_systemone_request", http.StatusBadRequest)
	}
	meta := util.GetRelayMeta(c)
	body, original, actual, err := systemOneRequest(body, meta.ModelMapping)
	if err != nil {
		return openai.ErrorWrapper(err, "invalid_systemone_request", http.StatusBadRequest)
	}
	meta.OriginModelName, meta.ActualModelName = original, actual
	requestURL, err := SystemOneRequestURL(meta.BaseURL)
	if err != nil {
		return openai.ErrorWrapper(err, "invalid_systemone_channel", http.StatusInternalServerError)
	}
	// 与通用 relay 一致：上游完成后仍需结算，不因客户端断线取消请求。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return openai.ErrorWrapper(err, "systemone_request_failed", http.StatusInternalServerError)
	}
	key := meta.ActualAPIKey
	if key == "" {
		key = meta.APIKey
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	channel.ApplyHeadersOverride(req, meta)
	tariff, err := newSystemOneTariff(meta)
	if err != nil {
		c.Set("systemone_no_retry", true)
		return openai.ErrorWrapper(err, "invalid_systemone_price", http.StatusInternalServerError)
	}
	// 字节数只用于预估额度；最终只采用 TypeSafe 返回的实际 token 用量。
	if config.PreConsumedQuota < 0 || config.PreConsumedQuota > int64(int(^uint(0)>>1)-len(body)) {
		c.Set("systemone_no_retry", true)
		return openai.ErrorWrapper(fmt.Errorf("invalid reservation configuration"), "invalid_systemone_price", http.StatusInternalServerError)
	}
	reserved, err := tariff.quota(len(body)+int(config.PreConsumedQuota), 0)
	if err != nil {
		c.Set("systemone_no_retry", true)
		return openai.ErrorWrapper(err, "invalid_systemone_price", http.StatusInternalServerError)
	}
	// 始终检查令牌和用户两种额度，避免用户余额充足时绕过令牌额度。
	if err := dbmodel.AdjustRelayQuota(meta.UserId, meta.TokenId, reserved, true); err != nil {
		c.Set("systemone_no_retry", true)
		if errors.Is(err, dbmodel.ErrInsufficientRelayQuota) {
			return openai.ErrorWrapper(err, "insufficient_systemone_quota", http.StatusForbidden)
		}
		logger.Error(ctx, "systemone reservation failed: "+err.Error())
		return openai.ErrorWrapper(fmt.Errorf("systemone quota reservation failed"), "systemone_billing_failed", http.StatusInternalServerError)
	}
	_ = dbmodel.CacheUpdateUserQuota2(meta.UserId)
	settled := false
	defer func() {
		if !settled {
			if reserved != 0 {
				if err := dbmodel.AdjustRelayQuota(meta.UserId, meta.TokenId, -reserved, false); err != nil {
					c.Set("systemone_no_retry", true)
					logger.Error(ctx, "systemone quota refund failed: "+err.Error())
				}
			}
		}
		if err := dbmodel.CacheUpdateUserQuota2(meta.UserId); err != nil {
			logger.Error(ctx, "systemone quota cache refresh failed: "+err.Error())
		}
	}()
	c.Set("metrics_upstream_started", true)
	resp, err := channel.DoRequest(c, req, meta)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		// 不把包含渠道 URL 的底层网络错误发送给客户端。
		return openai.ErrorWrapper(fmt.Errorf("TypeSafe upstream request failed"), "systemone_request_failed", http.StatusBadGateway)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			c.Header("Retry-After", retryAfter)
		}
		return systemOneError(resp)
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return openai.ErrorWrapper(fmt.Errorf("failed to read systemone response"), "systemone_response_failed", http.StatusBadGateway)
	}
	usage, err := ParseSystemOneUsage(responseBody)
	if err != nil {
		return openai.ErrorWrapper(err, "invalid_systemone_response", http.StatusBadGateway)
	}
	if err := ValidateSystemOneAnswers(body, responseBody); err != nil {
		return openai.ErrorWrapper(err, "invalid_systemone_response", http.StatusBadGateway)
	}
	quota, err := tariff.quota(usage.PromptTokens, usage.CompletionTokens)
	if err != nil {
		c.Set("systemone_no_retry", true)
		return openai.ErrorWrapper(err, "invalid_systemone_quota", http.StatusInternalServerError)
	}
	if err := dbmodel.SettleRelayQuota(meta.UserId, meta.TokenId, meta.ChannelId, reserved, quota); err != nil {
		c.Set("systemone_no_retry", true)
		logger.Error(ctx, "systemone settlement failed: "+err.Error())
		return openai.ErrorWrapper(fmt.Errorf("systemone quota settlement failed"), "systemone_billing_failed", http.StatusInternalServerError)
	}
	settled = true
	recordSystemOneConsumption(ctx, c, meta, usage, tariff, quota, time.Since(start).Seconds())
	c.Header("X-Request-ID", c.GetString("X-Request-ID"))
	c.Data(http.StatusOK, "application/json", responseBody)
	return nil
}

func systemOneError(resp *http.Response) *model.ErrorWithStatusCode { return SystemOneError(resp) }

// SystemOneError 同时供业务 relay 和后台渠道测试解析原生错误。
func SystemOneError(resp *http.Response) *model.ErrorWithStatusCode {
	defer resp.Body.Close()
	status := resp.StatusCode
	if status >= 200 && status < 400 {
		status = http.StatusBadGateway
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return openai.ErrorWrapper(fmt.Errorf("TypeSafe upstream returned HTTP %d", resp.StatusCode), "upstream_error", status)
	}
	// TypeSafe 的参数校验错误使用 detail（字符串或数组）。
	var details struct {
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(body, &details) == nil && len(details.Detail) > 0 && string(details.Detail) != "null" {
		message := string(details.Detail)
		_ = json.Unmarshal(details.Detail, &message)
		if strings.TrimSpace(message) != "" {
			return openai.ErrorWrapper(fmt.Errorf("%s", message), "upstream_error", status)
		}
	}
	copyResponse := *resp
	copyResponse.Body = io.NopCloser(bytes.NewReader(body))
	apiErr := util.RelayErrorHandler(&copyResponse)
	if apiErr.Error.Message == "" {
		apiErr.Error.Message = fmt.Sprintf("TypeSafe upstream returned HTTP %d", resp.StatusCode)
	}
	// 204/其他非协议成功响应应当是网关错误，不能返回一个带 error 的 2xx。
	apiErr.StatusCode = status
	return apiErr
}

// ValidateSystemOneAnswers 让渠道测试和实际转发使用相同的答案完整性检查。
func ValidateSystemOneAnswers(requestBody, responseBody []byte) error {
	var request struct {
		Questions map[string]struct {
			Type string `json:"type"`
		} `json:"questions"`
	}
	var response struct {
		Answers map[string]struct {
			Type   string   `json:"type"`
			Noul   *float64 `json:"noul"`
			Choice *string  `json:"choice"`
			Score  *float64 `json:"score"`
		} `json:"answers"`
	}
	if json.Unmarshal(requestBody, &request) != nil || json.Unmarshal(responseBody, &response) != nil {
		return fmt.Errorf("invalid systemone answers")
	}
	for id, q := range request.Questions {
		a, ok := response.Answers[id]
		if !ok || a.Type != q.Type {
			return fmt.Errorf("systemone answer missing or has incorrect type")
		}
		switch q.Type {
		case "noul":
			if a.Noul == nil || *a.Noul < 0 || *a.Noul > 1 {
				return fmt.Errorf("invalid systemone noul answer")
			}
		case "choice":
			if a.Choice == nil {
				return fmt.Errorf("invalid systemone choice answer")
			}
		case "score":
			if a.Score == nil {
				return fmt.Errorf("invalid systemone score answer")
			}
		default:
			return fmt.Errorf("invalid systemone answer type")
		}
	}
	return nil
}
