package anthropic

import (
	"testing"

	"github.com/songquanpeng/one-api/model"
)

func TestFilterBetaFlags(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		allowed map[string]struct{}
		want    []string
	}{
		{
			name:    "empty header",
			header:  "",
			allowed: BedrockAllowedBetaFlags,
			want:    nil,
		},
		{
			name:    "all allowed",
			header:  "context-management-2025-06-27,output-128k-2025-02-19",
			allowed: BedrockAllowedBetaFlags,
			want:    []string{"context-management-2025-06-27", "output-128k-2025-02-19"},
		},
		{
			name:    "some filtered",
			header:  "context-management-2025-06-27,unknown-flag-2025-01-01,output-128k-2025-02-19",
			allowed: BedrockAllowedBetaFlags,
			want:    []string{"context-management-2025-06-27", "output-128k-2025-02-19"},
		},
		{
			name:    "all filtered",
			header:  "unknown-flag-1,unknown-flag-2",
			allowed: BedrockAllowedBetaFlags,
			want:    []string{},
		},
		{
			name:    "with spaces",
			header:  " context-management-2025-06-27 , output-128k-2025-02-19 ",
			allowed: BedrockAllowedBetaFlags,
			want:    []string{"context-management-2025-06-27", "output-128k-2025-02-19"},
		},
		{
			name:    "bedrock rejects files-api",
			header:  "files-api-2025-04-14,context-management-2025-06-27",
			allowed: BedrockAllowedBetaFlags,
			want:    []string{"context-management-2025-06-27"},
		},
		{
			// Vertex 同样不支持 files-api：该 flag 已于 2026-06-11 从
			// VertexAllowedBetaFlags 移除（经官方文档交叉验证，Vertex 上
			// 无对应功能，见 docs/CHANGELOG.md）。这条用例原先断言 Vertex
			// 允许它，是改了代码没跟着改测试留下的陈旧断言。
			name:    "vertex rejects files-api",
			header:  "files-api-2025-04-14,context-management-2025-06-27",
			allowed: VertexAllowedBetaFlags,
			want:    []string{"context-management-2025-06-27"},
		},
		{
			// Vertex 独有、Bedrock 不支持的 flag，用来证明两个白名单确实不同，
			// 而不是上面那条改成 reject 后就再也测不出差异了。
			name:    "vertex allows fast-mode",
			header:  "fast-mode-2026-02-01,context-management-2025-06-27",
			allowed: VertexAllowedBetaFlags,
			want:    []string{"fast-mode-2026-02-01", "context-management-2025-06-27"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FilterBetaFlags(tt.header, tt.allowed)
			if tt.want == nil {
				if got != nil {
					t.Errorf("want nil, got %v", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("len mismatch: got %v, want %v", got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("index %d: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestInferBetaFlags(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
		want []string
	}{
		{
			name: "nil body",
			body: nil,
			want: nil,
		},
		{
			name: "empty body",
			body: map[string]any{},
			want: nil,
		},
		{
			name: "context_management present",
			body: map[string]any{
				"context_management": map[string]any{
					"edits": []any{map[string]any{"type": "clear_thinking_20251015"}},
				},
			},
			want: []string{"context-management-2025-06-27"},
		},
		{
			name: "output_config with task_budget",
			body: map[string]any{
				"output_config": map[string]any{
					"task_budget": 1000,
				},
			},
			want: []string{"task-budgets-2026-03-13"},
		},
		{
			name: "output_format present",
			body: map[string]any{
				"output_format": map[string]any{"type": "json"},
			},
			want: []string{"structured-outputs-2025-11-13"},
		},
		{
			name: "multiple inferred",
			body: map[string]any{
				"context_management": map[string]any{},
				"output_format":      "json",
			},
			want: []string{"context-management-2025-06-27", "structured-outputs-2025-11-13"},
		},
		{
			name: "output_config without task_budget",
			body: map[string]any{
				"output_config": map[string]any{
					"effort": "high",
				},
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InferBetaFlags(tt.body)
			if tt.want == nil {
				if got != nil {
					t.Errorf("want nil, got %v", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Errorf("len mismatch: got %v, want %v", got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("index %d: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestMergeBetaFlags(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		body       map[string]any
		allowed    map[string]struct{}
		wantLen    int
		wantFlags  []string
		wantAbsent []string
	}{
		{
			name:      "filter + infer combined",
			header:    "output-128k-2025-02-19",
			body:      map[string]any{"context_management": map[string]any{}},
			allowed:   BedrockAllowedBetaFlags,
			wantLen:   2,
			wantFlags: []string{"output-128k-2025-02-19", "context-management-2025-06-27"},
		},
		{
			name:      "dedup: user already has inferred flag",
			header:    "context-management-2025-06-27",
			body:      map[string]any{"context_management": map[string]any{}},
			allowed:   BedrockAllowedBetaFlags,
			wantLen:   1,
			wantFlags: []string{"context-management-2025-06-27"},
		},
		{
			name:       "inferred flag not in whitelist is skipped",
			header:     "",
			body:       map[string]any{"context_management": map[string]any{}},
			allowed:    map[string]struct{}{"output-128k-2025-02-19": {}},
			wantLen:    0,
			wantAbsent: []string{"context-management-2025-06-27"},
		},
		{
			name:       "unknown user flags filtered, inferred added",
			header:     "unknown-flag,context-1m-2025-08-07",
			body:       map[string]any{"context_management": map[string]any{}},
			allowed:    BedrockAllowedBetaFlags,
			wantLen:    2,
			wantFlags:  []string{"context-1m-2025-08-07", "context-management-2025-06-27"},
			wantAbsent: []string{"unknown-flag"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeBetaFlags(tt.header, tt.body, tt.allowed)
			if len(got) != tt.wantLen {
				t.Errorf("len: got %d (%v), want %d", len(got), got, tt.wantLen)
				return
			}
			flagSet := make(map[string]bool)
			for _, f := range got {
				flagSet[f] = true
			}
			for _, f := range tt.wantFlags {
				if !flagSet[f] {
					t.Errorf("expected flag %q not found in %v", f, got)
				}
			}
			for _, f := range tt.wantAbsent {
				if flagSet[f] {
					t.Errorf("unexpected flag %q found in %v", f, got)
				}
			}
		})
	}
}

func TestMarshalBetaFlags(t *testing.T) {
	got, err := MarshalBetaFlags(nil)
	if err != nil || got != nil {
		t.Errorf("nil input: got %v, err %v", got, err)
	}

	got, err = MarshalBetaFlags([]string{"a", "b"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(got) != `["a","b"]` {
		t.Errorf("got %s, want [\"a\",\"b\"]", string(got))
	}
}

func TestFilterBetaHeaderByMode(t *testing.T) {
	tests := []struct {
		name   string
		header string
		mode   model.BetaFilterMode
		want   string
	}{
		{
			name:   "empty mode passes through",
			header: "claude-code-20250219,context-management-2025-06-27",
			mode:   model.BetaFilterNone,
			want:   "claude-code-20250219,context-management-2025-06-27",
		},
		{
			name:   "empty header returns empty",
			header: "",
			mode:   model.BetaFilterBedrock,
			want:   "",
		},
		{
			name:   "bedrock mode filters unsupported flags",
			header: "claude-code-20250219,redact-thinking-2026-02-12,context-management-2025-06-27,prompt-caching-scope-2026-01-05",
			mode:   model.BetaFilterBedrock,
			want:   "context-management-2025-06-27",
		},
		{
			name:   "vertex mode filters unsupported flags",
			header: "claude-code-20250219,context-management-2025-06-27,effort-2025-11-24",
			mode:   model.BetaFilterVertex,
			want:   "context-management-2025-06-27",
		},
		{
			name:   "bedrock_vertex mode uses intersection",
			header: "context-management-2025-06-27,interleaved-thinking-2025-05-14,prompt-caching-2024-07-31",
			mode:   model.BetaFilterBedrockVertex,
			want:   "context-management-2025-06-27,interleaved-thinking-2025-05-14",
		},
		{
			name:   "all flags filtered returns empty string",
			header: "claude-code-20250219,redact-thinking-2026-02-12",
			mode:   model.BetaFilterBedrock,
			want:   "",
		},
		{
			name:   "unknown mode passes through",
			header: "claude-code-20250219",
			mode:   model.BetaFilterMode("invalid"),
			want:   "claude-code-20250219",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FilterBetaHeaderByMode(tt.header, tt.mode)
			if got != tt.want {
				t.Errorf("FilterBetaHeaderByMode(%q, %q) = %q, want %q", tt.header, tt.mode, got, tt.want)
			}
		})
	}
}

func TestBedrockVertexAllowedBetaFlags(t *testing.T) {
	for k := range BedrockVertexAllowedBetaFlags {
		if _, ok := BedrockAllowedBetaFlags[k]; !ok {
			t.Errorf("intersection flag %q not in BedrockAllowedBetaFlags", k)
		}
		if _, ok := VertexAllowedBetaFlags[k]; !ok {
			t.Errorf("intersection flag %q not in VertexAllowedBetaFlags", k)
		}
	}
	if len(BedrockVertexAllowedBetaFlags) == 0 {
		t.Error("intersection should not be empty")
	}
}
