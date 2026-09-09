package vertexai

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/gopkg/cache/asynccache"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/relay/util"
)

type mappedModelTransport func(*http.Request) (*http.Response, error)

func (f mappedModelTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDoRequestAppliesMappedClaudeModel(t *testing.T) {
	previousClient := util.HTTPClient
	previousCache := Cache
	Cache = asynccache.NewAsyncCache(asynccache.Options{
		RefreshDuration: time.Hour,
		Fetcher:         func(string) (interface{}, error) { return "test-token", nil },
	})
	t.Cleanup(func() {
		util.HTTPClient = previousClient
		Cache.Close()
		Cache = previousCache
	})
	called := false
	util.HTTPClient = &http.Client{Transport: mappedModelTransport(func(req *http.Request) (*http.Response, error) {
		called = true
		if !strings.Contains(req.URL.Path, "publishers/anthropic/models/claude-opus-4-7:rawPredict") {
			t.Errorf("unexpected URL: %s", req.URL)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"model", "temperature", "top_p", "top_k"} {
			if _, exists := body[field]; exists {
				t.Errorf("%s must be removed for mapped Opus 4.7", field)
			}
		}
		var thinking map[string]any
		if err := json.Unmarshal(body["thinking"], &thinking); err != nil {
			t.Fatal(err)
		}
		if thinking["type"] != "adaptive" || thinking["budget_tokens"] != nil {
			t.Errorf("mapped model thinking adaptation missing: %v", thinking)
		}
		if string(body["extra"]) != `{"keep":true}` {
			t.Errorf("extension field lost: %s", body["extra"])
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	a := &Adaptor{AccountCredentials: Credentials{ProjectID: "test-proj"}}
	meta := newVertexMetaForTest("claude-opus-4-6", "us-east5", false)
	meta.ActualModelName = "claude-opus-4-7"
	resp, err := a.DoRequest(c, meta, strings.NewReader(`{"model":"claude-opus-4-6","thinking":{"type":"enabled","budget_tokens":1024},"temperature":0.5,"top_p":0.9,"top_k":10,"extra":{"keep":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !called {
		t.Fatal("upstream request was not sent")
	}
}

// TestDoRequest_ClaudeModelInvokesRewrite 证明 DoRequest 在 Claude 模型上会先走
// rewriteBodyForVertexClaude 再去建 URL。
//
// 测试手法：传入非法 JSON 作为 body。如果 rewrite 分支被触发，会在 json.Unmarshal 阶段
// 返回 "rewriteBodyForVertexClaude: invalid json ..." 错误；如果分支没被触发，则会往下走到
// GetRequestURL（此处 ProjectID 缺失，报 "vertex AI project ID not found ..."）。
// 通过 error 字符串精确区分两条路径。
//
// 注意：我们不调用真实 HTTP，也不触发 SetupRequestHeader 里的 GetAccessToken。
// 真端到端验收交给 Task 6 smoke script。
func TestDoRequest_ClaudeModelInvokesRewrite(t *testing.T) {
	a := &Adaptor{} // 不设 AccountCredentials.ProjectID，若 rewrite 未被触发会走到 project ID 缺失的错误分支
	meta := newVertexMetaForTest("claude-opus-4-7", "us-east5", false)

	invalidBody := bytes.NewReader([]byte(`{not json`))
	_, err := a.DoRequest(nil, meta, invalidBody)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "rewriteBodyForVertexClaude") {
		t.Fatalf("expected rewriteBodyForVertexClaude path to run first and error on invalid json, got: %v", err)
	}
}

// TestDoRequest_ClaudeModelDetectedViaActualModelName 验证判断 Claude 分支时会同时看
// OriginModelName 和 ActualModelName：仅 ActualModelName 为 Claude 时也要触发改写。
func TestDoRequest_ClaudeModelDetectedViaActualModelName(t *testing.T) {
	a := &Adaptor{}
	meta := newVertexMetaForTest("gemini-2.5-pro", "us-east5", false)
	meta.OriginModelName = "some-alias"
	meta.ActualModelName = "claude-sonnet-4-5"

	invalidBody := bytes.NewReader([]byte(`{not json`))
	_, err := a.DoRequest(nil, meta, invalidBody)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "rewriteBodyForVertexClaude") {
		t.Fatalf("expected rewrite to be triggered via ActualModelName, got: %v", err)
	}
}

// TestDoRequest_GeminiBypassesRewrite 验证 Gemini 模型不走 rewrite 路径：
// 传入同样的非法 JSON，Gemini 分支会直接跳过 rewrite 走到 GetRequestURL，
// 因为 ProjectID 缺失，最终报 "project ID not found"，而不是 rewrite 错误。
func TestDoRequest_GeminiBypassesRewrite(t *testing.T) {
	a := &Adaptor{}
	meta := newVertexMetaForTest("gemini-2.5-pro", "us-central1", false)

	invalidBody := bytes.NewReader([]byte(`{not json`))
	_, err := a.DoRequest(nil, meta, invalidBody)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), "rewriteBodyForVertexClaude") {
		t.Fatalf("gemini model must NOT trigger claude rewrite, got: %v", err)
	}
	if !strings.Contains(err.Error(), "project ID not found") {
		t.Fatalf("expected gemini path to fall through to project-id error, got: %v", err)
	}
}
