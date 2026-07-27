package kling

import (
	"strings"
	"testing"
)

func TestGenerateTaskID(t *testing.T) {
	// 生成多个任务ID，确保它们是唯一的
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := GenerateTaskID()
		
		// 检查格式
		if !strings.HasPrefix(id, "kling_") {
			t.Errorf("GenerateTaskID() = %v, want prefix 'kling_'", id)
		}
		
		// 检查长度（kling_ + 32个十六进制字符）
		if len(id) != 38 {
			t.Errorf("GenerateTaskID() length = %v, want 38", len(id))
		}
		
		// 检查唯一性
		if ids[id] {
			t.Errorf("GenerateTaskID() generated duplicate ID: %v", id)
		}
		ids[id] = true
	}
}

func TestDetermineRequestType(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/kling/v1/videos/text2video", RequestTypeText2Video},
		{"/kling/v1/videos/omni-video", RequestTypeOmniVideo},
		{"/kling/v1/videos/image2video", RequestTypeImage2Video},
		{"/kling/v1/videos/multi-image2video", RequestTypeMultiImage2Video},
		{"/kling/v1/videos/unknown", ""},
		{"/other/path", ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := DetermineRequestType(tt.path)
			if got != tt.want {
				t.Errorf("DetermineRequestType(%v) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestGetPromptFromRequest(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]interface{}
		want   string
	}{
		{
			name:   "有prompt",
			params: map[string]interface{}{"prompt": "测试提示词"},
			want:   "测试提示词",
		},
		{
			name:   "无prompt",
			params: map[string]interface{}{"other": "value"},
			want:   "",
		},
		{
			name:   "空params",
			params: map[string]interface{}{},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetPromptFromRequest(tt.params)
			if got != tt.want {
				t.Errorf("GetPromptFromRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetDurationFromRequest(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]interface{}
		want   int
	}{
		{
			name:   "float64类型",
			params: map[string]interface{}{"duration": float64(10)},
			want:   10,
		},
		{
			name:   "int类型",
			params: map[string]interface{}{"duration": 15},
			want:   15,
		},
		{
			name:   "无duration",
			params: map[string]interface{}{"other": "value"},
			want:   5, // 默认值
		},
		{
			name:   "空params",
			params: map[string]interface{}{},
			want:   5, // 默认值
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetDurationFromRequest(tt.params)
			if got != tt.want {
				t.Errorf("GetDurationFromRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetAspectRatioFromRequest(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]interface{}
		want   string
	}{
		{
			name:   "有aspect_ratio",
			params: map[string]interface{}{"aspect_ratio": "9:16"},
			want:   "9:16",
		},
		{
			name:   "无aspect_ratio",
			params: map[string]interface{}{"other": "value"},
			want:   "16:9", // 默认值
		},
		{
			name:   "空params",
			params: map[string]interface{}{},
			want:   "16:9", // 默认值
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetAspectRatioFromRequest(tt.params)
			if got != tt.want {
				t.Errorf("GetAspectRatioFromRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetModelNameFromRequest(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]interface{}
		want   string
	}{
		{
			// Kling API 用的是 model_name，不是 model。
			// Adaptor.ConvertRequest 会把 gin context 里的 model 注入成
			// model_name 后 delete(requestBody, "model")，见 adaptor.go:73-82。
			name:   "有model_name",
			params: map[string]interface{}{"model_name": "kling-v1-5-std"},
			want:   "kling-v1-5-std",
		},
		{
			// 只有 model 字段时必须取不到 —— 这条固定 Kling 的字段契约，
			// 防止有人把 GetModelNameFromRequest 改回读 model 而不改 adaptor。
			name:   "只有model字段（Kling 不使用）",
			params: map[string]interface{}{"model": "kling-v1-5-std"},
			want:   "",
		},
		{
			name:   "无model_name",
			params: map[string]interface{}{"other": "value"},
			want:   "",
		},
		{
			name:   "空params",
			params: map[string]interface{}{},
			want:   "",
		},
		{
			// 类型不匹配时不能 panic
			name:   "model_name非字符串",
			params: map[string]interface{}{"model_name": 123},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetModelNameFromRequest(tt.params)
			if got != tt.want {
				t.Errorf("GetModelNameFromRequest() = %v, want %v", got, tt.want)
			}
		})
	}
}

