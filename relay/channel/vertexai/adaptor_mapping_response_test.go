package vertexai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/relay/channel/openai"
)

func TestDoResponseUsesMappedModelProtocol(t *testing.T) {
	previousApproximate := config.ApproximateTokenEnabled
	config.ApproximateTokenEnabled = true
	t.Cleanup(func() { config.ApproximateTokenEnabled = previousApproximate })
	for _, tc := range []struct {
		origin, actual, body string
	}{
		{
			"claude-opus-4-6", "gemini-2.5-pro",
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3}}`,
		},
		{
			"gemini-2.5-pro", "claude-opus-4-7",
			`{"id":"test","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`,
		},
	} {
		t.Run(tc.origin+"/"+tc.actual, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			meta := newVertexMetaForTest(tc.origin, "global", false)
			meta.ActualModelName = tc.actual
			resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}
			usage, err := (&Adaptor{}).DoResponse(ctx, resp, meta)
			if err != nil {
				t.Fatalf("response parsing failed: %+v", err)
			}
			if usage == nil || usage.PromptTokens != 2 || usage.CompletionTokens != 3 {
				t.Fatalf("incorrect usage: %+v", usage)
			}
			var result openai.TextResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Model != tc.actual || len(result.Choices) != 1 || result.Choices[0].Message.Content != "hello" {
				t.Fatalf("incorrect mapped response: %s", recorder.Body.String())
			}
		})
	}
}
