package aws

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/ctxkey"
	relaymodel "github.com/songquanpeng/one-api/relay/model"
)

type streamingRecorder struct {
	*httptest.ResponseRecorder
}

func (r *streamingRecorder) CloseNotify() <-chan bool {
	return make(chan bool)
}

func TestConvertedClaudeRequestSampling(t *testing.T) {
	for _, model := range []string{
		"claude-opus-4-8", "claude-opus-4-8-thinking",
		"claude-opus-4-7", "claude-opus-4-7-thinking",
		"claude-opus-4-6",
	} {
		for _, stream := range []bool{false, true} {
			name := model + "/non-stream"
			if stream {
				name = model + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				var payload map[string]any
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Errorf("decode Bedrock request: %v", err)
					}
					if stream {
						w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"test","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
				}))
				defer server.Close()
				client := bedrockruntime.New(bedrockruntime.Options{
					Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
					AuthSchemePreference: []string{"aws.auth#sigv4"},
					BaseEndpoint:         aws.String(server.URL),
				})
				ctx, _ := gin.CreateTestContext(&streamingRecorder{httptest.NewRecorder()})
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				adaptor := &Adaptor{}
				_, err := adaptor.ConvertRequest(ctx, 0, &relaymodel.GeneralOpenAIRequest{
					Model: model, MaxTokens: 4096, Stream: stream,
					Temperature: 0.2, TopP: 0.9, TopK: 10,
					Messages: []relaymodel.Message{{Role: "user", Content: "hi"}},
				})
				if err != nil {
					t.Fatal(err)
				}
				var relayErr *relaymodel.ErrorWithStatusCode
				if stream {
					relayErr, _ = StreamHandler(ctx, client, nil)
				} else {
					relayErr, _ = Handler(ctx, client, nil)
				}
				if relayErr != nil {
					t.Fatalf("Bedrock handler failed: %+v", relayErr)
				}
				if payload == nil {
					t.Fatal("Bedrock request was not sent")
				}
				if model == "claude-opus-4-6" {
					if payload["temperature"] != 0.2 || payload["top_p"] != 0.9 || payload["top_k"] != float64(10) {
						t.Fatalf("older model sampling parameters changed: %#v", payload)
					}
					return
				}
				for _, field := range []string{"temperature", "top_p", "top_k"} {
					if value, exists := payload[field]; exists {
						t.Errorf("%s must be omitted for %s, got %v", field, ctx.GetString(ctxkey.RequestModel), value)
					}
				}
			})
		}
	}
}

func TestBuildNativeClaudeRequestBody_PreservesBuiltInTools(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model": "claude-opus-4-6",
		"stream": true,
		"max_tokens": 16384,
		"temperature": 0.2,
		"thinking": {
			"type": "enabled",
			"budget_tokens": 2048
		},
		"messages": [
			{
				"role": "user",
				"content": [
					{
						"type": "text",
						"text": "who r u"
					}
				]
			}
		],
		"system": [
			{
				"type": "text",
				"text": "system prompt",
				"cache_control": {
					"type": "ephemeral"
				}
			}
		],
		"tools": [
			{
				"name": "computer",
				"type": "computer_20251124",
				"display_width_px": 1024,
				"display_height_px": 768,
				"display_number": 1
			},
			{
				"name": "str_replace_based_edit_tool",
				"type": "text_editor_20250728",
				"max_characters": 4096
			},
			{
				"name": "bash",
				"type": "bash_20250124"
			}
		],
		"tool_choice": {
			"type": "auto"
		},
		"output_config": {
			"format": {
				"type": "json_schema",
				"schema": {
					"type": "object",
					"properties": {
						"ok": {
							"type": "boolean"
						}
					}
				}
			}
		}
	}`))
	ctx.Request.Header.Set("anthropic-beta", "computer-use-2025-11-24")

	requestBody, err := buildNativeClaudeRequestBody(ctx)
	if err != nil {
		t.Fatalf("buildNativeClaudeRequestBody returned error: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(requestBody, &payload); err != nil {
		t.Fatalf("failed to unmarshal request body: %v", err)
	}

	if got := payload["anthropic_version"]; got != "bedrock-2023-05-31" {
		t.Fatalf("unexpected anthropic_version: %v", got)
	}
	if _, exists := payload["model"]; exists {
		t.Fatalf("model field should not be forwarded to Bedrock")
	}
	if _, exists := payload["stream"]; exists {
		t.Fatalf("stream field should not be forwarded to Bedrock")
	}
	if got := payload["temperature"]; got != float64(1) {
		t.Fatalf("thinking request should force temperature=1, got %v", got)
	}

	betaValues, ok := payload["anthropic_beta"].([]any)
	if !ok || len(betaValues) != 1 || betaValues[0] != "computer-use-2025-11-24" {
		t.Fatalf("unexpected anthropic_beta payload: %#v", payload["anthropic_beta"])
	}

	systemBlocks, ok := payload["system"].([]any)
	if !ok || len(systemBlocks) != 1 {
		t.Fatalf("unexpected system payload: %#v", payload["system"])
	}
	systemBlock, ok := systemBlocks[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected system block type: %#v", systemBlocks[0])
	}
	cacheControl, ok := systemBlock["cache_control"].(map[string]any)
	if !ok || cacheControl["type"] != "ephemeral" {
		t.Fatalf("cache_control should be preserved, got %#v", systemBlock["cache_control"])
	}

	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 3 {
		t.Fatalf("unexpected tools payload: %#v", payload["tools"])
	}

	computerTool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected computer tool type: %#v", tools[0])
	}
	if computerTool["type"] != "computer_20251124" ||
		computerTool["display_width_px"] != float64(1024) ||
		computerTool["display_height_px"] != float64(768) ||
		computerTool["display_number"] != float64(1) {
		t.Fatalf("computer tool fields were not preserved: %#v", computerTool)
	}

	editorTool, ok := tools[1].(map[string]any)
	if !ok {
		t.Fatalf("unexpected editor tool type: %#v", tools[1])
	}
	if editorTool["type"] != "text_editor_20250728" || editorTool["max_characters"] != float64(4096) {
		t.Fatalf("text editor tool fields were not preserved: %#v", editorTool)
	}

	bashTool, ok := tools[2].(map[string]any)
	if !ok {
		t.Fatalf("unexpected bash tool type: %#v", tools[2])
	}
	if bashTool["type"] != "bash_20250124" {
		t.Fatalf("bash tool fields were not preserved: %#v", bashTool)
	}

	toolChoice, ok := payload["tool_choice"].(map[string]any)
	if !ok || toolChoice["type"] != "auto" {
		t.Fatalf("tool_choice should be preserved, got %#v", payload["tool_choice"])
	}

	outputConfig, ok := payload["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config should be preserved, got %#v", payload["output_config"])
	}
	format, ok := outputConfig["format"].(map[string]any)
	if !ok || format["type"] != "json_schema" {
		t.Fatalf("output_config.format should be preserved, got %#v", outputConfig["format"])
	}
}
