package ali_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/helper"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
	"github.com/tidwall/gjson"
)

// 显式提供环境变量才进行付费上游调用，凭据不写入测试结果。
func TestAliLive(t *testing.T) {
	key, base := os.Getenv("ALI_VERIFY_API_KEY"), os.Getenv("ALI_VERIFY_BASE_URL")
	if key == "" || base == "" {
		t.Skip("ALI_VERIFY_API_KEY and ALI_VERIFY_BASE_URL are required")
	}
	gin.SetMode(gin.TestMode)
	oldClient := util.HTTPClient
	transport := http.DefaultTransport.(*http.Transport).Clone()
	util.HTTPClient = &http.Client{Transport: transport, Timeout: 45 * time.Second}
	t.Cleanup(func() {
		util.HTTPClient = oldClient
		transport.CloseIdleConnections()
	})
	redact := func(s string) string { return strings.ReplaceAll(s, key, "[REDACTED]") }
	call := func(t *testing.T, path, body string) (string, string) {
		t.Helper()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		meta := &util.RelayMeta{
			ChannelType: common.ChannelTypeAli, BaseURL: base,
			Mode: constant.Path2RelayMode(path), RequestURLPath: path,
			ActualAPIKey: key, IsStream: gjson.Get(body, "stream").Bool(), DisablePing: true,
		}
		a := helper.GetAdaptor(constant.ChannelType2APIType(meta.ChannelType))
		if meta.Mode == constant.RelayModeChatCompletions || meta.Mode == constant.RelayModeEmbeddings {
			var request model.GeneralOpenAIRequest
			if err := common.UnmarshalBodyReusable(c, &request); err != nil {
				t.Fatal(err)
			}
			converted, err := a.ConvertRequest(c, meta.Mode, &request)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(converted)
			if err != nil {
				t.Fatal(err)
			}
			body = string(encoded)
		}
		resp, err := a.DoRequest(c, meta, strings.NewReader(body))
		if err != nil {
			t.Fatal(redact(err.Error()))
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		if err != nil {
			t.Fatal(redact(err.Error()))
		}
		result := redact(string(data))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HTTP %d: %s", resp.StatusCode, result)
		}
		t.Logf("HTTP %d; model=%s; stream=%t", resp.StatusCode, gjson.Get(body, "model").String(), meta.IsStream)
		return result, resp.Header.Get("Content-Type")
	}

	for _, name := range []string{"qwen3.8-flash", "qwen3.8-max", "qwen3.7-plus", "qwen3-coder-next"} {
		t.Run("model/"+name, func(t *testing.T) {
			data, _ := call(t, "/v1/chat/completions", `{"model":"`+name+`","max_tokens":32,"enable_thinking":false,"messages":[{"role":"user","content":"Reply only OK."}]}`)
			if !strings.Contains(gjson.Get(data, "choices.0.message.content").String(), "OK") || !gjson.Get(data, "usage").Exists() {
				t.Fatalf("missing model output/usage: %s", data)
			}
		})
	}
	for _, name := range []string{"text-embedding-v4", "qwen3.7-text-embedding", "qwen3.7-text-embedding-flash"} {
		t.Run("embedding/"+name, func(t *testing.T) {
			data, _ := call(t, "/v1/embeddings", `{"model":"`+name+`","input":["hello"],"dimensions":256,"encoding_format":"float"}`)
			if n := len(gjson.Get(data, "data.0.embedding").Array()); n != 256 {
				t.Fatalf("embedding dimensions = %d", n)
			}
		})
	}

	t.Run("chat/preserve_thinking", func(t *testing.T) {
		first, _ := call(t, "/v1/chat/completions", `{"model":"qwen3.8-flash","max_tokens":512,"reasoning_effort":"low","preserve_thinking":true,"messages":[{"role":"user","content":"What is 2+3? Answer only the number."}]}`)
		message := gjson.Get(first, "choices.0.message")
		if message.Get("reasoning_content").String() == "" || !strings.Contains(message.Get("content").String(), "5") {
			t.Fatalf("missing thinking or answer: %s", first)
		}
		body := `{"model":"qwen3.8-flash","max_tokens":512,"reasoning_effort":"low","preserve_thinking":true,"messages":[{"role":"user","content":"What is 2+3? Answer only the number."},` + message.Raw + `,{"role":"user","content":"Now multiply that result by 2. Answer only the number."}]}`
		second, _ := call(t, "/v1/chat/completions", body)
		if !strings.Contains(gjson.Get(second, "choices.0.message.content").String(), "10") {
			t.Fatalf("multi-turn thinking failed: %s", second)
		}
	})

	for _, stream := range []bool{false, true} {
		suffix, streamField := "json", "false"
		streamOptions := ""
		if stream {
			suffix, streamField = "stream", "true"
			streamOptions = `,"stream_options":{"include_usage":true}`
		}
		t.Run("chat/parallel_tools/"+suffix, func(t *testing.T) {
			body := `{"model":"qwen3.8-flash","stream":` + streamField + streamOptions + `,"max_tokens":512,"enable_thinking":false,"parallel_tool_calls":true,"tool_stream":true,"messages":[{"role":"user","content":"Call get_weather twice, once for Beijing and once for Shanghai. Do not answer in text."}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather for one city.","parameters":{"type":"object","properties":{"city":{"type":"string","enum":["Beijing","Shanghai"]}},"required":["city"]}}}]}`
			data, contentType := call(t, "/v1/chat/completions", body)
			calls := map[int]string{}
			if stream {
				assertAliStream(t, data, contentType, "[DONE]")
				for _, event := range aliSSEData(t, data) {
					for _, tool := range gjson.Get(event, "choices.0.delta.tool_calls").Array() {
						calls[int(tool.Get("index").Int())] += tool.Get("function.arguments").String()
					}
				}
			} else {
				for i, tool := range gjson.Get(data, "choices.0.message.tool_calls").Array() {
					calls[i] = tool.Get("function.arguments").String()
				}
			}
			cities := map[string]bool{}
			for _, args := range calls {
				if !gjson.Valid(args) {
					t.Fatalf("invalid tool arguments: %s", args)
				}
				cities[gjson.Get(args, "city").String()] = true
			}
			if len(calls) != 2 || !cities["Beijing"] || !cities["Shanghai"] {
				t.Fatalf("expected two city tool calls, got %v", calls)
			}
		})
		t.Run("messages/schema_effort/"+suffix, func(t *testing.T) {
			body := `{"model":"qwen3.8-flash","stream":` + streamField + `,"max_tokens":512,"thinking":{"type":"enabled"},"output_config":{"effort":"low","format":{"type":"json_schema","schema":{"type":"object","properties":{"ok":{"type":"boolean"},"count":{"type":"integer"}},"required":["ok","count"],"additionalProperties":false}}},"messages":[{"role":"user","content":"Return JSON with ok true and count 3, and no other fields."}]}`
			data, contentType := call(t, "/v1/messages", body)
			var text, thinking string
			if stream {
				assertAliStream(t, data, contentType, "message_stop")
				for _, event := range aliSSEData(t, data) {
					text += gjson.Get(event, "delta.text").String()
					thinking += gjson.Get(event, "delta.thinking").String()
				}
			} else {
				for _, block := range gjson.Get(data, "content").Array() {
					text += block.Get("text").String()
					thinking += block.Get("thinking").String()
				}
			}
			if !gjson.Valid(text) || len(gjson.Parse(text).Map()) != 2 || !gjson.Get(text, "ok").Bool() || gjson.Get(text, "count").Int() != 3 || thinking == "" {
				t.Fatalf("missing thinking or invalid schema output: %s", data)
			}
		})
		t.Run("responses/"+suffix, func(t *testing.T) {
			body := `{"model":"qwen3.8-flash","stream":` + streamField + `,"max_output_tokens":128,"reasoning":{"effort":"none"},"store":false,"input":"Reply only OK."}`
			data, contentType := call(t, "/v1/responses", body)
			if stream {
				assertAliStream(t, data, contentType, "response.completed")
			} else if gjson.Get(data, "status").String() != "completed" || !gjson.Get(data, "usage").Exists() {
				t.Fatalf("Responses request incomplete: %s", data)
			}
			if !strings.Contains(data, "OK") {
				t.Fatalf("Responses text missing: %s", data)
			}
		})
	}
	t.Run("chat/complex_tool_stream", func(t *testing.T) {
		body := `{"model":"qwen3.8-flash","stream":true,"max_tokens":512,"enable_thinking":false,"parallel_tool_calls":false,"tool_stream":true,"messages":[{"role":"user","content":"Call record_cities with these eight cities in order: Beijing, Shanghai, Tokyo, Paris, Berlin, London, Rome, Madrid."}],"tool_choice":{"type":"function","function":{"name":"record_cities"}},"tools":[{"type":"function","function":{"name":"record_cities","description":"Record cities in order.","parameters":{"type":"object","properties":{"cities":{"type":"array","items":{"type":"string"}}},"required":["cities"]}}}]}`
		data, contentType := call(t, "/v1/chat/completions", body)
		assertAliStream(t, data, contentType, "[DONE]")
		var arguments string
		fragments := 0
		for _, event := range aliSSEData(t, data) {
			for _, tool := range gjson.Get(event, "choices.0.delta.tool_calls").Array() {
				if fragment := tool.Get("function.arguments").String(); fragment != "" {
					arguments += fragment
					fragments++
				}
			}
		}
		if !gjson.Valid(arguments) || len(gjson.Get(arguments, "cities").Array()) != 8 || fragments < 2 {
			t.Fatalf("invalid or unchunked complex tool arguments: fragments=%d, arguments=%s", fragments, arguments)
		}
		t.Logf("complex tool arguments received in %d fragments", fragments)
	})
}

func aliSSEData(t *testing.T, body string) []string {
	t.Helper()
	var events []string
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data != "[DONE]" {
				if !gjson.Valid(data) {
					t.Fatalf("invalid SSE data: %s", data)
				}
				if gjson.Get(data, "error").Exists() || gjson.Get(data, "type").String() == "error" {
					t.Fatalf("upstream SSE error: %s", data)
				}
				events = append(events, data)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func assertAliStream(t *testing.T, data, contentType, terminal string) {
	t.Helper()
	if !strings.Contains(contentType, "text/event-stream") || !strings.Contains(data, terminal) {
		t.Fatalf("incomplete SSE response: %s", data)
	}
	aliSSEData(t, data)
}
