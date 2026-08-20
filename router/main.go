package router

import (
	"embed"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/middleware"
)

func SetRouter(router *gin.Engine, buildFS embed.FS) {
	// CORS 只注册一次，由它内部按路径分派严格版 / 宽松版。
	//
	// 三个 Set*Router 拿到的是同一个 *gin.Engine，各自 router.Use(CORS())
	// 会让每个请求跑过三个 CORS 中间件：非预检下后者覆盖前者的 header，
	// 预检下 rs/cors 直接 abort，于是第一个注册的决定了所有路径的预检结果
	// —— 给 /api/* 用的 origin 白名单会连带把 /v1/* 的浏览器调用者挡掉。
	router.Use(middleware.CORS())

	SetApiRouter(router)
	SetDashboardRouter(router)
	SetRelayRouter(router)
	SetWebRouter(router, buildFS)
}
