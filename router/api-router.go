package router

import (
	"github.com/songquanpeng/one-api/controller"
	"github.com/songquanpeng/one-api/middleware"

	"github.com/gin-contrib/gzip"
	"github.com/gin-gonic/gin"
)

func SetApiRouter(router *gin.Engine) {
	apiRouter := router.Group("/api")
	apiRouter.Use(gzip.Gzip(gzip.DefaultCompression))
	apiRouter.Use(middleware.GlobalAPIRateLimit())
	{
		apiRouter.GET("/status", controller.GetStatus)
		apiRouter.GET("/models", middleware.UserAuth(), controller.DashboardListModels)
		apiRouter.GET("/notice", controller.GetNotice)
		apiRouter.GET("/about", controller.GetAbout)
		apiRouter.GET("/home_page_content", controller.GetHomePageContent)
		apiRouter.GET("/model-plaza", controller.GetModelPlaza)
		apiRouter.GET("/model-plaza/metrics/all", controller.GetAllModelMetricsMini)
		apiRouter.GET("/model-plaza/metrics/detail", middleware.TryUserAuth(), controller.GetModelMetricsDetail)
		apiRouter.GET("/model-plaza/metrics/timeseries", controller.GetModelMetricsTimeSeries)
		apiRouter.POST("/stripe/webhook", middleware.GlobalWebRateLimit(), controller.StripeWebhook)
		apiRouter.GET("/verification", middleware.CriticalRateLimit(), middleware.TurnstileCheck(), controller.SendEmailVerification)
		apiRouter.GET("/reset_password", middleware.CriticalRateLimit(), middleware.TurnstileCheck(), controller.SendPasswordResetEmail)
		apiRouter.POST("/user/reset", middleware.CriticalRateLimit(), controller.ResetPassword)
		// OAuth 只保留前端（next-auth）直传的这两条：next-auth 自己完成与
		// GitHub / Google 的跳转与 code 交换，后端只接收已认证的用户信息。
		// 老的重定向流程（/oauth/state、/oauth/{provider}、
		// /oauth/{provider}/callback）已随之下线 —— 它往 github_id 存的是
		// GitHub 登录名，而这条路径存的是 provider 数字 id，同一列两种语义，
		// 并存会让同一个 GitHub 账号在两条路径下被认成两个人。
		apiRouter.POST("/github/login", middleware.CriticalRateLimit(), controller.GitHubLogin)
		apiRouter.POST("/google/login", middleware.CriticalRateLimit(), controller.GoogleLogin)
		apiRouter.POST("/oauth/provider-config", middleware.CriticalRateLimit(), controller.GetOAuthProviderConfig)
		apiRouter.GET("/oauth/email/bind", middleware.CriticalRateLimit(), middleware.UserAuth(), controller.EmailBind)

		userRoute := apiRouter.Group("/user")
		{
			userRoute.POST("/register", middleware.CriticalRateLimit(), middleware.TurnstileCheck(), controller.Register)
			userRoute.POST("/login", middleware.CriticalRateLimit(), controller.Login)
			userRoute.GET("/logout", controller.Logout)

			selfRoute := userRoute.Group("/")
			selfRoute.Use(middleware.UserAuth())
			{
				selfRoute.GET("/self", controller.GetSelf)
				selfRoute.PUT("/self", controller.UpdateSelf)
				selfRoute.DELETE("/self", controller.DeleteSelf)
				selfRoute.GET("/token", controller.GenerateAccessToken)
				selfRoute.GET("/aff", controller.GetAffCode)
				selfRoute.GET("/aff/stats", controller.GetAffStats)
				selfRoute.GET("/aff/records", controller.GetAffCommissionRecords)
				selfRoute.GET("/invitees", controller.GetInvitees)
				selfRoute.POST("/topup", controller.TopUp)
				selfRoute.GET("/topup/self", controller.GetUserTopUps)
				selfRoute.POST("/stripe/pay", middleware.CriticalRateLimit(), controller.RequestStripePay)
			}

			adminRoute := userRoute.Group("/")
			adminRoute.Use(middleware.AdminAuth())
			{
				adminRoute.GET("/", controller.GetAllUsers)
				adminRoute.GET("/search", controller.SearchUsers)
				adminRoute.GET("/:id", controller.GetUser)
				adminRoute.POST("/", controller.CreateUser)
				adminRoute.POST("/manage", controller.ManageUser)
				adminRoute.PUT("/", controller.UpdateUser)
				adminRoute.POST("/batchdelete", controller.BatchDelteUser)
				adminRoute.DELETE("/:id", controller.DeleteUser)
				adminRoute.GET("/topup", controller.GetUserTopUps)
				adminRoute.POST("/topup/complete", controller.CompleteTopUp)
			}
		}
		affRoute := apiRouter.Group("/aff")
		affRoute.Use(middleware.AdminAuth())
		{
			affRoute.GET("/report", controller.GetAffReport)
		}
		optionRoute := apiRouter.Group("/option")
		optionRoute.Use(middleware.RootAuth())
		{
			optionRoute.GET("/", controller.GetOptions)
			optionRoute.PUT("/", controller.UpdateOption)
		}

		// 模型价格管理相关路由（需要管理员权限）
		pricingRoute := apiRouter.Group("/pricing")
		pricingRoute.Use(middleware.AdminAuth())
		{
			pricingRoute.GET("/models", controller.GetModelPrices)              // 获取所有模型价格信息
			pricingRoute.GET("/unset", controller.GetUnsetRatioModels)          // 获取未设置倍率的模型
			pricingRoute.PUT("/model", controller.UpdateModelRatio)             // 更新单个模型倍率
			pricingRoute.PUT("/models/batch", controller.BatchUpdateModelRatio) // 批量更新模型倍率
		}

		// 测试通知相关路由（需要管理员权限）
		testRoute := apiRouter.Group("/test")
		testRoute.Use(middleware.AdminAuth())
		{
			testRoute.POST("/smtp", controller.TestSMTP)
			testRoute.POST("/feishu", controller.TestFeishuWebhook)
		}

		// 分组等级配置管理路由（需要管理员权限）
		groupConfigRoute := apiRouter.Group("/group-config")
		groupConfigRoute.Use(middleware.AdminAuth())
		{
			groupConfigRoute.GET("/", controller.GetAllGroupConfigs)
			groupConfigRoute.POST("/", controller.CreateGroupConfigHandler)
			groupConfigRoute.PUT("/", controller.UpdateGroupConfigHandler)
			groupConfigRoute.DELETE("/:id", controller.DeleteGroupConfigHandler)
		}

		channelRoute := apiRouter.Group("/channel")
		channelRoute.Use(middleware.AdminAuth())
		{
			channelRoute.GET("/", controller.GetAllChannels)
			channelRoute.GET("/search", controller.SearchChannels)
			channelRoute.GET("/models", controller.ListModels)
			channelRoute.GET("/models_by_id", controller.GetChannelModelsById)
			channelRoute.GET("/types", controller.ListTypes)
			channelRoute.GET("/model/details", controller.ListModelDetails)
			// 获取上游模型列表（必须在 /:id 之前注册）
			channelRoute.GET("/fetch_models/:id", controller.FetchUpstreamModels)
			channelRoute.POST("/fetch_models", controller.FetchModels)
			channelRoute.GET("/:id", controller.GetChannel)
			channelRoute.GET("/test", controller.TestChannels)
			channelRoute.POST("/test/:id", controller.TestChannel)
			channelRoute.GET("/update_balance", controller.UpdateAllChannelsBalance)
			channelRoute.GET("/update_balance/:id", controller.UpdateChannelBalance)
			channelRoute.GET("/clear_quota/:id", controller.ClearChannelQuota)
			channelRoute.POST("/", controller.AddChannel)
			channelRoute.PUT("/", controller.UpdateChannel)
			channelRoute.POST("/batchdelete", controller.BatchDelteChannel)
			channelRoute.POST("/disabled", controller.BatchDisableChannel)
			channelRoute.DELETE("/disabled", controller.BatchEnableChannel)
			channelRoute.DELETE("/:id", controller.DeleteChannel)
			channelRoute.POST("/copy/:id", controller.CopyChannel)

			// 多Key管理相关路由
			channelRoute.GET("/:id/keys/stats", controller.GetChannelKeyStats)
			channelRoute.GET("/:id/keys/details", controller.GetChannelKeyDetails)
			channelRoute.GET("/:id/keys/health", controller.GetChannelKeyHealthStatus)
			channelRoute.POST("/keys/import", controller.BatchImportChannelKeys)
			channelRoute.POST("/keys/toggle", controller.ToggleChannelKey)
			channelRoute.POST("/keys/batch-toggle", controller.BatchToggleChannelKeys)
			channelRoute.POST("/keys/batch-toggle-by-batch", controller.ToggleChannelKeysByBatch)
			channelRoute.POST("/keys/retry", controller.RetryChannelKey)
			channelRoute.POST("/keys/delete-disabled", controller.DeleteDisabledKeys)
			channelRoute.POST("/keys/fix-status", controller.FixMultiKeyChannelStatus)
			channelRoute.PUT("/multi-key/settings", controller.UpdateChannelMultiKeySettings)
			channelRoute.POST("/upstream_updates/detect", controller.DetectChannelUpstreamModelUpdates)
			channelRoute.POST("/upstream_updates/apply", controller.ApplyChannelUpstreamModelUpdates)
			channelRoute.POST("/upstream_updates/detect_all", controller.DetectAllChannelUpstreamModelUpdates)
			channelRoute.POST("/upstream_updates/apply_all", controller.ApplyAllChannelUpstreamModelUpdates)
		}
		affinityRoute := apiRouter.Group("/affinity")
		affinityRoute.Use(middleware.AdminAuth())
		{
			affinityRoute.GET("/config", controller.GetAffinityConfig)
			affinityRoute.PUT("/config", controller.UpdateAffinityConfig)
			affinityRoute.GET("/cache", controller.GetAffinityCacheStats)
			affinityRoute.DELETE("/cache", controller.ClearAffinityCache)
		}
		tokenRoute := apiRouter.Group("/token")
		tokenRoute.Use(middleware.UserAuth())
		{
			tokenRoute.GET("/", controller.GetAllTokens)
			tokenRoute.GET("/search", controller.SearchTokens)
			tokenRoute.GET("/:id", controller.GetToken)
			tokenRoute.POST("/", controller.AddToken)
			tokenRoute.PUT("/", controller.UpdateToken)
			tokenRoute.POST("/batchdelete", controller.BatchDeleteToken)
			tokenRoute.DELETE("/:id", controller.DeleteToken)
		}
		redemptionRoute := apiRouter.Group("/redemption")
		redemptionRoute.Use(middleware.AdminAuth())
		{
			redemptionRoute.GET("/", controller.GetAllRedemptions)
			redemptionRoute.GET("/search", controller.SearchRedemptions)
			redemptionRoute.GET("/:id", controller.GetRedemption)
			redemptionRoute.POST("/", controller.AddRedemption)
			redemptionRoute.PUT("/", controller.UpdateRedemption)
			redemptionRoute.POST("/batchdelete", controller.BatchDeleteRedemption)
			redemptionRoute.DELETE("/:id", controller.DeleteRedemption)
		}
		logRoute := apiRouter.Group("/log")
		logRoute.GET("/", middleware.AdminAuth(), controller.GetAllLogs)
		logRoute.DELETE("/", middleware.AdminAuth(), controller.DeleteHistoryLogs)
		logRoute.GET("/stat", middleware.AdminAuth(), controller.GetLogsStat)
		logRoute.GET("/stat/performance", middleware.AdminAuth(), controller.GetLogsPerformanceStat)
		logRoute.GET("/self/stat", middleware.UserAuth(), controller.GetLogsSelfStat)
		logRoute.GET("/self/stat/performance", middleware.UserAuth(), controller.GetLogsSelfPerformanceStat)
		logRoute.GET("/search", middleware.AdminAuth(), controller.SearchAllLogs)
		logRoute.GET("/self", middleware.UserAuth(), controller.GetUserLogs)
		logRoute.GET("/self/search", middleware.UserAuth(), controller.SearchUserLogs)
		groupRoute := apiRouter.Group("/group")
		groupRoute.Use(middleware.AdminAuth())
		{
			groupRoute.GET("/", controller.GetGroups)
		}
	}
	cryptoaiRoute := apiRouter.Group("/")
	// cryptoaiRoute.GET("/pay/crypt/get_qrcode", middleware.UserAuth(), middleware.GlobalAPIRateLimit(), controller.GetQrcode)
	cryptoaiRoute.GET("/pay/get_qrcode", middleware.UserAuth(), middleware.GlobalAPIRateLimit(), controller.GetQrcode)
	cryptoaiRoute.GET("/pay/get_channel", middleware.UserAuth(), middleware.GlobalAPIRateLimit(), controller.GetPayChannel)
	cryptoaiRoute.GET("/crypt/callback", middleware.CryptCallbackAuth(), controller.CryptCallback)

	orderRoute := apiRouter.Group("/order")
	orderRoute.GET("/", middleware.AdminAuth(), controller.GetAllOrders)
	orderRoute.GET("/self", middleware.UserAuth(), controller.GetUserOrders)

	dashboardRoute := apiRouter.Group("/dashboard")
	dashboardRoute.GET("/", middleware.AdminAuth(), controller.GetAdminDashboard)
	dashboardRoute.GET("/graph", middleware.AdminAuth(), controller.GetAllGraph)
	dashboardRoute.GET("/stats", middleware.AdminAuth(), controller.GetAdminDashboardStats)
	dashboardRoute.GET("/self", middleware.UserAuth(), controller.GetUserDashboard)
	dashboardRoute.GET("/graph/self", middleware.UserAuth(), controller.GetUserGraph)
	dashboardRoute.GET("/stats/self", middleware.UserAuth(), controller.GetUserDashboardStats)

	videoRoute := apiRouter.Group("/video")
	videoRoute.GET("/self", middleware.UserAuth(), controller.GetUserVideos)
	videoRoute.GET("/", middleware.AdminAuth(), controller.GetAllVideos)

	imageRoute := apiRouter.Group("/image")
	imageRoute.GET("/self", middleware.UserAuth(), controller.GetUserImages)
	imageRoute.GET("/", middleware.AdminAuth(), controller.GetALLImages)

	chargeRoute := apiRouter.Group("/charge")
	chargeRoute.GET("/get_config", middleware.UserAuth(), middleware.GlobalWebRateLimit(), controller.GetChargeConfigs)
	chargeRoute.POST("/create_order", middleware.UserAuth(), middleware.GlobalWebRateLimit(), controller.CreateChargeOrder)
	chargeRoute.GET("/get_order", middleware.UserAuth(), middleware.GlobalWebRateLimit(), controller.GetUserChargeOrders)
	chargeRoute.POST("/stripe_callback", middleware.GlobalWebRateLimit(), controller.StripeCallback)

	fileRoute := apiRouter.Group("/files")
	{
		fileRoute.POST("/", middleware.UserAuth(), middleware.GlobalWebRateLimit(), controller.UploadFile)
		fileRoute.DELETE("/", middleware.UserAuth(), middleware.GlobalWebRateLimit(), controller.DeletiFile)
	}
}
