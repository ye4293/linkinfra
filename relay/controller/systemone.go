package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
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
	modelRatio, groupRatio := common.GetModelRatio(meta.BillingModelName()), meta.CombinedGroupRatio()
	ratio := modelRatio * groupRatio
	billingRequest := &model.GeneralOpenAIRequest{Model: actual}
	// 字节数只用于预估额度；最终只采用 TypeSafe 返回的实际 token 用量。
	estimate := (float64(config.PreConsumedQuota) + float64(len(body))) * ratio
	if price := common.GetModelPrice(meta.BillingModelName(), false); price != -1 {
		estimate = price * 500000 * groupRatio
	}
	if math.IsNaN(estimate) || math.IsInf(estimate, 0) || estimate < 0 || estimate >= float64(math.MaxInt64) {
		return openai.ErrorWrapper(fmt.Errorf("invalid systemone price"), "invalid_systemone_price", http.StatusInternalServerError)
	}
	reserved := int64(math.Ceil(estimate))
	// 始终检查令牌和用户两种额度，避免用户余额充足时绕过令牌额度。
	if err := dbmodel.PreConsumeTokenQuota(meta.TokenId, reserved); err != nil {
		return openai.ErrorWrapper(err, "insufficient_systemone_quota", http.StatusForbidden)
	}
	_ = dbmodel.CacheUpdateUserQuota2(meta.UserId)
	settled := false
	defer func() {
		if !settled {
			if reserved != 0 {
				if err := dbmodel.PostConsumeTokenQuota(meta.TokenId, -reserved); err != nil {
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
	// 成功返回前执行结算；数据库是否批量更新仍遵循全局配置。
	postConsumeQuota(ctx, c, usage, meta, billingRequest, ratio, reserved, modelRatio, groupRatio,
		time.Since(start).Seconds(), c.GetHeader("X-Title"), c.GetHeader("HTTP-Referer"), 0)
	settled = true
	c.Header("X-Request-ID", c.GetString("X-Request-ID"))
	c.Data(http.StatusOK, "application/json", responseBody)
	return nil
}

func systemOneError(resp *http.Response) *model.ErrorWithStatusCode {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return openai.ErrorWrapper(fmt.Errorf("TypeSafe upstream returned HTTP %d", resp.StatusCode), "upstream_error", resp.StatusCode)
	}
	// TypeSafe 的参数校验错误使用 detail（字符串或数组）。
	var details struct {
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(body, &details) == nil && len(details.Detail) > 0 && string(details.Detail) != "null" {
		message := string(details.Detail)
		_ = json.Unmarshal(details.Detail, &message)
		return openai.ErrorWrapper(fmt.Errorf("%s", message), "upstream_error", resp.StatusCode)
	}
	copyResponse := *resp
	copyResponse.Body = io.NopCloser(bytes.NewReader(body))
	return util.RelayErrorHandler(&copyResponse)
}
