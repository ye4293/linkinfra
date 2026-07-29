package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestAffParsePagingBoundaries 分页参数的边界与恶意输入。
//
// 这几个接口是登录用户可直接调用的，参数完全由客户端控制。要防的是：
//   - 负数 / 0 导致 offset 为负（部分数据库会报错或返回意外结果）
//   - 超大 pagesize 导致一次查出全表（内存打爆 / 慢查询拖垮 DB）
//   - 非数字导致解析出 0 后走到未定义分支
func TestAffParsePagingBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name         string
		query        string
		wantPage     int
		wantPageSize int
	}{
		{"无参数走默认", "", 1, 10},
		{"正常值", "?page=3&pagesize=20", 3, 20},
		{"page 为 0 归一为 1", "?page=0", 1, 10},
		{"page 为负归一为 1", "?page=-5", 1, 10},
		{"pagesize 为 0 走默认", "?pagesize=0", 1, 10},
		{"pagesize 为负走默认", "?pagesize=-20", 1, 10},
		{"非数字走默认", "?page=abc&pagesize=xyz", 1, 10},
		// 关键：超大 pagesize 必须被限制，否则一次请求就能查出全表
		{"pagesize 超大必须被限制", "?pagesize=1000000", 1, affMaxPageSize},
		{"pagesize 刚好在上限内", "?pagesize=100", 1, 100},
		// page 超大不会查出数据（offset 太大），但不能导致 offset 溢出
		{"page 超大不溢出", "?page=999999999", 999999999, 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/x"+tt.query, nil)
			page, pageSize := affParsePaging(c)
			if page != tt.wantPage {
				t.Errorf("page = %d, want %d", page, tt.wantPage)
			}
			if pageSize != tt.wantPageSize {
				t.Errorf("pageSize = %d, want %d", pageSize, tt.wantPageSize)
			}
		})
	}
}

// TestMaskUsernameEmoji 脱敏对多字节字符与代理对的处理。
//
// maskUsername 按 rune 切分。Go 的 rune 是 code point，emoji 中的
// ZWJ 序列（如 👨‍👩‍👧）由多个 code point 组成，按 rune 切会把它拆开 ——
// 结果可能显示成半个家庭 emoji。这不是安全问题（脱敏目的已达成），
// 但值得知道行为。
func TestMaskUsernameEmoji(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"单个emoji", "😀"},
		{"emoji加字母", "a😀b"},
		{"纯emoji串", "😀😃😄"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := maskUsername(tt.in)
			// 只断言不 panic、不返回空（除非输入为空）、且确实做了遮蔽
			if tt.in != "" && got == "" {
				t.Errorf("maskUsername(%q) 返回空串", tt.in)
			}
			if got == tt.in && len([]rune(tt.in)) > 0 {
				t.Errorf("maskUsername(%q) = %q 未做任何遮蔽", tt.in, got)
			}
			t.Logf("maskUsername(%q) = %q", tt.in, got)
		})
	}
}
