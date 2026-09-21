package aws

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/gin-gonic/gin"
	relaymodel "github.com/songquanpeng/one-api/relay/model"
	"github.com/stretchr/testify/require"
)

func TestRankingBedrockOpenAIConversionPreservesCachedInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"test","type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":300,"cache_creation_input_tokens":50}}`)
	}))
	defer server.Close()
	client := bedrockruntime.New(bedrockruntime.Options{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), AuthSchemePreference: []string{"aws.auth#sigv4"}, BaseEndpoint: aws.String(server.URL)})
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, err := (&Adaptor{}).ConvertRequest(ctx, 0, &relaymodel.GeneralOpenAIRequest{Model: "claude-opus-4-6", MaxTokens: 4096, Messages: []relaymodel.Message{{Role: "user", Content: "test"}}})
	require.NoError(t, err)
	relayErr, usage := Handler(ctx, client, nil)
	require.Nil(t, relayErr)
	require.NotNil(t, usage)
	require.EqualValues(t, 350, usage.RankingExtraInputTokens)
	require.Equal(t, 100, usage.PromptTokens, "billing input remains unchanged")
}
