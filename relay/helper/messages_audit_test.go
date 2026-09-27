package helper_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/helper"
	"github.com/songquanpeng/one-api/relay/util"
)

// 从生产工厂到真实 HTTP 发送，覆盖嵌入方法分派、SDK 基址及协议头。
func TestMessagesChannelDispatchMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	old := util.HTTPClient
	util.HTTPClient = &http.Client{}
	t.Cleanup(func() { util.HTTPClient = old })
	cases := []struct {
		name        string
		channelType int
		prefix      string
		bases       []string
		header      string
	}{
		{"deepseek", common.ChannelTypeDeepseek, "/anthropic", []string{"", "/v1/", "/anthropic", "/anthropic/v1/"}, "Authorization"},
		{"baidu", common.ChannelTypeBaidu, "/anthropic", []string{"", "/v2/", "/anthropic", "/anthropic/v1/"}, "X-Api-Key"},
		{"mimo", common.ChannelTypeMimo, "/anthropic", []string{"", "/v1/", "/anthropic", "/anthropic/v1/"}, "Authorization"},
		{"moonshot", common.ChannelTypeMoonshot, "/anthropic", []string{"", "/v1/", "/anthropic", "/anthropic/v1/"}, "Authorization"},
		{"minimax", common.ChannelTypeMinimax, "/anthropic", []string{"", "/v1/", "/anthropic", "/anthropic/v1/"}, "Authorization"},
		{"xai", common.ChannelTypeXAI, "", []string{"", "/v1/"}, "Authorization"},
		{"zhipu", common.ChannelTypeZhipu, "/api/anthropic", []string{"", "/api/anthropic", "/api/anthropic/v1/"}, "Authorization"},
		{"ali", common.ChannelTypeAli, "/apps/anthropic", []string{"", "/apps/anthropic", "/apps/anthropic/v1/"}, "Authorization"},
		{"anthropic", common.ChannelTypeAnthropic, "", []string{"", "/", "/v1/"}, "X-Api-Key"},
	}
	for _, tc := range cases {
		for _, base := range tc.bases {
			for _, stream := range []bool{false, true} {
				for _, fallback := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/stream=%v/fallback=%v", tc.name, base, stream, fallback), func(t *testing.T) {
						body := fmt.Sprintf(`{"model":"claude-test","max_tokens":128,"stream":%v,"system":[{"type":"text","text":"system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"sig"},{"type":"tool_use","id":"t1","name":"read","input":{"file":"a"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"OK"}]}],"tools":[{"name":"read","input_schema":{"type":"object","properties":{"file":{"type":"string"}},"required":["file"]}}],"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"future_field":9007199254740993}`, stream)
						response := `{"type":"message","content":[{"type":"text","text":"OK"}]}`
						if stream {
							response = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
						}
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							want := "/proxy" + tc.prefix + "/v1/messages?beta=true"
							if r.URL.RequestURI() != want {
								t.Errorf("path %q, want %q", r.URL.RequestURI(), want)
							}
							key := "channel-key"
							if fallback {
								key = "fallback-key"
							}
							if tc.header == "Authorization" {
								key = "Bearer " + key
							}
							if r.Header.Get(tc.header) != key {
								t.Error("wrong credential")
							}
							other := "X-Api-Key"
							if tc.header == other {
								other = "Authorization"
							}
							if r.Header.Get(other) != "" {
								t.Error("client credential leaked")
							}
							if r.Header.Get("anthropic-version") != "custom-version" || r.Header.Get("anthropic-beta") != "test-beta" {
								t.Error("missing protocol headers")
							}
							if r.Header.Get("Content-Type") != "application/json" {
								t.Error("missing content type")
							}
							if stream && r.Header.Get("Accept") != "text/event-stream" {
								t.Error("missing SSE accept")
							}
							got, err := io.ReadAll(r.Body)
							if err != nil || string(got) != body {
								t.Error("body changed")
							}
							w.Header().Set("Content-Type", "application/json")
							if stream {
								w.Header().Set("Content-Type", "text/event-stream")
							}
							io.WriteString(w, response)
						}))
						defer server.Close()
						c, _ := gin.CreateTestContext(httptest.NewRecorder())
						c.Request = httptest.NewRequest("POST", "/v1/messages?beta=true", strings.NewReader(body))
						c.Request.Header.Set("Content-Type", "application/json")
						c.Request.Header.Set("anthropic-version", "custom-version")
						c.Request.Header.Set("anthropic-beta", "test-beta")
						c.Request.Header.Set("X-Api-Key", "client-key")
						meta := &util.RelayMeta{ChannelType: tc.channelType, BaseURL: server.URL + "/proxy" + base, RequestURLPath: "/v1/messages?beta=true", Mode: constant.RelayModeClaude, IsStream: stream, DisablePing: true, APIKey: "channel-key", ActualAPIKey: "channel-key", ActualModelName: "claude-test"}
						if fallback {
							meta.ActualAPIKey = ""
							meta.APIKey = "fallback-key"
						}
						a := helper.GetAdaptor(constant.ChannelType2APIType(tc.channelType))
						a.Init(meta)
						resp, err := a.DoRequest(c, meta, strings.NewReader(body))
						if err != nil {
							t.Fatal(err)
						}
						defer resp.Body.Close()
						got, err := io.ReadAll(resp.Body)
						if err != nil || string(got) != response {
							t.Error("response changed")
						}
					})
				}
			}
		}
	}
}

// Anthropic 的 OpenAI 转换入口不能把 /v1/chat/completions 发给上游。
func TestAnthropicConvertedChatKeepsMessagesEndpoint(t *testing.T) {
	a := helper.GetAdaptor(constant.APITypeAnthropic)
	for _, base := range []string{"https://api.anthropic.com", "https://api.anthropic.com/", "https://api.anthropic.com/v1/"} {
		got, err := a.GetRequestURL(&util.RelayMeta{BaseURL: base, Mode: constant.RelayModeChatCompletions, RequestURLPath: "/v1/chat/completions"})
		if err != nil || got != "https://api.anthropic.com/v1/messages" {
			t.Fatalf("got %s err %v", got, err)
		}
	}
}
