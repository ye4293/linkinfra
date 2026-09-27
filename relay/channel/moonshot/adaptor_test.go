package moonshot

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

// Claude 原生请求走 moonshot anthropic 兼容端点（base 填 api.moonshot.cn，同 base 兼顾 OpenAI）。
func TestGetRequestURL_ClaudeMode(t *testing.T) {
	a := &Adaptor{}
	meta := &util.RelayMeta{
		Mode:    constant.RelayModeClaude,
		BaseURL: "https://api.moonshot.cn",
	}
	url, err := a.GetRequestURL(meta)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := "https://api.moonshot.cn/anthropic/v1/messages"
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

	req, _ := http.NewRequest("POST", "https://api.moonshot.cn/anthropic/v1/messages", nil)
	a := &Adaptor{}
	meta := &util.RelayMeta{
		Mode:         constant.RelayModeClaude,
		ActualAPIKey: "test-moonshot-key",
	}
	if err := a.SetupRequestHeader(c, req, meta); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got, want := req.Header.Get("Authorization"), "Bearer test-moonshot-key"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version default = %q, want 2023-06-01", got)
	}
	if got := req.Header.Get("anthropic-beta"); got != "interleaved-thinking-2025-05-14" {
		t.Errorf("anthropic-beta not forwarded, got %q", got)
	}
}

// 验证实际发送路径，避免只测 GetRequestURL 而漏掉内嵌接收者分派。
func TestDoRequestUsesMoonshotAdaptor(t *testing.T) {
	old := util.HTTPClient
	util.HTTPClient = &http.Client{}
	t.Cleanup(func() { util.HTTPClient = old })
	for _, suffix := range []string{"", "/v1/", "/anthropic", "/anthropic/v1/"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", suffix, stream), func(t *testing.T) {
				body := fmt.Sprintf(`{"model":"kimi-k3","messages":[{"role":"user","content":"OK"}],"max_tokens":128,"stream":%v}`, stream)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.RequestURI() != "/proxy/anthropic/v1/messages?beta=true" {
						t.Errorf("wrong URL: %s", r.URL)
					}
					if r.Header.Get("Authorization") != "Bearer channel-key" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("anthropic-beta") != "test-beta" {
						t.Error("wrong headers")
					}
					got, _ := io.ReadAll(r.Body)
					if string(got) != body {
						t.Error("body changed")
					}
					w.WriteHeader(200)
				}))
				defer server.Close()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("POST", "/v1/messages?beta=true", strings.NewReader(body))
				c.Request.Header.Set("anthropic-beta", "test-beta")
				a := &Adaptor{}
				meta := &util.RelayMeta{Mode: constant.RelayModeClaude, BaseURL: server.URL + "/proxy" + suffix, RequestURLPath: "/v1/messages?beta=true", ActualAPIKey: "channel-key", APIKey: "client-key", IsStream: stream, DisablePing: true}
				resp, err := a.DoRequest(c, meta, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
			})
		}
	}
}

func TestOpenAIPathsRemainUnchanged(t *testing.T) {
	for _, base := range []string{"https://api.moonshot.cn", "https://api.moonshot.cn/v1/", "https://api.moonshot.cn/anthropic/v1/"} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses?test=true"} {
			meta := &util.RelayMeta{BaseURL: base, RequestURLPath: path, Mode: constant.Path2RelayMode(path)}
			got, err := (&Adaptor{}).GetRequestURL(meta)
			if err != nil || got != "https://api.moonshot.cn"+path {
				t.Fatalf("got %q, err %v", got, err)
			}
			if meta.BaseURL != base {
				t.Fatal("changed shared metadata")
			}
		}
	}
}
