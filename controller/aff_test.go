package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestReadAffCode 验证「查询参数优先、session 兜底」的取值优先级。
//
// 两个通道各覆盖一条注册流程：POST /api/{provider}/login 是前端直传，
// 没调过 /api/oauth/state 所以 session 里没有邀请码；
// GET /api/oauth/{provider}/callback 的回调 URL 由 OAuth 提供商拼装，
// 前端无法附加参数，只能走 session。
func TestReadAffCode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name        string
		query       string
		sessionCode any
		want        string
	}{
		{"两者都无", "", nil, ""},
		{"只有查询参数", "?aff_code=q1", nil, "q1"},
		{"只有session", "", "s1", "s1"},
		{"两者都有时参数优先", "?aff_code=q1", "s1", "q1"},
		{"参数为空串时回退session", "?aff_code=", "s1", "s1"},
		{"session里是非字符串类型时忽略", "", 12345, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/x"+tt.query, nil)
			// 传入假的 session 取值函数，避免为了测这层取值逻辑
			// 而引入真实的 cookie store
			got := readAffCode(c, func(key interface{}) interface{} {
				if key == affCodeSessionKey {
					return tt.sessionCode
				}
				return nil
			})
			if got != tt.want {
				t.Errorf("readAffCode() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestReadAffCodeNilSession getSession 为 nil 时不能 panic。
func TestReadAffCodeNilSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
	if got := readAffCode(c, nil); got != "" {
		t.Errorf("readAffCode() = %q, want empty", got)
	}
}

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
