package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/songquanpeng/one-api/relay/model"
	"github.com/tidwall/gjson"
)

// responsesTerminalFailure 判断一个上游事件本身是否已经是客户端能识别的终止失败事件。
// Responses 协议里只有 response.failed 且带嵌套 response.error 才会被官方客户端
// （Codex 的 process_responses_event）当成致命错误；裸 {"type":"error"} 事件除
// flex 不可用之外会被静默丢弃。
func responsesTerminalFailure(raw []byte) bool {
	root := gjson.ParseBytes(raw)
	if root.Get("type").String() != "response.failed" {
		return false
	}
	if root.Get("response.status").String() != "failed" {
		return false
	}
	for _, field := range []string{"code", "message"} {
		value := root.Get("response.error." + field)
		if value.Type != gjson.String || strings.TrimSpace(value.String()) == "" {
			return false
		}
	}
	errorType := root.Get("response.error.type")
	return !errorType.Exists() || errorType.Type == gjson.String
}

// 修正已有 failed 事件，保留上游序号、响应元数据及大整数扩展字段。
// 不能先发送畸形终止事件再追加一个正确事件：客户端可能在第一个事件处停止。
func normalizeResponsesFailureEvent(raw []byte, responseID string, failure *model.ErrorWithStatusCode) string {
	fallback := responsesFailureEvent(responseID, failure)
	var event, response, detail map[string]json.RawMessage
	if json.Unmarshal(raw, &event) != nil || event == nil {
		return fallback
	}
	if json.Unmarshal(event["response"], &response) != nil || response == nil {
		response = make(map[string]json.RawMessage)
	}
	if json.Unmarshal(response["error"], &detail) != nil || detail == nil {
		detail = make(map[string]json.RawMessage)
	}
	normalized := gjson.Parse(fallback)
	for _, field := range []string{"code", "message", "type"} {
		if value := normalized.Get("response.error." + field); value.Exists() {
			detail[field] = json.RawMessage(value.Raw)
		}
	}
	response["error"], _ = json.Marshal(detail)
	response["status"] = json.RawMessage(`"failed"`)
	if _, ok := response["object"]; !ok {
		response["object"] = json.RawMessage(`"response"`)
	}
	if _, ok := response["id"]; !ok && responseID != "" {
		response["id"], _ = json.Marshal(responseID)
	}
	event["response"], _ = json.Marshal(response)
	payload, err := json.Marshal(event)
	if err != nil {
		return fallback
	}
	return string(payload)
}

// responsesFailureEvent 把内部错误转换成符合 Responses 协议的 response.failed 事件。
// 流已经开始输出时不能再改 HTTP 状态码，只能靠这个事件让客户端拿到真实错误，
// 否则客户端只会看到流提前结束（stream closed before response.completed）并反复重试。
func responsesFailureEvent(responseID string, failure *model.ErrorWithStatusCode) string {
	detail := map[string]any{}
	if failure != nil {
		// Codex 侧 error.code 是 Option<String>，非字符串会让整个 error 反序列化失败，
		// 这里统一转成字符串，保证错误码不丢。
		if code := strings.TrimSpace(fmt.Sprintf("%v", failure.Error.Code)); failure.Error.Code != nil && code != "" {
			detail["code"] = code
		}
		if failure.Error.Type != "" {
			detail["type"] = failure.Error.Type
		}
		if strings.TrimSpace(failure.Error.Message) != "" {
			detail["message"] = failure.Error.Message
		}
	}
	if _, ok := detail["code"]; !ok {
		detail["code"] = "responses_failed"
	}
	if _, ok := detail["message"]; !ok {
		detail["message"] = "Upstream Responses request failed"
	}
	response := map[string]any{
		"object": "response",
		"status": "failed",
		"error":  detail,
	}
	if responseID != "" {
		response["id"] = responseID
	}
	payload, err := json.Marshal(map[string]any{"type": "response.failed", "response": response})
	if err != nil {
		return `{"type":"response.failed","response":{"object":"response","status":"failed","error":{"code":"responses_failed","message":"Upstream Responses request failed"}}}`
	}
	return string(payload)
}

// responsesPayloadError checks the application result, including failures carried
// inside HTTP 200 responses. Never infer rate limiting from missing token usage.
func responsesPayloadError(raw []byte) *model.ErrorWithStatusCode {
	root := gjson.ParseBytes(raw)
	response := root
	if nested := root.Get("response"); nested.IsObject() {
		response = nested
	}
	event, status := root.Get("type").String(), response.Get("status").String()
	detail := response.Get("error")
	if !detail.IsObject() {
		detail = root.Get("error")
	}
	failed := event == "error" || event == "response.failed" || event == "response.cancelled" ||
		status == "failed" || status == "cancelled" || status == "expired"
	if !failed && !detail.IsObject() {
		return nil
	}
	if !detail.IsObject() {
		detail = root
	}
	code, errorType, message := strings.TrimSpace(detail.Get("code").String()), detail.Get("type").String(), detail.Get("message").String()
	statusCode := http.StatusBadGateway
	// A specific code takes precedence over a generic error type, e.g.
	// rate_limit_exceeded with type=invalid_request_error is still HTTP 429.
	for _, value := range []string{errorType, code} {
		switch strings.ToLower(value) {
		case "rate_limit_exceeded", "rate_limit_error", "too_many_requests", "insufficient_quota":
			statusCode = http.StatusTooManyRequests
		case "invalid_request_error", "invalid_encrypted_content", "context_length_exceeded":
			statusCode = http.StatusBadRequest
		case "authentication_error", "invalid_api_key":
			statusCode = http.StatusUnauthorized
		case "permission_denied", "permission_error":
			statusCode = http.StatusForbidden
		}
	}
	for _, value := range []gjson.Result{detail.Get("status_code"), detail.Get("status"), detail.Get("code")} {
		if number, err := strconv.Atoi(value.String()); err == nil && number >= 400 && number <= 599 {
			statusCode = number
			break
		}
	}
	if code == "" {
		code = "responses_failed"
	}
	if errorType == "" || errorType == event {
		errorType = "upstream_error"
	}
	if strings.TrimSpace(message) == "" {
		message = "Upstream Responses request failed (" + code + ")"
	}
	return &model.ErrorWithStatusCode{
		Error:      model.Error{Code: code, Type: errorType, Message: message},
		StatusCode: statusCode,
	}
}
