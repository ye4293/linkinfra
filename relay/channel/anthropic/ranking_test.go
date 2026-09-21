package anthropic

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRankingCachePreservedThroughOpenAIConversion(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"msg_test","model":"claude-sonnet-4","content":[],"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":300,"cache_creation_input_tokens":50}}`))}
	relayErr, usage := Handler(c, resp, 0, "claude-sonnet-4")
	require.Nil(t, relayErr)
	require.NotNil(t, usage)
	require.Equal(t, 100, usage.PromptTokens, "ranking must not change billing inputs")
	require.EqualValues(t, 350, usage.RankingExtraInputTokens)
	encoded, err := json.Marshal(usage)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "RankingExtra")
}
