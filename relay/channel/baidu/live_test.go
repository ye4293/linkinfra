package baidu_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/helper"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

// Opt-in integration test: the key is supplied only through the environment.
func TestQianfanV2Live(t *testing.T) {
	key := os.Getenv("QIANFAN_TEST_API_KEY")
	if key == "" {
		t.Skip("QIANFAN_TEST_API_KEY is not set")
	}
	gin.SetMode(gin.TestMode)
	// Honor the developer machine's proxy for live tests and bound network waits.
	oldClient := util.HTTPClient
	transport := http.DefaultTransport.(*http.Transport).Clone()
	util.HTTPClient = &http.Client{Transport: transport, Timeout: 30 * time.Second}
	t.Cleanup(func() {
		util.HTTPClient = oldClient
		transport.CloseIdleConnections()
	})
	for _, tc := range []struct {
		name, path, body, terminal string
	}{
		{"chat", "/v1/chat/completions", `{"model":"ernie-4.5-turbo-32k","max_tokens":128,"messages":[{"role":"user","content":"Reply with only OK."}]}`, "[DONE]"},
		{"responses", "/v1/responses", `{"model":"deepseek-v3.2","max_output_tokens":128,"input":"Reply with only OK.","store":false}`, "response.completed"},
		{"messages", "/v1/messages", `{"model":"ernie-4.5-turbo-32k","max_tokens":128,"messages":[{"role":"user","content":"Reply with only OK."}]}`, "message_stop"},
	} {
		for _, stream := range []bool{false, true} {
			name := tc.name + "/json"
			if stream {
				name = tc.name + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				body := strings.TrimSuffix(tc.body, "}")
				if stream {
					body += `,"stream":true`
				}
				body += "}"
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				meta := &util.RelayMeta{
					ChannelType:    common.ChannelTypeBaidu,
					RequestURLPath: tc.path,
					Mode:           constant.Path2RelayMode(tc.path),
					ActualAPIKey:   key,
					IsStream:       stream,
					DisablePing:    true,
				}
				a := helper.GetAdaptor(constant.ChannelType2APIType(meta.ChannelType))
				a.Init(meta)
				if tc.name == "chat" {
					var request model.GeneralOpenAIRequest
					if err := json.Unmarshal([]byte(body), &request); err != nil {
						t.Fatal(err)
					}
					converted, err := a.ConvertRequest(c, meta.Mode, &request)
					if err != nil {
						t.Fatal(err)
					}
					data, err := json.Marshal(converted)
					if err != nil {
						t.Fatal(err)
					}
					body = string(data)
				}
				resp, err := a.DoRequest(c, meta, strings.NewReader(body))
				if err != nil {
					t.Fatal(strings.ReplaceAll(err.Error(), key, "[REDACTED]"))
				}
				defer resp.Body.Close()
				data, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
				if err != nil {
					t.Fatal(err)
				}
				safeBody := strings.ReplaceAll(string(data), key, "[REDACTED]")
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("HTTP %d: %s", resp.StatusCode, safeBody)
				}
				if stream {
					if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") || !strings.Contains(safeBody, tc.terminal) {
						t.Fatalf("invalid/incomplete event stream: %s", safeBody)
					}
					if !strings.Contains(safeBody, "OK") {
						t.Fatalf("missing expected output: %s", safeBody)
					}
				} else {
					var result map[string]any
					if err := json.Unmarshal(data, &result); err != nil {
						t.Fatal(err)
					}
					if result["error"] != nil || result["usage"] == nil || !strings.Contains(safeBody, "OK") {
						t.Fatalf("invalid response or missing usage/output: %s", safeBody)
					}
					t.Logf("usage: %v", result["usage"])
				}
				t.Logf("HTTP %d, %s, completed with OK", resp.StatusCode, resp.Header.Get("Content-Type"))
			})
		}
	}
}
