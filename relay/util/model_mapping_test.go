package util

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRewriteRequestModelPreservesNativeFields(t *testing.T) {
	const input = `{"model":"alias","stream":true,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"id","content":"hello"}]}],"thinking":{"type":"adaptive"},"context_management":{"edits":[]},"extra":{"integer":9007199254740993},"max_tokens":8192}`
	body := []byte(input)
	for _, target := range []string{"claude-opus-4-7", "glm-5", `provider/quoted"model`} {
		mapped, err := RewriteRequestModel(body, target)
		if err != nil {
			t.Fatal(err)
		}
		var before, after map[string]json.RawMessage
		if err := json.Unmarshal(body, &before); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(mapped, &after); err != nil {
			t.Fatal(err)
		}
		var model string
		if err := json.Unmarshal(after["model"], &model); err != nil || model != target {
			t.Fatalf("model = %q, want %q; err = %v", model, target, err)
		}
		delete(before, "model")
		delete(after, "model")
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("native fields changed: %s", mapped)
		}
		if string(body) != input {
			t.Fatal("original request was mutated; channel retries need the original alias")
		}
	}
}

func TestRewriteRequestModelRejectsInvalidBody(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `{`, `"text"`} {
		if _, err := RewriteRequestModel([]byte(body), "claude-opus-4-7"); err == nil {
			t.Errorf("expected error for %s", body)
		}
	}
}
