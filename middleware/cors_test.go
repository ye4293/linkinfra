package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// withAllowedOrigins 临时替换白名单，测试结束还原。
func withAllowedOrigins(t *testing.T, origins []string) {
	t.Helper()
	orig := allowedOrigins
	allowedOrigins = origins
	t.Cleanup(func() { allowedOrigins = orig })
}

// newCORSTestRouter 起一个挂了 CORS 的 engine，注册两条代表性路径。
func newCORSTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(CORS())
	r.GET("/api/user/self", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.POST("/v1/chat/completions", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return r
}

func do(r *gin.Engine, method, path, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if method == http.MethodOptions {
		req.Header.Set("Access-Control-Request-Method", "POST")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestCORSRestrictsCredentialedApiToAllowlist /api/* 走 cookie 认证，必须只
// 允许白名单内的 origin 带凭证读取。
//
// 原实现是 AllowOriginFunc 恒 true + AllowCredentials: true，rs/cors 会把
// Origin 原样回显（不是 `*`，浏览器会接受），于是任何网站都能带着已登录用户
// 的 cookie 读走他的 key、额度、日志。
func TestCORSRestrictsCredentialedApiToAllowlist(t *testing.T) {
	withAllowedOrigins(t, []string{"https://app.example.com"})
	r := newCORSTestRouter()

	allowed := do(r, http.MethodGet, "/api/user/self", "https://app.example.com")
	if got := allowed.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("白名单内的 origin 被拒了: ACAO = %q", got)
	}
	if got := allowed.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("白名单内的 origin 没拿到 Allow-Credentials: %q（管理后台会登不上）", got)
	}

	evil := do(r, http.MethodGet, "/api/user/self", "https://evil.example.net")
	if got := evil.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("白名单外的 origin 拿到了 ACAO = %q —— 任意网站可读已登录用户数据", got)
	}
}

// TestCORSKeepsPublicApiOpenWithoutCredentials /v1/* 走 Bearer token，浏览器
// 不会自动附 token，所以 origin 保持开放；但必须**不带** credentials。
//
// 这是个 API 网关，用户在自己网页里直接调 /v1/chat/completions 是正常用法，
// 收紧 origin 会直接打断他们。
func TestCORSKeepsPublicApiOpenWithoutCredentials(t *testing.T) {
	withAllowedOrigins(t, []string{"https://app.example.com"})
	r := newCORSTestRouter()

	// 一个完全不在白名单里的第三方站点
	resp := do(r, http.MethodPost, "/v1/chat/completions", "https://someones-app.dev")
	if got := resp.Header().Get("Access-Control-Allow-Origin"); got == "" {
		t.Error("/v1/* 对第三方 origin 返回了空 ACAO —— 浏览器里调 API 的用户会被打断")
	}
	if got := resp.Header().Get("Access-Control-Allow-Credentials"); got == "true" {
		t.Error("/v1/* 不该带 Allow-Credentials（Bearer 认证不需要 cookie）")
	}
}

// TestCORSPreflightDispatchesByPath 预检请求是这次结构调整的关键。
//
// rs/cors 在 OPTIONS 上会 abort，所以如果给每棵路由树各挂一个 CORS
// （它们都注册到同一个 engine 上），第一个注册的会决定**所有**路径的预检
// 结果 —— /api/* 的严格白名单会连带把 /v1/* 的浏览器调用者挡掉。
func TestCORSPreflightDispatchesByPath(t *testing.T) {
	withAllowedOrigins(t, []string{"https://app.example.com"})
	r := newCORSTestRouter()

	// /v1/* 的预检：第三方 origin 也该放行
	v1 := do(r, http.MethodOptions, "/v1/chat/completions", "https://someones-app.dev")
	if got := v1.Header().Get("Access-Control-Allow-Origin"); got == "" {
		t.Error("/v1/* 的预检把第三方 origin 挡掉了 —— 严格版泄漏到了公共路径")
	}

	// /api/* 的预检：白名单外必须挡
	api := do(r, http.MethodOptions, "/api/user/self", "https://evil.example.net")
	if got := api.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("/api/* 的预检放行了白名单外的 origin: %q", got)
	}
}

// TestCORSFallsBackToPermissiveWhenUnset 未配置 ALLOWED_ORIGINS 时退回宽松，
// 不能把管理后台整体锁死 —— 那比维持现状更糟。
func TestCORSFallsBackToPermissiveWhenUnset(t *testing.T) {
	withAllowedOrigins(t, nil)
	r := newCORSTestRouter()

	resp := do(r, http.MethodGet, "/api/user/self", "https://anything.example.net")
	if got := resp.Header().Get("Access-Control-Allow-Origin"); got == "" {
		t.Error("未配置白名单时把 /api/* 锁死了，管理后台会不可用")
	}
}

// TestIsCredentialedPath /api/* 与裸 /api 走严格版，其余走宽松版。
func TestIsCredentialedPath(t *testing.T) {
	cases := map[string]bool{
		"/api":                  true,
		"/api/":                 true,
		"/api/user/self":        true,
		"/v1/chat/completions":  false,
		"/v1/dashboard/billing": false,
		"/dashboard/billing":    false,
		"/":                     false,
		// 不能被前缀相似的路径骗过
		"/apifoo":     false,
		"/apifoo/bar": false,
	}
	for path, want := range cases {
		if got := isCredentialedPath(path); got != want {
			t.Errorf("isCredentialedPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestParseAllowedOrigins(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"空", "", nil},
		{"单个", "https://a.com", []string{"https://a.com"}},
		{"多个带空格", " https://a.com , https://b.com ", []string{"https://a.com", "https://b.com"}},
		// 人配置时很容易带尾斜杠，而浏览器发来的 Origin 头从不带
		{"去尾斜杠", "https://a.com/", []string{"https://a.com"}},
		{"转小写", "HTTPS://A.COM", []string{"https://a.com"}},
		{"忽略空项", "https://a.com,,https://b.com", []string{"https://a.com", "https://b.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseAllowedOrigins(tt.raw)
			if len(got) != len(tt.want) {
				t.Fatalf("parseAllowedOrigins(%q) = %v, want %v", tt.raw, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestIsOriginAllowedRejectsLookalikes 精确匹配，不能被相似域名骗过。
func TestIsOriginAllowedRejectsLookalikes(t *testing.T) {
	withAllowedOrigins(t, []string{"https://example.com"})

	cases := map[string]bool{
		"https://example.com":          true,
		"https://example.com/":         true, // 尾斜杠容错
		"HTTPS://EXAMPLE.COM":          true, // 大小写容错
		"https://evil-example.com":     false,
		"https://example.com.evil.net": false,
		"https://sub.example.com":      false, // 不做通配子域
		"http://example.com":           false, // scheme 不同
		"":                             false,
	}
	for origin, want := range cases {
		if got := isOriginAllowed(origin); got != want {
			t.Errorf("isOriginAllowed(%q) = %v, want %v", origin, got, want)
		}
	}
}
