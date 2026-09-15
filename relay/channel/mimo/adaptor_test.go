package mimo_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/helper"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

func TestMimoProtocolURLs(t *testing.T) {
	bases := []string{"", "https://api.xiaomimimo.com", "https://api.xiaomimimo.com/", "https://api.xiaomimimo.com/v1/", "https://api.xiaomimimo.com/anthropic", "https://api.xiaomimimo.com/anthropic/v1/"}
	for _, base := range bases {
		for _, tc := range []struct {
			path string
			want string
		}{
			{"/v1/chat/completions", "/v1/chat/completions"},
			{"/v1/responses", "/v1/responses"},
			{"/v1/messages?beta=true", "/anthropic/v1/messages?beta=true"},
		} {
			t.Run(base+tc.path, func(t *testing.T) {
				meta := &util.RelayMeta{ChannelType: common.ChannelTypeMimo, BaseURL: base, RequestURLPath: tc.path, Mode: constant.Path2RelayMode(tc.path)}
				a := helper.GetAdaptor(constant.ChannelType2APIType(meta.ChannelType))
				a.Init(meta)
				got, err := a.GetRequestURL(meta)
				if err != nil || got != "https://api.xiaomimimo.com"+tc.want {
					t.Fatalf("URL = %q, err = %v; want %q", got, err, "https://api.xiaomimimo.com"+tc.want)
				}
			})
		}
	}
}

// Exercise the production factory and DoRequest, not just URL/header methods:
// embedded adaptors can accidentally dispatch using the embedded receiver.
func TestMimoDoRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oldClient := util.HTTPClient
	util.HTTPClient = &http.Client{}
	t.Cleanup(func() { util.HTTPClient = oldClient })
	for _, tc := range []struct {
		path     string
		upstream string
		body     string
	}{
		{"/v1/chat/completions", "/v1/chat/completions", `{"model":"mimo-v2.5-pro","messages":[{"role":"user","content":"OK"}]}`},
		{"/v1/responses", "/v1/responses", `{"model":"mimo-v2.5-pro","input":"OK","tools":[{"type":"function","name":"test"}]}`},
		{"/v1/messages?beta=true", "/anthropic/v1/messages?beta=true", `{"model":"mimo-v2.5-pro","max_tokens":8,"messages":[{"role":"user","content":"OK"}]}`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(tc.path+map[bool]string{false: "/json", true: "/stream"}[stream], func(t *testing.T) {
				body := tc.body
				response := `{"ok":true}`
				contentType := "application/json"
				if stream {
					body = strings.TrimSuffix(body, "}") + `,"stream":true}`
					contentType = "text/event-stream"
					response = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.RequestURI() != "/proxy"+tc.upstream {
						t.Errorf("upstream request = %s %s", r.Method, r.URL.RequestURI())
					}
					if r.Header.Get("Authorization") != "Bearer channel-key" || r.Header.Get("X-Api-Key") != "" {
						t.Error("upstream must use the channel credential")
					}
					if strings.Contains(tc.path, "/messages") {
						if r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("anthropic-beta") != "test-beta" {
							t.Error("missing Anthropic protocol headers")
						}
					} else if r.Header.Get("anthropic-version") != "" {
						t.Error("Anthropic headers leaked to OpenAI protocol")
					}
					if stream && r.Header.Get("Accept") != "text/event-stream" {
						t.Error("missing SSE Accept header")
					}
					got, err := io.ReadAll(r.Body)
					if err != nil || string(got) != body {
						t.Errorf("request body changed: %q, err = %v", got, err)
					}
					w.Header().Set("Content-Type", contentType)
					_, _ = io.WriteString(w, response)
				}))
				defer server.Close()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Request.Header.Set("X-Api-Key", "client-key")
				c.Request.Header.Set("anthropic-beta", "test-beta")
				meta := &util.RelayMeta{ChannelType: common.ChannelTypeMimo, BaseURL: server.URL + "/proxy/anthropic/v1/", RequestURLPath: tc.path, Mode: constant.Path2RelayMode(tc.path), APIKey: "client-key", ActualAPIKey: "channel-key", IsStream: stream, DisablePing: true}
				a := helper.GetAdaptor(constant.ChannelType2APIType(meta.ChannelType))
				a.Init(meta)
				resp, err := a.DoRequest(c, meta, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				got, err := io.ReadAll(resp.Body)
				if err != nil || string(got) != response || resp.Header.Get("Content-Type") != contentType {
					t.Fatalf("response changed: %q, err = %v", got, err)
				}
			})
		}
	}
}

func TestMimoHeaderFallbackAndVersion(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("anthropic-version", "custom-version")
	meta := &util.RelayMeta{ChannelType: common.ChannelTypeMimo, Mode: constant.RelayModeClaude, APIKey: "fallback-key"}
	a := helper.GetAdaptor(constant.ChannelType2APIType(meta.ChannelType))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if err := a.SetupRequestHeader(c, req, meta); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "Bearer fallback-key" || req.Header.Get("anthropic-version") != "custom-version" {
		t.Fatal("key fallback or explicit Anthropic version was not preserved")
	}
}

func TestMimoChatThinkingAndTokenLimit(t *testing.T) {
	var request model.GeneralOpenAIRequest
	if err := json.Unmarshal([]byte(`{"model":"mimo-v2.5-pro","max_tokens":128,"thinking":{"type":"disabled"},"messages":[{"role":"assistant","content":"OK","reasoning_content":"reasoning"}]}`), &request); err != nil {
		t.Fatal(err)
	}
	a := helper.GetAdaptor(constant.ChannelType2APIType(common.ChannelTypeMimo))
	converted, err := a.ConvertRequest(nil, constant.RelayModeChatCompletions, &request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(converted)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	if body["thinking"].(map[string]any)["type"] != "disabled" {
		t.Fatal("thinking configuration was lost")
	}
	if _, exists := body["max_tokens"]; exists || body["max_completion_tokens"] != float64(128) {
		t.Fatalf("token limit was not converted: %s", data)
	}
	if !strings.Contains(string(data), `"reasoning_content":"reasoning"`) {
		t.Fatal("conversation reasoning content was lost")
	}
}
