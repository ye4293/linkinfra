package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/relay/util"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 按客户端字符串错误码契约解析实际下游 SSE，不能只验证事件构造函数。
func TestResponsesAuditTerminalErrorIsClientReadable(t *testing.T) {
	oldRedis := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = oldRedis })
	for _, tc := range []struct{ name, event string }{
		{"bare_numeric", `{"type":"error","code":429,"message":"Too many requests"}`},
		{"failed_string", `{"type":"response.failed","response":{"status":"failed","error":{"code":"429","message":"Too many requests"}}}`},
		{"failed_numeric", `{"type":"response.failed","response":{"status":"failed","error":{"code":429,"message":"Too many requests"}}}`},
		{"failed_empty_error", `{"type":"response.failed","response":{"status":"failed","error":{}}}`},
		{"failed_null_error", `{"type":"response.failed","response":{"status":"failed","error":null}}`},
		{"failed_missing_error", `{"type":"response.failed","response":{"status":"failed"}}`},
		{"failed_blank_fields", `{"type":"response.failed","response":{"status":"failed","error":{"code":"  ","message":"  "}}}`},
		{"failed_null_fields", `{"type":"response.failed","response":{"status":"failed","error":{"code":null,"message":null}}}`},
		{"failed_numeric_message", `{"type":"response.failed","response":{"status":"failed","error":{"code":429,"message":123,"type":12}}}`},
		{"failed_metadata", `{"type":"response.failed","sequence_number":7,"future":9007199254740993,"response":{"id":"resp_metadata","model":"gpt-test","status":"failed","error":{"code":429,"message":"Too many requests","future":"keep"},"future":{"n":9007199254740993}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			c.Set("channel_id", 14)
			payload := "data: {\"type\":\"response.in_progress\",\"response\":{}}\n\ndata: " + tc.event + "\n\n"
			upstream := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}
			_, failure := doNativeOpenaiResponseStream(c, upstream, &util.RelayMeta{DisablePing: true})
			require.NotNil(t, failure)
			require.Equal(t, 1, strings.Count(w.Body.String(), "event: response.failed\n"))
			var terminal string
			for _, chunk := range strings.Split(w.Body.String(), "\n\n") {
				if strings.HasPrefix(chunk, "event: response.failed\n") {
					terminal = strings.TrimPrefix(chunk, "event: response.failed\ndata: ")
				}
			}
			require.NotEmpty(t, terminal)
			var client struct {
				Response struct {
					Error struct {
						Code    *string `json:"code"`
						Message *string `json:"message"`
					} `json:"error"`
				} `json:"response"`
			}
			require.NoError(t, json.Unmarshal([]byte(terminal), &client), terminal)
			require.NotNil(t, client.Response.Error.Code, terminal)
			require.NotNil(t, client.Response.Error.Message, terminal)
			require.NotEmpty(t, *client.Response.Error.Message, terminal)
			require.NotEmpty(t, strings.TrimSpace(*client.Response.Error.Code), terminal)
			require.NotEmpty(t, strings.TrimSpace(*client.Response.Error.Message), terminal)
			if tc.name == "failed_string" {
				require.Equal(t, tc.event, terminal, "valid upstream event must remain byte-for-byte unchanged")
			}
			if tc.name == "failed_metadata" {
				for _, field := range []string{"sequence_number", "future", "response.id", "response.model", "response.future", "response.error.future"} {
					require.Equal(t, gjson.Get(tc.event, field).Raw, gjson.Get(terminal, field).Raw, field)
				}
			}
		})
	}
}
