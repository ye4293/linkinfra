package minimax

import (
	"net/http"
	"net/http/httptest"
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
