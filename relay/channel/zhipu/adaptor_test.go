package zhipu

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/util"
)

// Claude 原生请求走智谱 anthropic 兼容端点，且 BaseURL 尾部斜杠被裁掉。
func TestGetRequestURL_ClaudeMode(t *testing.T) {
	a := &Adaptor{}
	meta := &util.RelayMeta{
		Mode:    constant.RelayModeClaude,
		BaseURL: "https://open.bigmodel.cn/",
	}
	url, err := a.GetRequestURL(meta)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := "https://open.bigmodel.cn/api/anthropic/v1/messages"
	if url != want {
		t.Errorf("Claude URL = %q, want %q", url, want)
	}
}

// 回归：OpenAI 协议（chat/completions）+ glm-4 仍走 v4 原生端点。
func TestGetRequestURL_OpenAIV4Mode(t *testing.T) {
	a := &Adaptor{}
	meta := &util.RelayMeta{
		Mode:            constant.RelayModeChatCompletions,
		BaseURL:         "https://open.bigmodel.cn",
		ActualModelName: "glm-4",
	}
	url, err := a.GetRequestURL(meta)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := "https://open.bigmodel.cn/api/paas/v4/chat/completions"
	if url != want {
		t.Errorf("OpenAI v4 URL = %q, want %q", url, want)
	}
}

// Claude 分支：Bearer 渠道 key + anthropic-version 默认值 + beta 透传。
func TestSetupRequestHeader_ClaudeMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	c.Request = r

	req, _ := http.NewRequest("POST", "https://open.bigmodel.cn/api/anthropic/v1/messages", nil)
	a := &Adaptor{}
	meta := &util.RelayMeta{
		Mode:         constant.RelayModeClaude,
		ActualAPIKey: "test-zhipu-key",
	}
	if err := a.SetupRequestHeader(c, req, meta); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got, want := req.Header.Get("Authorization"), "Bearer test-zhipu-key"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version default = %q, want 2023-06-01", got)
	}
	if got := req.Header.Get("anthropic-beta"); got != "interleaved-thinking-2025-05-14" {
		t.Errorf("anthropic-beta not forwarded, got %q", got)
	}
}
