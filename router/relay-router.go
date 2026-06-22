package router

import (
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/controller"
	"github.com/songquanpeng/one-api/middleware"
)

func SetRelayRouter(router *gin.Engine) {
	router.Use(middleware.CORS())
	// https://platform.openai.com/docs/api-reference/introduction
	modelsRouter := router.Group("/v1/models")
	modelsRouter.Use(middleware.TokenAuth())
	{
		modelsRouter.GET("", controller.ListModels)
		modelsRouter.GET("/:model", controller.RetrieveModel)
	}
	relayV1Router := router.Group("/v1")
	relayV1Router.Use(middleware.RelayPanicRecover(), middleware.TokenAuth())
	{
		relayV1Router.POST("/files", controller.UploadFile)
		relayV1Router.GET("/video/generations/result", controller.RelayVideoResult)
	}

	// Sora 视频生成路由 - 需要 Distribute 中间件进行渠道选择
	soraRouter := router.Group("/v1")
	soraRouter.Use(middleware.RelayPanicRecover(), middleware.TokenAuth(), middleware.Distribute())
	{
		soraRouter.POST("/videos", controller.RelaySoraVideo)
		//soraRouter.POST("/videos/characters", controller.RelaySoraCharacter)
	}

	// Sora 视频操作路由（基于已有视频）- 不需要 Distribute 中间件，从请求体中的 video.id 查找原视频渠道
	soraVideoOpsRouter := router.Group("/v1")
	soraVideoOpsRouter.Use(middleware.RelayPanicRecover(), middleware.TokenAuth())
	{
		soraVideoOpsRouter.POST("/videos/:videoId/remix", controller.RelaySoraVideoRemix)
		soraVideoOpsRouter.POST("/videos/edits", controller.RelaySoraVideoEdit)
		soraVideoOpsRouter.POST("/videos/extensions", controller.RelaySoraVideoExtension)
	}

	// Sora 查询路由 - 不需要 Distribute 中间件
	soraResultRouter := router.Group("/v1")
	soraResultRouter.Use(middleware.RelayPanicRecover(), middleware.TokenAuth())
	{
		soraResultRouter.GET("/videos/characters/:characterId", controller.RelaySoraCharacterGet)
		soraResultRouter.GET("/videos/:videoId/content", controller.RelaySoraVideoContent)
		soraResultRouter.GET("/videos/:videoId", controller.RelaySoraVideoResult)
	}

	// Create separate router groups for POST and GET
	asyncImagePostRouter := router.Group("/v1/async")
	asyncImagePostRouter.Use(middleware.TokenAuth(), middleware.Distribute())
	{
		asyncImagePostRouter.POST("/images/generations", controller.RelayImageGenerateAsync)
	}

	asyncImageGetRouter := router.Group("/v1/async")
	asyncImageGetRouter.Use(middleware.TokenAuth())
	{
		asyncImageGetRouter.GET("/images/result", controller.RelayImageResult)
	}

	relayV1Router.Use(middleware.RelayPanicRecover(), middleware.TokenAuth(), middleware.Distribute())
	{
		relayV1Router.POST("/completions", controller.Relay)
		relayV1Router.POST("/chat/completions", controller.Relay)
		relayV1Router.POST("/edits", controller.Relay)
		relayV1Router.POST("/images/edits", controller.Relay)
		relayV1Router.POST("/images/variations", controller.RelayNotImplemented)
		relayV1Router.POST("/embeddings", controller.Relay)
		relayV1Router.POST("/engines/:model/embeddings", controller.Relay)
		relayV1Router.POST("/audio/transcriptions", controller.Relay)
		relayV1Router.POST("/audio/translations", controller.Relay)
		relayV1Router.POST("/audio/speech", controller.Relay)
		relayV1Router.GET("/files", controller.RelayNotImplemented)
		relayV1Router.DELETE("/files/:id", controller.RelayNotImplemented)
		relayV1Router.GET("/files/:id", controller.RelayNotImplemented)
		relayV1Router.GET("/files/:id/content", controller.RelayNotImplemented)
		relayV1Router.POST("/fine_tuning/jobs", controller.RelayNotImplemented)
		relayV1Router.GET("/fine_tuning/jobs", controller.RelayNotImplemented)
		relayV1Router.GET("/fine_tuning/jobs/:id", controller.RelayNotImplemented)
		relayV1Router.POST("/fine_tuning/jobs/:id/cancel", controller.RelayNotImplemented)
		relayV1Router.GET("/fine_tuning/jobs/:id/events", controller.RelayNotImplemented)
		relayV1Router.DELETE("/models/:model", controller.RelayNotImplemented)
		relayV1Router.POST("/moderations", controller.Relay)
		relayV1Router.POST("/assistants", controller.RelayNotImplemented)
		relayV1Router.GET("/assistants/:id", controller.RelayNotImplemented)
		relayV1Router.POST("/assistants/:id", controller.RelayNotImplemented)
		relayV1Router.DELETE("/assistants/:id", controller.RelayNotImplemented)
		relayV1Router.GET("/assistants", controller.RelayNotImplemented)
		relayV1Router.POST("/assistants/:id/files", controller.RelayNotImplemented)
		relayV1Router.GET("/assistants/:id/files/:fileId", controller.RelayNotImplemented)
		relayV1Router.DELETE("/assistants/:id/files/:fileId", controller.RelayNotImplemented)
		relayV1Router.GET("/assistants/:id/files", controller.RelayNotImplemented)
		relayV1Router.POST("/threads", controller.RelayNotImplemented)
		relayV1Router.GET("/threads/:id", controller.RelayNotImplemented)
		relayV1Router.POST("/threads/:id", controller.RelayNotImplemented)
		relayV1Router.DELETE("/threads/:id", controller.RelayNotImplemented)
		relayV1Router.POST("/threads/:id/messages", controller.RelayNotImplemented)
		relayV1Router.GET("/threads/:id/messages/:messageId", controller.RelayNotImplemented)
		relayV1Router.POST("/threads/:id/messages/:messageId", controller.RelayNotImplemented)
		relayV1Router.GET("/threads/:id/messages/:messageId/files/:filesId", controller.RelayNotImplemented)
		relayV1Router.GET("/threads/:id/messages/:messageId/files", controller.RelayNotImplemented)
		relayV1Router.POST("/threads/:id/runs", controller.RelayNotImplemented)
		relayV1Router.GET("/threads/:id/runs/:runsId", controller.RelayNotImplemented)
		relayV1Router.POST("/threads/:id/runs/:runsId", controller.RelayNotImplemented)
		relayV1Router.GET("/threads/:id/runs", controller.RelayNotImplemented)
		relayV1Router.POST("/threads/:id/runs/:runsId/submit_tool_outputs", controller.RelayNotImplemented)
		relayV1Router.POST("/threads/:id/runs/:runsId/cancel", controller.RelayNotImplemented)
		relayV1Router.GET("/threads/:id/runs/:runsId/steps/:stepId", controller.RelayNotImplemented)
		relayV1Router.GET("/threads/:id/runs/:runsId/steps", controller.RelayNotImplemented)
		relayV1Router.POST("/video/generations", controller.RelayVideoGenerate)
		relayV1Router.POST("/ocr", controller.RelayOcr)
		relayV1Router.POST("/images/imageToImage", controller.RelayRecraft)
		relayV1Router.POST("/images/inpaint", controller.RelayRecraft)
		relayV1Router.POST("/images/replaceBackground", controller.RelayRecraft)
		relayV1Router.POST("/images/vectorize", controller.RelayRecraft)
		relayV1Router.POST("/images/removeBackground", controller.RelayRecraft)
		relayV1Router.POST("/images/crispUpscale", controller.RelayRecraft)
		relayV1Router.POST("/images/creativeUpscale", controller.RelayRecraft)
		relayV1Router.POST("/styles", controller.RelayRecraft)
		relayV1Router.POST("/images/generations", controller.Relay)
		relayV1Router.POST("/messages", controller.RelayClaude)
		relayV1Router.POST("/messages/count_tokens", controller.RelayClaudeCountTokens) // Claude count_tokens 接口
		relayV1Router.POST("/responses/compact", controller.RelayResponse)
		relayV1Router.POST("/responses", controller.RelayResponse)
	}
	// Flux 生成路由：POST 需要 Distribute 选渠道
	relayFluxRouter := router.Group("/flux")
	relayFluxRouter.Use(middleware.RelayPanicRecover(), middleware.TokenAuth(), middleware.Distribute())
	{
		relayFluxRouter.POST("/v1/*model", controller.Relay)
	}

	// Flux 查询路由：渠道从 DB 按 task_id 解析，不需要 Distribute
	fluxResultRouter := router.Group("/flux")
	fluxResultRouter.Use(middleware.RelayPanicRecover(), middleware.TokenAuth())
	{
		fluxResultRouter.GET("/v1/get_result", controller.GetFlux)
	}

	// Flux 回调路由组 - 接收 Flux API 回调通知
	fluxCallbackRouter := router.Group("/flux/internal")
	{
		fluxCallbackRouter.POST("/callback", controller.HandleFluxCallback)
		fluxCallbackRouter.POST("/replicate/callback", controller.HandleReplicateCallback)
	}
	// 豆包API兼容路由组 - 支持原始豆包API路径格式
	doubaoApiRouter := router.Group("/api/v3/contents/generations")
	doubaoApiRouter.Use(middleware.TokenAuth()).GET("/tasks/:taskid", controller.RelayDouBaoVideoResultById)
	doubaoApiRouter.Use(middleware.TokenAuth(), middleware.Distribute()).POST("/tasks", controller.RelayVideoGenerate)

	// 豆包 v2 路由组 - 带 doubao/ 前缀，走独立 controller + 回调驱动
	// POST 创建任务：需要 TokenAuth + Distribute（渠道选择）
	doubaoV2CreateRouter := router.Group("/doubao/api/v3/contents/generations")
	doubaoV2CreateRouter.Use(middleware.TokenAuth(), middleware.Distribute()).POST("/tasks", controller.RelayDoubaoVideoCreate)
	// GET 查询任务：渠道信息来自 DB，不需要 Distribute；单独建 group 避免继承 POST 的中间件链
	doubaoV2QueryRouter := router.Group("/doubao/api/v3/contents/generations")
	doubaoV2QueryRouter.GET("/tasks/:taskId", controller.RelayDoubaoVideoResult)

	// 豆包回调路由（无需鉴权，来自豆包上游）
	doubaoCallbackRouter := router.Group("/doubao/internal")
	doubaoCallbackRouter.POST("/callback", controller.HandleDoubaoCallback)

	// Runway AI 路由组 - 在官方API路径中间插入"runway"
	// Runway API 使用直接代理模式，不需要 Distribute 中间件
	runwayRouter := router.Group("/runway/v1")
	runwayRouter.Use(middleware.RelayPanicRecover(), middleware.TokenAuth(), middleware.Distribute())
	{
		// 视频生成相关端点
		runwayRouter.POST("/image_to_video", controller.RelayRunway)
		runwayRouter.POST("/video_to_video", controller.RelayRunway)
		runwayRouter.POST("/text_to_image", controller.RelayRunway)
		runwayRouter.POST("/video_upscale", controller.RelayRunway)
		runwayRouter.POST("/character_performance", controller.RelayRunway)
	}

	// 不需要模型分发的查询端点
	runwayResultRouter := router.Group("/runway/v1")
	runwayResultRouter.Use(middleware.RelayPanicRecover(), middleware.TokenAuth())
	{
		runwayResultRouter.GET("/tasks/:taskId", controller.RelayRunwayResult)
	}

	// Kling AI 路由组 - 统一管理所有 Kling 接口
	klingRouter := router.Group("/kling/v1")
	klingRouter.Use(middleware.RelayPanicRecover(), middleware.TokenAuth(), middleware.Distribute())
	{
		// ========== 视频类接口 ==========
		// 现有视频接口
		klingRouter.POST("/videos/text2video", controller.RelayKlingVideo)
		klingRouter.POST("/videos/omni-video", controller.RelayKlingVideo)
		klingRouter.POST("/videos/image2video", controller.RelayKlingVideo)
		klingRouter.POST("/videos/multi-image2video", controller.RelayKlingVideo)
		klingRouter.POST("/videos/identify-face", controller.DoIdentifyFace)
		klingRouter.POST("/videos/advanced-lip-sync", controller.DoAdvancedLipSync)

		// 新增视频类接口（6个）
		klingRouter.POST("/videos/motion-control", controller.RelayKlingVideo)
		klingRouter.POST("/videos/multi-elements/init-selection", controller.RelayKlingVideo)
		klingRouter.POST("/videos/video-extend", controller.RelayKlingVideo)
		klingRouter.POST("/videos/avatar/image2video", controller.RelayKlingVideo)
		klingRouter.POST("/videos/effects", controller.RelayKlingVideo)
		klingRouter.POST("/videos/image-recognize", controller.RelayKlingVideo)

		// ========== 音频类接口（3个）==========
		klingRouter.POST("/audio/text-to-audio", controller.RelayKlingVideo)
		klingRouter.POST("/audio/video-to-audio", controller.RelayKlingVideo)
		klingRouter.POST("/audio/tts", controller.RelayKlingVideo)

		// ========== 图片类接口（4个）==========
		klingRouter.POST("/images/generations", controller.RelayKlingVideo)
		klingRouter.POST("/images/omni-image", controller.RelayKlingVideo)
		klingRouter.POST("/images/multi-image2image", controller.RelayKlingVideo)
		klingRouter.POST("/images/editing/expand", controller.RelayKlingVideo)

		// ========== 通用类接口（2个）==========
		klingRouter.POST("/general/custom-elements", controller.DoCustomElements)           // 同步接口
		klingRouter.POST("/general/advanced-custom-elements", controller.RelayKlingVideo)   // 异步接口
		klingRouter.POST("/general/custom-voices", controller.RelayKlingVideo)              // 异步接口，复用逻辑

		// ========== 查询和管理接口（透明代理，不计费）==========
		// 使用通配符批量处理所有 GET 查询接口
		klingRouter.GET("/videos/*path", controller.RelayKlingTransparent)  // 视频类查询
		klingRouter.GET("/audio/*path", controller.RelayKlingTransparent)   // 音频类查询
		klingRouter.GET("/images/*path", controller.RelayKlingTransparent)  // 图片类查询
		klingRouter.GET("/general/*path", controller.RelayKlingTransparent) // 通用类查询
		//klingRouter.POST("/general/delete-elements", controller.RelayKlingTransparent) // 删除元素
		//klingRouter.POST("/general/delete-voices", controller.RelayKlingTransparent)   // 删除声音
	}
	// Kling 查询路由（不需要 Distribute 中间件）
	klingResultRouter := router.Group("/kling/v1")
	klingResultRouter.Use(middleware.RelayPanicRecover(), middleware.TokenAuth())
	{
		klingResultRouter.GET("/video/:id", controller.RelayKlingVideoResult)
	}

	// Kling 回调路由（不需要认证）
	klingCallbackRouter := router.Group("/kling/internal")
	{
		klingCallbackRouter.POST("/callback", controller.HandleKlingCallback)
	}

	// // Gemini 原生API透传路由组 - 支持完整的Gemini官方API格式
	// geminiNativeRouter := router.Group("/gemini/v1beta")
	// geminiNativeRouter.Use(middleware.RelayPanicRecover(), middleware.TokenAuth(), middleware.Distribute())
	// {
	// 	// 核心聊天接口
	// 	geminiNativeRouter.POST("/models/:model:generateContent", controller.RelayGeminiNative)
	// 	geminiNativeRouter.POST("/models/:model:streamGenerateContent", controller.RelayGeminiNative)

	// 	// Token计数接口
	// 	geminiNativeRouter.POST("/models/:model:countTokens", controller.RelayGeminiNative)

	// 	// 嵌入接口
	// 	geminiNativeRouter.POST("/models/:model:embedContent", controller.RelayGeminiNative)
	// 	geminiNativeRouter.POST("/models/:model:batchEmbedContents", controller.RelayGeminiNative)

	// 	// 模型管理接口
	// 	geminiNativeRouter.GET("/models", controller.RelayGeminiNative)
	// 	geminiNativeRouter.GET("/models/:model", controller.RelayGeminiNative)

	// 	// 文件上传接口
	// 	geminiNativeRouter.POST("/files", controller.RelayGeminiNative)
	// 	geminiNativeRouter.GET("/files/:name", controller.RelayGeminiNative)
	// 	geminiNativeRouter.DELETE("/files/:name", controller.RelayGeminiNative)
	// 	geminiNativeRouter.GET("/files", controller.RelayGeminiNative)

	// 	// 微调接口
	// 	geminiNativeRouter.POST("/tunedModels", controller.RelayGeminiNative)
	// 	geminiNativeRouter.GET("/tunedModels", controller.RelayGeminiNative)
	// 	geminiNativeRouter.GET("/tunedModels/:name", controller.RelayGeminiNative)
	// 	geminiNativeRouter.PATCH("/tunedModels/:name", controller.RelayGeminiNative)
	// 	geminiNativeRouter.DELETE("/tunedModels/:name", controller.RelayGeminiNative)
	// }

	// Claude 原生API透传路由组 - 支持完整的Anthropic Claude官方API格式

	// Gemini API原生接口路由组
	// 路径格式: /v1beta/models/{model_name}:{action}
	// 支持 generateContent, streamGenerateContent, embedContent, batchEmbedContents 等操作
	geminiRouter := router.Group("/v1beta")
	geminiRouter.Use(middleware.TokenAuth(), middleware.Distribute())
	{
		// 使用通配符捕获 models/ 后的所有内容: gemini-2.0-flash:generateContent
		geminiRouter.POST("/models/*path", controller.RelayGemini)
	}

	// Gemini API v1 版本路由（某些模型使用 v1）
	geminiV1Router := router.Group("/v1")
	geminiV1Router.Use(middleware.TokenAuth(), middleware.Distribute())
	{
		geminiV1Router.POST("/models/*path", controller.RelayGemini)
	}

	// Gemini API v1alpha 版本路由（某些项目使用 v1alpha）
	geminiV1AlphaRouter := router.Group("/v1alpha")
	geminiV1AlphaRouter.Use(middleware.TokenAuth(), middleware.Distribute())
	{
		geminiV1AlphaRouter.POST("/models/*path", controller.RelayGemini)
	}

	// 阿里云万相视频生成路由组
	// 路径与 DashScope 原生路径对应，统一支持 T2V 和 I2V
	// POST 和 GET 均挂载 Distribute，GET handler 内部会用 resolveChannelForTaskQuery 覆盖为任务绑定渠道
	aliVideoRouter := router.Group("/ali/api/v1")
	aliVideoRouter.Use(middleware.RelayPanicRecover(), middleware.TokenAuth(), middleware.Distribute())
	{
		aliVideoRouter.POST("/services/aigc/video-generation/video-synthesis", controller.RelayAliVideoCreate)
		aliVideoRouter.GET("/tasks/:taskId", controller.RelayAliVideoResult)
	}

	// xAI Grok Video 原生透传路由组
	// POST 端点需要 Distribute 中间件进行渠道选择
	xaiVideoRouter := router.Group("/xai/v1/videos")
	xaiVideoRouter.Use(middleware.RelayPanicRecover(), middleware.TokenAuth(), middleware.Distribute())
	{
		xaiVideoRouter.POST("/generations", controller.RelayXaiVideoGeneration)
		xaiVideoRouter.POST("/edits", controller.RelayXaiVideoEdit)
		xaiVideoRouter.POST("/extensions", controller.RelayXaiVideoExtension)
		
	}

	// xAI Grok Video 查询路由 - 不需要 Distribute，从数据库查找渠道
	xaiVideoResultRouter := router.Group("/xai/v1/videos")
	xaiVideoResultRouter.Use(middleware.RelayPanicRecover(), middleware.TokenAuth())
	{
		xaiVideoResultRouter.GET("/:requestId", controller.RelayXaiVideoResult)
	}
}
