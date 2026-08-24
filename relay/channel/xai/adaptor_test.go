package xai

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (&Adaptor{}).GetRequestURL(&util.RelayMeta{
				Mode: tt.mode, BaseURL: "https://api.x.ai/", RequestURLPath: tt.path,
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
