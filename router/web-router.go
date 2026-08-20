package router

import (
	"embed"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/controller"
)

// SetWebRouter 注册根路径与非 API 路径的兜底响应。
//
// 后端不再对外服务内嵌的 React 控制台 UI（web/default）——只作为 API
// 服务暴露，避免管理界面被公网直接访问。根路径及任何非 /api、/v1 前缀
// 的未匹配路径，返回一段纯 JSON 欢迎信息（仿 https://api.openai.com/
// 根路径响应）；/api、/v1 前缀仍走 controller.RelayNotFound 返回
// OpenAI 风格的 404。
//
// buildFS 保留以兼容调用链，内嵌 UI 不再对外服务。
func SetWebRouter(router *gin.Engine, buildFS embed.FS) {
	_ = buildFS
	router.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.RequestURI, "/v1") || strings.HasPrefix(c.Request.RequestURI, "/api") {
			controller.RelayNotFound(c)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"message": "Welcome to the LinkInfra API!",
		})
	})
}
