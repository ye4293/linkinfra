package middleware

import (
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	cors "github.com/rs/cors/wrapper/gin"
	"github.com/songquanpeng/one-api/common/logger"
)

// allowedOrigins 是 /api/* 这类**带 cookie** 的接口允许的来源，
// 从环境变量 ALLOWED_ORIGINS 读，逗号分隔。
//
// 为什么需要它：/api/* 走 session cookie 认证，而原实现是
// AllowOriginFunc 恒 true + AllowCredentials: true。rs/cors 在这种配置下会把
// 请求的 Origin **原样回显**（而不是回 `*`），浏览器会接受这种组合 —— 于是
// 任何网站都能带着已登录用户的 cookie 去读 /api/*，把他的 API key、额度、
// 调用日志读走。回 `*` 反而会被浏览器拒掉，所以"原样回显"才是危险的那种。
//
// 只约束 /api/*：/v1/* 与 /dashboard/* 走 Bearer token（TokenAuth），浏览器
// 不会自动附上 token，CSRF 与凭证盗读都不成立；而那些接口本来就是给第三方
// 客户端调的，收紧 origin 会直接打断在浏览器里调 API 的用户。
var allowedOrigins = parseAllowedOrigins(os.Getenv("ALLOWED_ORIGINS"))

// parseAllowedOrigins 解析逗号分隔的 origin 列表。
//
// 归一化：去空白、去尾部斜杠、转小写 —— 浏览器发来的 Origin 头没有尾斜杠，
// 而人配置时很容易带上，不归一化会导致明明配了却匹配不上。
func parseAllowedOrigins(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		out = append(out, strings.ToLower(strings.TrimRight(item, "/")))
	}
	return out
}

// isOriginAllowed 精确匹配（大小写与尾斜杠不敏感）。
//
// 不做通配子域：`*.example.com` 这类写法很容易实现成前缀/后缀匹配，从而把
// evil-example.com 或 example.com.evil.net 一起放进来。需要多个子域就显式列出。
func isOriginAllowed(origin string) bool {
	normalized := strings.ToLower(strings.TrimRight(strings.TrimSpace(origin), "/"))
	if normalized == "" {
		return false
	}
	for _, allowed := range allowedOrigins {
		if normalized == allowed {
			return true
		}
	}
	return false
}

// CORS 是全局唯一的 CORS 中间件，按路径把请求分派给严格版或宽松版。
//
// 为什么是"一个中间件按路径分派"而不是"每棵路由树各挂一个"：
// SetApiRouter / SetDashboardRouter / SetRelayRouter 拿到的是**同一个**
// *gin.Engine，它们里面的 router.Use(...) 都是注册到全局的。挂三个 CORS
// 会让每个请求依次跑过三个：非预检请求下后者的 header 覆盖前者，而预检
// (OPTIONS) 请求下 rs/cors 会直接 abort —— 于是**第一个**注册的那个决定了
// 所有路径的预检结果。那意味着给 /api/* 用的严格 origin 白名单会连带管住
// /v1/*，把在浏览器里调 API 的第三方用户全部挡掉。
//
// 分派规则：/api/* 走 session cookie 认证 → 严格；其余（/v1/*、/dashboard/*）
// 走 Bearer token → 宽松。
func CORS() gin.HandlerFunc {
	strict := credentialedCORS()
	public := PublicAPICORS()

	return func(c *gin.Context) {
		if isCredentialedPath(c.Request.URL.Path) {
			strict(c)
			return
		}
		public(c)
	}
}

// isCredentialedPath 判断该路径是否走 session cookie 认证。
//
// 同时匹配 "/api" 本身：现在 /api 下只有子路由、裸 /api 会 404，所以这个
// 分支当前走不到；但万一以后有人在 /api 上挂了接口，默认落到宽松版会是个
// 静默的安全回退。
func isCredentialedPath(path string) bool {
	return path == "/api" || strings.HasPrefix(path, "/api/")
}

// credentialedCORS 是 /api/* 用的、带 cookie 的严格版本。
//
// ALLOWED_ORIGINS 未配置时退回旧的宽松行为并大声告警，**不** fail closed：
// 这里一旦配错，整个管理后台会立刻不可用，那比维持现状（一个既有问题）更糟。
// 与 OAUTH_LOGIN_SECRET 的取舍不同 —— 那个 fail closed 只影响 OAuth 登录
// 一个入口，且它防的是仅凭公开信息就能完成的账号接管。
func credentialedCORS() gin.HandlerFunc {
	if len(allowedOrigins) == 0 {
		logger.SysError("ALLOWED_ORIGINS is not set: /api/* accepts credentialed requests from ANY origin. " +
			"Any website can read a logged-in user's keys, quota and logs. " +
			"Set ALLOWED_ORIGINS to your frontend origin(s), comma separated.")
		return cors.New(cors.Options{
			AllowOriginFunc:  func(origin string) bool { return true },
			AllowCredentials: true,
			AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
			AllowedHeaders:   []string{"*"},
		})
	}

	logger.SysLog("CORS for /api/* restricted to: " + strings.Join(allowedOrigins, ", "))
	return cors.New(cors.Options{
		AllowOriginFunc:  isOriginAllowed,
		AllowCredentials: true,
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
	})
}

// PublicAPICORS 是 /v1/* 与 /dashboard/* 用的宽松版本。
//
// 这些接口走 Bearer token：浏览器不会自动附上 token，所以放开 origin 不构成
// 凭证盗读或 CSRF。关键区别是 **AllowCredentials: false** —— 明确不参与
// cookie 交换，即使有人往这些路径发带 cookie 的跨站请求，浏览器也拿不到响应。
//
// origin 保持开放是有意的：这是个 API 网关，用户在自己的网页里直接调
// /v1/chat/completions 是正常用法，收紧会直接打断他们。
func PublicAPICORS() gin.HandlerFunc {
	return cors.New(cors.Options{
		AllowOriginFunc:  func(origin string) bool { return true },
		AllowCredentials: false,
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
	})
}
