package aws

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/gin-gonic/gin"
	relaymodel "github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

func TestSamplingFallbackAcrossRequestPaths(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("native=%t/stream=%t", native, stream), func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					var payload map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					if string(payload["max_tokens"]) != "4096" || len(payload["messages"]) == 0 {
						t.Errorf("request content changed: %v", payload)
					}
					for _, field := range []string{"temperature", "top_p", "top_k"} {
						if _, exists := payload[field]; exists {
							w.Header().Set("Content-Type", "application/json")
							w.Header().Set("X-Amzn-Errortype", "ValidationException")
							w.WriteHeader(http.StatusBadRequest)
							_ = json.NewEncoder(w).Encode(map[string]string{"message": "`" + field + "` is deprecated for this model."})
							return
						}
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
					Region: "us-east-1", BaseEndpoint: aws.String(server.URL),
					Credentials:          credentials.NewStaticCredentialsProvider("test", "test", ""),
					AuthSchemePreference: []string{"aws.auth#sigv4"},
				})
				ctx, _ := gin.CreateTestContext(&streamingRecorder{httptest.NewRecorder()})
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"max_tokens":4096,"messages":[{"role":"user","content":"hi"}],"temperature":0.2,"top_p":0.9,"top_k":10}`))
				// An opaque mapped profile must work without knowing the model name.
				model := "arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/test"
				adaptor := &Adaptor{}
				if !native {
					_, err := adaptor.ConvertRequest(ctx, 0, &relaymodel.GeneralOpenAIRequest{
						Model: model, MaxTokens: 4096, Stream: stream,
						Temperature: 0.2, TopP: 0.9, TopK: 10,
						Messages: []relaymodel.Message{{Role: "user", Content: "hi"}},
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				_, relayErr := adaptor.DoResponse(ctx, client, &util.RelayMeta{ActualModelName: model, IsStream: stream})
				if relayErr != nil || calls != 4 {
					t.Fatalf("expected success after 3 parameter rejections: calls=%d, err=%+v", calls, relayErr)
				}
			})
		}
	}
}

func TestSamplingFallbackStopsOnUnrelatedOrRepeatedErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		calls int
	}{
		{"invalid value", &types.ValidationException{Message: aws.String("temperature must be between 0 and 1")}, 1},
		{"other field", &types.ValidationException{Message: aws.String("max_tokens is not supported for this model.")}, 1},
		{"access denied", &types.AccessDeniedException{Message: aws.String("temperature is deprecated for this model.")}, 1},
		{"transport error", errors.New("temperature is deprecated for this model."), 1},
		{"absent parameter", &types.ValidationException{Message: aws.String("top_k is deprecated for this model.")}, 1},
		{"repeated rejection", &types.ValidationException{Message: aws.String("temperature is deprecated for this model.")}, 2},
		{"unsupported parameter", fmt.Errorf("wrapped: %w", &types.ValidationException{Message: aws.String("'temperature' is not supported by this model.")}), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			body := []byte(`{"temperature":0.2,"max_tokens":32}`)
			_, err := withSamplingFallback(body, func([]byte) (string, error) {
				calls++
				return "", tc.err
			})
			if err != tc.err || calls != tc.calls {
				t.Fatalf("calls=%d, err=%v; want calls=%d and original error", calls, err, tc.calls)
			}
			if string(body) != `{"temperature":0.2,"max_tokens":32}` {
				t.Fatal("original request body was modified")
			}
		})
	}
}
