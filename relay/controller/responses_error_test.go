package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/relay/util"
	"github.com/stretchr/testify/require"
)

func TestResponsesHTTP200Failures(t *testing.T) {
	oldRedis := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = oldRedis })
	for _, tc := range []struct {
		name, payload, code string
		status              int
	}{
		{"rate_limit", `{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"Rate limit reached"}}}`, "rate_limit_exceeded", 429},
		{"rate_limit_with_generic_type", `{"error":{"type":"invalid_request_error","code":"rate_limit_exceeded","message":"Rate limit reached"}}`, "rate_limit_exceeded", 429},
		{"invalid_key_with_generic_type", `{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"Invalid API key"}}`, "invalid_api_key", 401},
		{"explicit_status", `{"error":{"type":"invalid_request_error","code":"custom_limit","status_code":429,"message":"Rate limit reached"}}`, "custom_limit", 429},
		{"error_event", `{"type":"error","code":"server_error","message":"Upstream unavailable"}`, "server_error", 502},
		{"azure_numeric", `{"error":{"code":"429","message":"Too many requests"}}`, "429", 429},
		{"invalid_history", `{"type":"response.failed","response":{"status":"failed","error":{"code":"invalid_encrypted_content","message":"Cannot decrypt history"}}}`, "invalid_encrypted_content", 400},
		{"unknown", `{"type":"response.failed","response":{"status":"failed"}}`, "responses_failed", 502},
	} {
		for _, stream := range []bool{false, true} {
			for _, started := range []bool{false, true} {
				if !stream && started {
					continue
				}
				t.Run(tc.name+map[bool]string{false: "/json", true: "/stream"}[stream]+map[bool]string{false: "/first", true: "/started"}[started], func(t *testing.T) {
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
					c.Set("channel_id", 14)
					payload := tc.payload
					if stream {
						payload = "data: " + payload + "\n\n"
						if started {
							payload = "data: {\"type\":\"response.in_progress\",\"response\":{}}\n\n" + payload
						}
					}
					resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}
					handler := doNativeOpenaiResponse
					if stream {
						handler = doNativeOpenaiResponseStream
					}
					_, err := handler(c, resp, &util.RelayMeta{DisablePing: true})
					require.NotNil(t, err, "HTTP 200 must not hide a Responses failure")
					require.Equal(t, tc.status, err.StatusCode)
					require.Equal(t, tc.code, err.Error.Code)
					require.False(t, c.GetBool("responses_state_record_failed"))
					if started {
						require.Contains(t, w.Body.String(), tc.payload, "preserve the upstream failure after output starts")
					} else {
						require.False(t, c.Writer.Written(), "allow the outer relay to retry or return the error status")
						require.Empty(t, w.Header().Get("Content-Type"))
					}
				})
			}
		}
	}
}

func TestResponsesStreamRequiresTerminalEvent(t *testing.T) {
	oldRedis := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = oldRedis })
	for _, tc := range []struct {
		name, payload string
		failure       bool
	}{
		{"empty", "", true},
		{"done_only", "data: [DONE]\n\n", true},
		{"truncated", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", true},
		{"completed_zero", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n", false},
		{"incomplete_with_usage", "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":5,\"output_tokens\":3}}}\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			c.Set("channel_id", 14)
			resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.payload))}
			usage, err := doNativeOpenaiResponseStream(c, resp, &util.RelayMeta{DisablePing: true})
			if tc.failure {
				require.NotNil(t, err)
				require.Equal(t, "responses_stream_incomplete", err.Error.Code)
				require.Equal(t, 502, err.StatusCode)
				if tc.name == "truncated" {
					require.Contains(t, w.Body.String(), "responses_stream_incomplete")
				}
			} else {
				require.Nil(t, err)
				if tc.name == "incomplete_with_usage" {
					require.Equal(t, 5, usage.InputTokens)
					require.Equal(t, 3, usage.OutputTokens)
				}
			}
		})
	}
}
