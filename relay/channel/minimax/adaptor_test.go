package minimax

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/util"
)

// Claude 原生请求走 minimax anthropic 兼容端点（base 填 api.minimaxi.com）。
func TestGetRequestURL_ClaudeMode(t *testing.T) {
	a := &Adaptor{}
	meta := &util.RelayMeta{
		Mode:    constant.RelayModeClaude,
		BaseURL: "https://api.minimaxi.com",
	}
	url, err := a.GetRequestURL(meta)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := "https://api.minimaxi.com/anthropic/v1/messages"
	if url != want {
		t.Errorf("Claude URL = %q, want %q", url, want)
	}
}

// Claude 分支：Bearer 渠道 key + anthropic-version 默认 + beta 透传。
func TestSetupRequestHeader_ClaudeMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	c.Request = r

	req, _ := http.NewRequest("POST", "https://api.minimaxi.com/anthropic/v1/messages", nil)
	a := &Adaptor{}
	meta := &util.RelayMeta{
		Mode:         constant.RelayModeClaude,
		ActualAPIKey: "test-minimax-key",
	}
	if err := a.SetupRequestHeader(c, req, meta); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got, want := req.Header.Get("Authorization"), "Bearer test-minimax-key"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version default = %q, want 2023-06-01", got)
	}
	if got := req.Header.Get("anthropic-beta"); got != "interleaved-thinking-2025-05-14" {
		t.Errorf("anthropic-beta not forwarded, got %q", got)
	}
}

// 验证真实发送，避免内嵌 OpenAI 接收者绕过 MiniMax URL/请求头覆盖。
func TestDoRequestUsesMiniMaxAdaptor(t *testing.T) {
	old := util.HTTPClient
	util.HTTPClient = &http.Client{}
	t.Cleanup(func() { util.HTTPClient = old })
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.RequestURI() != "/anthropic/v1/messages?beta=true" {
					t.Errorf("wrong upstream path: %s", r.URL.RequestURI())
				}
				if r.Header.Get("Authorization") != "Bearer channel-key" {
					t.Error("wrong credential")
				}
				if r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("anthropic-beta") != "test-beta" {
					t.Error("missing Anthropic headers")
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != `{"model":"MiniMax-M3","messages":[{"role":"user","content":"OK"}],"max_tokens":128}` {
					t.Error("body changed")
				}
				w.WriteHeader(200)
			}))
			defer server.Close()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/messages?beta=true", nil)
			c.Request.Header.Set("anthropic-beta", "test-beta")
			meta := &util.RelayMeta{Mode: constant.RelayModeClaude, BaseURL: server.URL, RequestURLPath: "/v1/messages?beta=true", APIKey: "client-key", ActualAPIKey: "channel-key", IsStream: stream, DisablePing: true}
			resp, err := (&Adaptor{}).DoRequest(c, meta, strings.NewReader(`{"model":"MiniMax-M3","messages":[{"role":"user","content":"OK"}],"max_tokens":128}`))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		})
	}
}

func TestProtocolBaseURLs(t *testing.T) {
	for _, base := range []string{"", "https://api.minimaxi.com", "https://api.minimaxi.com/v1/", "https://api.minimaxi.com/anthropic", "https://api.minimaxi.com/anthropic/v1/"} {
		for _, path := range []string{"/v1/messages?beta=true", "/v1/chat/completions", "/v1/responses"} {
			meta := &util.RelayMeta{BaseURL: base, RequestURLPath: path, Mode: constant.Path2RelayMode(path)}
			got, err := (&Adaptor{}).GetRequestURL(meta)
			prefix := "https://api.minimaxi.com"
			if meta.Mode == constant.RelayModeClaude {
				prefix += "/anthropic"
			}
			if err != nil || got != prefix+path {
				t.Errorf("base %s path %s got %s, err %v", base, path, got, err)
			}
			if meta.BaseURL != base {
				t.Error("mutated shared metadata")
			}
		}
	}
}
