package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestReadAffCode 验证从查询参数取邀请码。
//
// 早先这里还测「查询参数优先、session 兜底」的双通道优先级。session 通道
// 服务的是 GET /api/oauth/{provider}/callback 那条重定向流，该流程已随
// next-auth 接管跳转而下线，通道成为死代码并被删除，测试同步收敛。
func TestReadAffCode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{"无参数", "", ""},
		{"有参数", "?aff_code=q1", "q1"},
		{"参数为空串", "?aff_code=", ""},
		{"其他参数不影响", "?foo=bar", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/x"+tt.query, nil)
			if got := readAffCode(c); got != tt.want {
				t.Errorf("readAffCode() = %q, want %q", got, tt.want)
			}
		})
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
