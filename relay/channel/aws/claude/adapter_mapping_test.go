package aws

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/ctxkey"
	"github.com/songquanpeng/one-api/relay/util"
)

func TestNativeRetryUsesCurrentMappedModel(t *testing.T) {
	for _, stream := range []bool{false, true} {
		called := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			if !strings.Contains(r.URL.Path, "anthropic.claude-opus-4-7") {
				t.Errorf("retry used stale model: %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"test upstream rejection"}`))
		}))
		client := bedrockruntime.New(bedrockruntime.Options{
			Region: "us-east-1", BaseEndpoint: aws.String(server.URL),
			Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
		})
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"alias","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
		c.Set(ctxkey.RequestModel, "claude-opus-4-6")
		meta := &util.RelayMeta{OriginModelName: "alias", ActualModelName: "claude-opus-4-7", IsStream: stream}
		_, relayErr := (&Adaptor{}).DoResponse(c, client, meta)
		server.Close()
		if !called || relayErr == nil {
			t.Fatalf("expected mocked upstream rejection; called=%v, err=%v", called, relayErr)
		}
		if got := c.GetString(ctxkey.RequestModel); got != meta.ActualModelName {
			t.Errorf("request model = %q, want %q", got, meta.ActualModelName)
		}
	}
}
