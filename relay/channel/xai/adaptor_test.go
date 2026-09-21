package xai

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
