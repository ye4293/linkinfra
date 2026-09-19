package ali_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/relay/channel/ali"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/helper"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

// 经由实际渠道注册、请求解析和转换，验证模型映射不会丢失扩展参数。
func TestAliPreservesChatParameters(t *testing.T) {
	body := `{"model":"alias","temperature":0,"max_tokens":256,"max_completion_tokens":512,"enable_thinking":false,"preserve_thinking":false,"thinking_budget":0,"reasoning_effort":"low","parallel_tool_calls":false,"tool_stream":false,"enable_search":false,"search_options":{"forced_search":false},"messages":[{"role":"assistant","content":"OK","reasoning_content":"reason"},{"role":"user","content":[{"type":"text","text":"continue","cache_control":{"type":"ephemeral"}}]}],"tools":[{"type":"function","function":{"name":"weather","strict":true,"parameters":{"type":"object","properties":{"cities":{"type":"array","items":{"type":"string"}}},"additionalProperties":false}}}],"stream_options":{"include_usage":false},"vendor_number":9007199254740993}`
	for _, mappedModel := range []string{"qwen3.8-flash", "qwen3.8-flash-internet"} {
		t.Run(mappedModel, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			var request model.GeneralOpenAIRequest
			if err := common.UnmarshalBodyReusable(c, &request); err != nil {
				t.Fatal(err)
			}
			request.Model = mappedModel
			a := helper.GetAdaptor(constant.ChannelType2APIType(common.ChannelTypeAli))
			converted, err := a.ConvertRequest(c, constant.RelayModeChatCompletions, &request)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(converted)
			if err != nil {
				t.Fatal(err)
			}
			var want, got map[string]json.RawMessage
			if err := json.Unmarshal([]byte(body), &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			want["model"] = json.RawMessage(`"qwen3.8-flash"`)
			if strings.HasSuffix(mappedModel, "-internet") {
				want["enable_search"] = json.RawMessage("true")
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("parameters changed: %s", encoded)
			}
			if request.MaxTokens != 256 || request.MaxCompletionTokens != 512 {
				t.Fatal("token limit semantics changed")
			}
			original, err := common.GetRequestBody(c)
			if err != nil || string(original) != body {
				t.Fatal("original request changed, affecting retries")
			}
		})
	}
}

func TestAliProgrammaticRequest(t *testing.T) {
	for _, suffix := range []string{"", "-internet"} {
		for _, body := range []bool{false, true} {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if !body {
				c.Request.Body = nil
			}
			request := &model.GeneralOpenAIRequest{Model: "qwen-plus" + suffix, MaxTokens: 64}
			converted, err := (&ali.Adaptor{}).ConvertRequest(c, constant.RelayModeChatCompletions, request)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(converted)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			if got["model"] != "qwen-plus" || got["max_tokens"] != float64(64) {
				t.Fatalf("unexpected programmatic request: %s", encoded)
			}
			if suffix != "" && got["enable_search"] != true {
				t.Fatal("search suffix no longer enables search")
			}
		}
	}
}

func TestAliRejectsInvalidBody(t *testing.T) {
	a := &ali.Adaptor{}
	if _, err := a.ConvertRequest(nil, constant.RelayModeChatCompletions, nil); err == nil {
		t.Fatal("nil request accepted")
	}
	for _, body := range []string{"null", "[]", "{"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		if _, err := a.ConvertRequest(c, constant.RelayModeChatCompletions, &model.GeneralOpenAIRequest{Model: "qwen-plus"}); err == nil {
			t.Fatalf("invalid body accepted: %s", body)
		}
	}
}

func TestAliCurrentProtocolPaths(t *testing.T) {
	for _, base := range []string{"", "https://dashscope.aliyuncs.com/", "https://test-workspace.cn-beijing.maas.aliyuncs.com/"} {
		for mode, path := range map[int]string{
			constant.RelayModeChatCompletions: "/compatible-mode/v1/chat/completions",
			constant.RelayModeEmbeddings:      "/compatible-mode/v1/embeddings",
			constant.RelayModeOpenaiResponse:  "/compatible-mode/v1/responses",
			constant.RelayModeClaude:          "/apps/anthropic/v1/messages",
		} {
			got, err := (&ali.Adaptor{}).GetRequestURL(&util.RelayMeta{BaseURL: base, Mode: mode})
			wantBase := strings.TrimRight(base, "/")
			if wantBase == "" {
				wantBase = "https://dashscope.aliyuncs.com"
			}
			if err != nil || got != wantBase+path {
				t.Fatalf("mode %d: URL %q, error %v", mode, got, err)
			}
		}
	}
}

// 模拟上游接收 Messages 原始请求，覆盖局部模型改写后的 effort/Schema 透传。
func TestAliMessagesExtensionsOnWire(t *testing.T) {
	body := `{"model":"alias","max_tokens":64,"thinking":{"type":"enabled"},"output_config":{"effort":"low","format":{"type":"json_schema","schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}}},"messages":[{"role":"user","content":"Return JSON with ok true."}]}`
	mapped, err := util.RewriteRequestModel([]byte(body), "qwen3.8-flash")
	if err != nil {
		t.Fatal(err)
	}
	var received map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apps/anthropic/v1/messages" || r.Header.Get("Authorization") != "Bearer selected-key" {
			t.Error("incorrect Messages path or selected channel key")
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("anthropic-beta") != "test-beta" {
			t.Error("Messages compatibility headers missing")
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[]}`))
	}))
	defer server.Close()
	oldClient := util.HTTPClient
	util.HTTPClient = server.Client()
	t.Cleanup(func() { util.HTTPClient = oldClient })
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(mapped)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("anthropic-beta", "test-beta")
	resp, err := (&ali.Adaptor{}).DoRequest(c, &util.RelayMeta{
		BaseURL: server.URL, Mode: constant.RelayModeClaude,
		APIKey: "fallback-key", ActualAPIKey: "selected-key", DisablePing: true,
	}, strings.NewReader(string(mapped)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var want map[string]json.RawMessage
	if err := json.Unmarshal(mapped, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, received) {
		t.Fatal("Messages extension fields changed on wire")
	}
}
