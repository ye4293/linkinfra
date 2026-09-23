package xai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/util"
)

func TestGetRequestURLProtocolModes(t *testing.T) {
	tests := []struct {
		name string
		mode int
		path string
		want string
	}{
		{name: "claude", mode: constant.RelayModeClaude, want: "https://api.x.ai/v1/messages"},
		{name: "responses", mode: constant.RelayModeOpenaiResponse, want: "https://api.x.ai/v1/responses"},
		{name: "responses retrieval", mode: constant.RelayModeOpenaiResponse, path: "/v1/responses/response-123?include=output", want: "https://api.x.ai/v1/responses/response-123?include=output"},
		{name: "chat", mode: constant.RelayModeChatCompletions, path: "/v1/chat/completions", want: "https://api.x.ai/v1/chat/completions"},
		{name: "chat default", mode: constant.RelayModeChatCompletions, want: "https://api.x.ai/v1/chat/completions"},
		{name: "messages query", mode: constant.RelayModeClaude, path: "/v1/messages?beta=true", want: "https://api.x.ai/v1/messages?beta=true"},
		{name: "responses compact", mode: constant.RelayModeOpenaiResponse, path: "/v1/responses/compact", want: "https://api.x.ai/v1/responses/compact"},
		{name: "images", mode: constant.RelayModeImagesGenerations, path: "/v1/images/generations", want: "https://api.x.ai/v1/images/generations"},
	}

	for _, tt := range tests {
		for _, base := range []string{"https://api.x.ai", "https://api.x.ai/", "https://api.x.ai/v1", "https://api.x.ai/v1/"} {
			t.Run(tt.name+"/"+base, func(t *testing.T) {
				got, err := (&Adaptor{}).GetRequestURL(&util.RelayMeta{
					Mode: tt.mode, BaseURL: base, RequestURLPath: tt.path, ChannelType: common.ChannelTypeXAI,
				})
				if err != nil {
					t.Fatalf("GetRequestURL() error = %v", err)
				}
				if got != tt.want {
					t.Fatalf("GetRequestURL() = %q, want %q", got, tt.want)
				}
			})
		}
	}
}

func TestGetRequestURLProtocolDefaultWithoutRequestPath(t *testing.T) {
	tests := []struct {
		mode int
		want string
	}{
		{mode: constant.RelayModeClaude, want: "https://api.x.ai/v1/messages"},
		{mode: constant.RelayModeOpenaiResponse, want: "https://api.x.ai/v1/responses"},
	}
	for _, tt := range tests {
		got, err := (&Adaptor{}).GetRequestURL(&util.RelayMeta{Mode: tt.mode, BaseURL: "https://api.x.ai/"})
		if err != nil {
			t.Fatalf("GetRequestURL() error = %v", err)
		}
		if got != tt.want {
			t.Fatalf("GetRequestURL() = %q, want %q", got, tt.want)
		}
	}
}

func TestSetupRequestHeaderClaude(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("anthropic-beta", "test-beta")

	req := httptest.NewRequest(http.MethodPost, "https://api.x.ai/v1/messages", nil)
	meta := &util.RelayMeta{Mode: constant.RelayModeClaude, APIKey: "test-xai-key"}
	if err := (&Adaptor{}).SetupRequestHeader(c, req, meta); err != nil {
		t.Fatalf("SetupRequestHeader() error = %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer test-xai-key" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got)
	}
	if got := req.Header.Get("anthropic-beta"); got != "test-beta" {
		t.Errorf("anthropic-beta = %q", got)
	}
}

// 从客户端路径识别协议，通过真实 HTTP 请求验证路径、渠道密钥及原生请求体透传。
func TestDoRequestProtocolDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousClient := util.HTTPClient
	util.HTTPClient = &http.Client{}
	t.Cleanup(func() { util.HTTPClient = previousClient })

	for _, path := range []string{"/v1/chat/completions", "/v1/messages?beta=true", "/v1/responses", "/v1/responses/compact"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", path, stream), func(t *testing.T) {
				content := `"messages":[{"role":"user","content":"hello"}]`
				if strings.HasPrefix(path, "/v1/responses") {
					content = `"input":"hello","store":false,"previous_response_id":"resp-123"`
				}
				body := fmt.Sprintf(`{"model":"grok-4","max_tokens":32,"stream":%v,%s}`, stream, content)
				responseBody := `{"id":"test-response"}`
				contentType := "application/json"
				if stream {
					responseBody = "event: test\ndata: {\"id\":\"test-response\"}\n\n"
					contentType = "text/event-stream"
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.RequestURI() != path {
						t.Errorf("upstream request = %s %s, want POST %s", r.Method, r.URL.RequestURI(), path)
					}
					if got := r.Header.Get("Authorization"); got != "Bearer selected-channel-key" {
						t.Errorf("Authorization = %q", got)
					}
					if got := r.Header.Get("Content-Type"); got != "application/json" {
						t.Errorf("Content-Type = %q", got)
					}
					if strings.HasPrefix(path, "/v1/messages") {
						if r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("anthropic-beta") != "test-beta" {
							t.Error("Anthropic headers were not forwarded")
						}
					}
					if stream && r.Header.Get("Accept") != "text/event-stream" {
						t.Errorf("Accept = %q", r.Header.Get("Accept"))
					}
					got, err := io.ReadAll(r.Body)
					if err != nil || string(got) != body {
						t.Errorf("request body changed: %s, err = %v", got, err)
					}
					w.Header().Set("Content-Type", contentType)
					_, _ = io.WriteString(w, responseBody)
				}))
				defer server.Close()

				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Request.Header.Set("anthropic-version", "2023-06-01")
				c.Request.Header.Set("anthropic-beta", "test-beta")
				meta := &util.RelayMeta{
					Mode: constant.Path2RelayMode(c.Request.URL.Path), ChannelType: common.ChannelTypeXAI,
					BaseURL: server.URL + "/v1/", RequestURLPath: c.Request.URL.String(),
					APIKey: "fallback-key", ActualAPIKey: "selected-channel-key", IsStream: stream, DisablePing: true,
				}
				a := &Adaptor{}
				a.Init(meta)
				resp, err := a.DoRequest(c, meta, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				got, err := io.ReadAll(resp.Body)
				if err != nil || string(got) != responseBody || resp.Header.Get("Content-Type") != contentType {
					t.Fatalf("response changed: %s, err = %v", got, err)
				}
			})
		}
	}
}

func TestClaudeSystemMessageCompatibility(t *testing.T) {
	old := util.HTTPClient
	util.HTTPClient = &http.Client{}
	t.Cleanup(func() { util.HTTPClient = old })
	for _, mode := range []int{constant.RelayModeClaude, constant.RelayModeChatCompletions} {
		for _, system := range []string{`"base"`, `[{"type":"text","text":"base","cache_control":{"type":"ephemeral"}}]`} {
			t.Run(fmt.Sprintf("%d/%s", mode, system), func(t *testing.T) {
				body := `{"model":"grok-4.7","system":` + system + `,"messages":[{"role":"user","content":"hello"},{"role":"system","content":"environment"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"test","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"OK"}]},{"role":"system","content":[{"type":"text","text":"update","cache_control":{"type":"ephemeral"}}]}],"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"unknown":{"preserve":true}}`
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					got, _ := io.ReadAll(r.Body)
					if mode != constant.RelayModeClaude {
						if string(got) != body {
							t.Error("changed OpenAI request")
						}
						return
					}
					var p struct {
						System       []map[string]any
						Messages     []map[string]any
						Thinking     map[string]any
						OutputConfig map[string]any `json:"output_config"`
						Unknown      map[string]any
					}
					if err := json.Unmarshal(got, &p); err != nil {
						t.Fatal(err)
					}
					if len(p.System) != 3 || p.System[0]["text"] != "base" || p.System[1]["text"] != "environment" || p.System[2]["text"] != "update" || p.System[2]["cache_control"] == nil {
						t.Errorf("system lost: %s", got)
					}
					if len(p.Messages) != 3 || p.Messages[1]["role"] != "assistant" || p.Messages[2]["content"] == nil {
						t.Errorf("messages lost: %s", got)
					}
					if p.Thinking["type"] != "adaptive" || p.OutputConfig["effort"] != "max" || p.Unknown["preserve"] != true {
						t.Error("unknown fields lost")
					}
				}))
				defer server.Close()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
				resp, err := (&Adaptor{}).DoRequest(c, &util.RelayMeta{Mode: mode, BaseURL: server.URL, APIKey: "test-key"}, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
			})
		}
	}
	original := []byte(`{"messages":[{"role":"user","content":"OK"}],"unknown":true}`)
	got, err := normalizeSystemMessages(original)
	if err != nil || string(got) != string(original) {
		t.Fatal("ordinary request changed")
	}
}

func TestSystemMessagesWithoutTopLevelSystem(t *testing.T) {
	for _, top := range []string{"", `,"system":null`} {
		got, err := normalizeSystemMessages([]byte(`{"messages":[{"role":"user","content":"hello"},{"role":"system","content":"environment"}]` + top + `}`))
		if err != nil {
			t.Fatal(err)
		}
		var p struct {
			System   []map[string]any
			Messages []map[string]any
		}
		if err = json.Unmarshal(got, &p); err != nil || len(p.System) != 1 || len(p.Messages) != 1 {
			t.Fatalf("got %s, err %v", got, err)
		}
	}
	if _, err := normalizeSystemMessages([]byte(`{"messages":[{"role":"system","content":42}]}`)); err == nil {
		t.Fatal("invalid system content silently accepted")
	}
}
