package controller

import "testing"

func TestMaskUsername(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"空串", "", ""},
		{"单字符", "a", "*"},
		{"两字符", "ab", "**"},
		{"三字符", "abc", "a*c"},
		{"常见长度", "zhangsan", "z******n"},
		{"中文", "张三丰", "张*丰"},
		// 中文按 rune 处理，不能按 byte 切 —— 按 byte 会切出乱码
		{"中文长名", "王小明同学", "王***学"},
		// user张三 是 6 个 rune（u/s/e/r/张/三），保留首尾后中间 4 个
		{"中英混合", "user张三", "u****三"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maskUsername(tt.in); got != tt.want {
				t.Errorf("maskUsername(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
