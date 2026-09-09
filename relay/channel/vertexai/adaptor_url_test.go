package vertexai

import (
	"strings"
	"testing"

	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/util"
)

func newVertexMetaForTest(modelName, region string, stream bool) *util.RelayMeta {
	return &util.RelayMeta{
		ChannelId:       1,
		OriginModelName: modelName,
		ActualModelName: modelName,
		IsStream:        stream,
		Config: model.ChannelConfig{
			Region: region,
		},
	}
}

// 注意：该测试依赖"能拿到 projectID"。为绕开凭证解析，我们用 AccountCredentials 的 ProjectID fallback。
func TestGetRequestURL_ClaudeNonStream(t *testing.T) {
	a := &Adaptor{
		AccountCredentials: Credentials{ProjectID: "test-proj"},
	}
	meta := newVertexMetaForTest("claude-opus-4-1-20250805", "us-east5", false)
	url, err := a.GetRequestURL(meta)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	wantSubstr := "us-east5-aiplatform.googleapis.com/v1/projects/test-proj/locations/us-east5/publishers/anthropic/models/claude-opus-4-1@20250805:rawPredict"
	if !strings.Contains(url, wantSubstr) {
		t.Errorf("url = %q\nwant contains %q", url, wantSubstr)
	}
	if strings.Contains(url, "?alt=sse") {
		t.Errorf("non-stream URL should not contain alt=sse: %s", url)
	}
	if strings.Contains(url, "publishers/google") {
		t.Errorf("claude model should NOT use google publisher: %s", url)
	}
}

func TestGetRequestURL_ClaudeStream(t *testing.T) {
	a := &Adaptor{
		AccountCredentials: Credentials{ProjectID: "test-proj"},
	}
	meta := newVertexMetaForTest("claude-opus-4-7", "global", true)
	url, err := a.GetRequestURL(meta)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(url, "locations/global/publishers/anthropic/models/claude-opus-4-7:streamRawPredict?alt=sse") {
		t.Errorf("stream URL wrong: %s", url)
	}
	if !strings.HasPrefix(url, "https://aiplatform.googleapis.com/") {
		t.Errorf("global region should use root endpoint: %s", url)
	}
}

func TestGetRequestURL_ThinkingSuffixStripped(t *testing.T) {
	a := &Adaptor{AccountCredentials: Credentials{ProjectID: "test-proj"}}
	meta := newVertexMetaForTest("claude-opus-4-7-thinking", "us-east5", false)
	url, err := a.GetRequestURL(meta)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(url, "-thinking") {
		t.Errorf("URL must not contain -thinking suffix, got %s", url)
	}
	if !strings.Contains(url, "publishers/anthropic/models/claude-opus-4-7:rawPredict") {
		t.Errorf("URL should use base model name claude-opus-4-7, got %s", url)
	}
}

func TestGetRequestURL_GeminiStillWorks(t *testing.T) {
	a := &Adaptor{
		AccountCredentials: Credentials{ProjectID: "test-proj"},
	}
	meta := newVertexMetaForTest("gemini-2.5-pro", "us-central1", false)
	url, err := a.GetRequestURL(meta)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(url, "publishers/google/models/gemini-2.5-pro:generateContent") {
		t.Errorf("gemini path broken: %s", url)
	}
}

func TestGetRequestURLUsesMappedModel(t *testing.T) {
	for _, origin := range []string{"my-alias", "claude-opus-4-6"} {
		for _, stream := range []bool{false, true} {
			a := &Adaptor{AccountCredentials: Credentials{ProjectID: "test-proj"}}
			meta := newVertexMetaForTest(origin, "us-east5", stream)
			meta.ActualModelName = "claude-opus-4-7"
			url, err := a.GetRequestURL(meta)
			if err != nil {
				t.Fatal(err)
			}
			want := "publishers/anthropic/models/claude-opus-4-7:" + claudeSuffix(stream)
			if !strings.Contains(url, want) {
				t.Errorf("origin %q: URL = %q, want %q", origin, url, want)
			}
		}
	}
}
