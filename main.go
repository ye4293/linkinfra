package main

import (
	"context"
	"embed"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/controller"
	"github.com/songquanpeng/one-api/middleware"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/monitor"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/router"
)

//go:embed web/build/*
var buildFS embed.FS

// monitorGoroutines 定期监控 goroutine 数量
func monitorGoroutines() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		count := runtime.NumGoroutine()

		// 记录当前goroutine数量
		if count > 5000 {
			logger.SysError(fmt.Sprintf("⚠️ High goroutine count detected: %d", count))
		} else if count > 2000 {
			logger.SysLog(fmt.Sprintf("⚠️ Goroutine count elevated: %d", count))
		} else {
			// 只在调试模式下记录正常数量
			if config.DebugEnabled {
				logger.SysLog(fmt.Sprintf("Goroutine count: %d", count))
			}
		}

		// 记录内存统计
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		if config.DebugEnabled {
			logger.SysLog(fmt.Sprintf("Memory: Alloc=%dMB, TotalAlloc=%dMB, Sys=%dMB, NumGC=%d",
				m.Alloc/1024/1024, m.TotalAlloc/1024/1024, m.Sys/1024/1024, m.NumGC))
		}
	}
}

// setupMonitoringEndpoints 设置监控端点
func setupMonitoringEndpoints(server *gin.Engine) {
	// 添加健康检查端点
	server.GET("/api/monitor/health", func(c *gin.Context) {
		count := runtime.NumGoroutine()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)

		c.JSON(200, gin.H{
			"status":     "ok",
			"goroutines": count,
			"memory": gin.H{
				"alloc_mb":       m.Alloc / 1024 / 1024,
				"total_alloc_mb": m.TotalAlloc / 1024 / 1024,
				"sys_mb":         m.Sys / 1024 / 1024,
				"num_gc":         m.NumGC,
			},
		})
	})

	logger.SysLog("monitoring endpoints enabled at /api/monitor/health")
}

func main() {
	logger.SetupLogger()
	logger.SysLog(fmt.Sprintf("One API %s started", common.Version))
	if os.Getenv("GIN_MODE") != "debug" {
		gin.SetMode(gin.ReleaseMode)
	}
	if config.DebugEnabled {
		logger.SysLog("running in debug mode")
	}
	var err error
	// Initialize SQL Database
	model.DB, err = model.InitDB("SQL_DSN")
	if err != nil {
		logger.FatalLog("failed to initialize database: " + err.Error())
	}
	if os.Getenv("LOG_SQL_DSN") != "" {
		logger.SysLog("using secondary database for table logs")
		model.LOG_DB, err = model.InitDB("LOG_SQL_DSN")
		if err != nil {
			logger.FatalLog("failed to initialize secondary database: " + err.Error())
		}
	} else {
		model.LOG_DB = model.DB
	}
	err = model.CreateRootAccountIfNeed()
	if err != nil {
		logger.FatalLog("database init error: " + err.Error())
	}
	defer func() {
		err := model.CloseDB()
		if err != nil {
			logger.FatalLog("failed to close database: " + err.Error())
		}
	}()

	// Initialize Redis
	err = common.InitRedisClient()
	if err != nil {
		logger.FatalLog("failed to initialize Redis: " + err.Error())
	}

	// Initialize options
	model.InitOptionMap()
	logger.SysLog(fmt.Sprintf("using theme %s", config.Theme))
	if common.RedisEnabled {
		// for compatibility with old versions
		config.MemoryCacheEnabled = true
	}
	if config.MemoryCacheEnabled {
		logger.SysLog("memory cache enabled")
		logger.SysError(fmt.Sprintf("sync frequency: %d seconds", config.SyncFrequency))
		model.InitChannelCache()
	}

	// 系统启动时检查数据一致性
	logger.SysLog("checking data consistency between channels and abilities...")
	err = model.CheckDataConsistency()
	if err != nil {
		logger.SysError("data consistency check failed: " + err.Error())
		// 数据一致性检查失败不应该阻止系统启动，但需要记录
	} else {
		logger.SysLog("data consistency check completed successfully")
	}
	if config.MemoryCacheEnabled {
		go model.SyncOptions(config.SyncFrequency)
		go model.SyncChannelCache(config.SyncFrequency)
	}

	go controller.AutomaticallyTestChannels()
	controller.StartChannelUpstreamModelUpdateTask()
	if os.Getenv("BATCH_UPDATE_ENABLED") == "true" {
		config.BatchUpdateEnabled = true
		logger.SysLog("batch update enabled with interval " + strconv.Itoa(config.BatchUpdateInterval) + "s")
		model.InitBatchUpdater()
	}
	if config.EnableMetric {
		logger.SysLog("metric enabled, will disable channel if too much request failed")
	}
	openai.InitTokenEncoders()

	// 启动Key禁用通知监听器
	monitor.StartKeyNotificationListener()
	logger.SysLog("key disable notification listener started")

	// 启动阿里云万相视频任务轮询器
	common.SafeGoroutine(func() {
		controller.StartAliWanTaskPoller(context.Background())
	})

	// 启动豆包视频任务轮询器
	common.SafeGoroutine(func() {
		controller.StartDoubaoVideoTaskPoller(context.Background())
	})

	// 启动 Flux/Replicate 任务对账 reconciler
	common.SafeGoroutine(func() {
		controller.StartFluxReconciler(context.Background())
	})

	// 启动 xAI 视频任务轮询器（带 Redis 分布式锁）
	common.SafeGoroutine(func() {
		controller.StartXaiVideoTaskPoller(context.Background())
	})
	
	// 启动 Goroutine 监控
	go monitorGoroutines()

	// 启动模型指标聚合 Worker
	if config.ModelMetricsEnabled {
		common.SafeGoroutine(func() {
			model.StartModelMetricsAggregator()
		})
		logger.SysLog("model metrics aggregator started")
	}

	// Initialize HTTP server
	server := gin.New()
	server.Use(gin.Recovery())
	// This will cause SSE not to work!!!
	//server.Use(gzip.Gzip(gzip.DefaultCompression))
	server.Use(middleware.RequestId())
	middleware.SetUpLogger(server)
	// Initialize session store
	store := cookie.NewStore([]byte(config.SessionSecret))
	server.Use(sessions.Sessions("session", store))

	router.SetRouter(server, buildFS)

	// 添加监控端点
	setupMonitoringEndpoints(server)
	var port = os.Getenv("PORT")
	if port == "" {
		port = strconv.Itoa(*common.Port)
	}
	err = server.Run(":" + port)
	if err != nil {
		logger.FatalLog("failed to start HTTP server: " + err.Error())
	}
}
