package controller

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/songquanpeng/one-api/relay/model"
	"github.com/tidwall/gjson"
)

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
	code, errorType, message := detail.Get("code").String(), detail.Get("type").String(), detail.Get("message").String()
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
	if message == "" {
		message = "Upstream Responses request failed (" + code + ")"
	}
	return &model.ErrorWithStatusCode{
		Error:      model.Error{Code: code, Type: errorType, Message: message},
		StatusCode: statusCode,
	}
}
