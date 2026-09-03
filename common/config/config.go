package config

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/songquanpeng/one-api/common/env"
)

var GeminiVersion = "v1/beta"
var SystemName = "EZLINK AI"
var ServerAddress = "http://localhost:3000"
var FrontendServerAddress = ""
var DocsAddress = ""
var Footer = ""
var Logo = ""
var TopUpLink = ""
var ChatLink = ""
var QuotaPerUnit = 500 * 1000.0 // $0.002 / 1K tokens
var DisplayInCurrencyEnabled = true
var DisplayTokenStatEnabled = true

// Any options with "Secret", "Token" in its key won't be return by GetOptions

var SessionSecret = uuid.New().String()

var OptionMap map[string]string
var OptionMapRWMutex sync.RWMutex

var ItemsPerPage = 10
var MaxRecentItems = 100

var PasswordLoginEnabled = true
var PasswordRegisterEnabled = true
var EmailVerificationEnabled = false
var GitHubOAuthEnabled = false

// OAuthLoginSecret 是前端（next-auth）与本服务之间的共享密钥，从环境变量
// OAUTH_LOGIN_SECRET 读取。
//
// POST /api/{github,google}/login 接收的是「用户已通过 OAuth 认证」这个
// 断言，OAuth 的 code 交换发生在前端（next-auth 持有 client secret），
// 后端无法自己验证断言真伪。没有这道密钥，任何人拿着公开可查的
// github_id（api.github.com/users/<login> 就能拿到）直接 POST 就能取得
// 对方的 session 与 access_token —— 零凭证的全量账号接管。
//
// 未配置时这两个端点一律拒绝（fail closed）：若改成放行，漏配不会有任何
// 症状，线上会长期处于裸奔状态而无人察觉。
var OAuthLoginSecret = os.Getenv("OAUTH_LOGIN_SECRET")

var TurnstileCheckEnabled = false
var RegisterEnabled = true

var CryptPaymentEnabled = false
var StripePaymentEnabled = false
var CryptCallbackUrl = ""
var AddressOut = ""
var StripeCallbackUrl = ""
var StripePrivateKey = ""
var StripePublicKey = ""
var StripeEndpointSecret = ""
var StripeApiSecret = ""
var StripeWebhookSecret = ""
var StripePriceId = ""
var StripeMinTopUp = 1
var StripePromotionCodesEnabled = false

var CfR2storeEnabled = true
var CfBucketFileName = "ezlinkai-file"

// var CfFileAccessKey = "42a3d63d1371f46956f7d3de36b3b9a5"
// var CfFileSecretKey = "31db6128dbf10f3a4a823cea1e52af23934e77353d06f1d1f966288e217073f9"
// var CfFileEndpoint = "https://f19328743901865dd8223e016b2ff78d.r2.cloudflarestorage.com"

var CfFileAccessKey = "GQwDfmzqBlTMsBjf"
var CfFileSecretKey = "JrL2Sy9ojWemCKv45iJukOKKrlBw64"
var CfFileEndpoint = "https://hua.cn-nb1.rains3.com"
var CfFilePublicUrl = "" // 公共访问 URL（如自定义域），为空时使用 Endpoint

var CfBucketImageName = ""
var CfImageAccessKey = ""
var CfImageSecretKey = ""
var CfImageEndpoint = ""

var EmailDomainRestrictionEnabled = false
var EmailDomainWhitelist = []string{
	"gmail.com",
	"163.com",
	"126.com",
	"qq.com",
	"outlook.com",
	"hotmail.com",
	"icloud.com",
	"yahoo.com",
	"foxmail.com",
}

var DebugEnabled = strings.ToLower(os.Getenv("DEBUG")) == "true"
var DebugSQLEnabled = strings.ToLower(os.Getenv("DEBUG_SQL")) == "true"
var MemoryCacheEnabled = strings.ToLower(os.Getenv("MEMORY_CACHE_ENABLED")) == "true"

var LogConsumeEnabled = true

var ResendApiKey = ""
var ResendFrom = ""

var GitHubClientId = ""
var GitHubClientSecret = ""
var GithubRedirectUri = ""

var GoogleOAuthEnabled = true
var GoogleClientId = ""
var GoogleClientSecret = ""
var GoogleRedirectUri = ""
var StripeKey = ""


var MessagePusherAddress = ""
var MessagePusherToken = ""

var TurnstileSiteKey = ""
var TurnstileSecretKey = ""

var QuotaForNewUser int64 = 0
var QuotaForInviter int64 = 0
var QuotaForInvitee int64 = 0
var ChannelDisableThreshold = 5.0
var AutomaticDisableChannelEnabled = false
var AutomaticEnableChannelEnabled = false
var AutoTestChannelFrequency = 0           // 自动测试渠道的频率（分钟），0 表示禁用
var UpstreamModelUpdateIntervalMinutes = 0 // 上游模型巡检间隔（分钟），0 表示使用默认值（5 分钟 / 300 秒）
var FeishuWebhookUrls = ""
var PingIntervalEnabled = false
var PingIntervalSeconds = 0

// 自动禁用关键词配置（一行一个关键词）
var AutoDisableKeywords = `api key not valid
invalid_api_key
incorrect api key provided
authentication_error
permission denied
account_deactivated
insufficient_quota
credit balance is too low
not_enough_credits
credit
balance
used all available credits
reached its monthly spending limit
resource pack exhausted
billing to be enabled
permission_denied
unauthenticated
operation not allowed
organization has been disabled
consumer
has been suspended
service account
project not found
billing account
imagen api
generativelanguage.googleapis.com
console.x.ai`
// 跨渠道重试关键词配置（一行一个关键词，匹配到则触发跨渠道重试）
var RetryKeywords = `api key not valid
invalid_api_key
authentication_error
api key not found
invalid api key
billing_hard_limit_reached
billing hard limit has been reached
billing limit has been reached
hard limit has been reached
operation not allowed
your resource has been blocked because we detected unusual behavior`

var QuotaRemindThreshold int64 = 1000
var PreConsumedQuota int64 = 500
var ApproximateTokenEnabled = false
var RetryTimes = 0

var RootUserEmail = ""

var IsMasterNode = os.Getenv("NODE_TYPE") == "master"

var requestInterval, _ = strconv.Atoi(os.Getenv("POLLING_INTERVAL"))
var RequestInterval = time.Duration(requestInterval) * time.Second

var SyncFrequency = env.Int("SYNC_FREQUENCY", 10*60) // unit is second

var BatchUpdateEnabled = false
var BatchUpdateInterval = env.Int("BATCH_UPDATE_INTERVAL", 5)

var RelayTimeout = env.Int("RELAY_TIMEOUT", 0)           // unit is second
var StreamingTimeout = env.Int("STREAMING_TIMEOUT", 300) // unit is second

var GeminiSafetySetting = env.String("GEMINI_SAFETY_SETTING", "BLOCK_NONE")

var Theme = env.String("THEME", "default")
var ValidThemes = map[string]bool{
	"default": true,
	"berry":   true,
	"air":     true,
}

// All duration's unit is seconds
// Shouldn't larger then RateLimitKeyExpirationDuration
var (
	GlobalApiRateLimitNum            = env.Int("GLOBAL_API_RATE_LIMIT", 180000)
	GlobalApiRateLimitDuration int64 = 30 * 60

	GlobalWebRateLimitNum            = env.Int("GLOBAL_WEB_RATE_LIMIT", 6000)
	GlobalWebRateLimitDuration int64 = 30 * 60

	UploadRateLimitNum            = 10
	UploadRateLimitDuration int64 = 600

	DownloadRateLimitNum            = 10
	DownloadRateLimitDuration int64 = 60

	CriticalRateLimitNum            = 200
	CriticalRateLimitDuration int64 = 200 * 60
)

var RateLimitKeyExpirationDuration = 20 * time.Minute

var EnableMetric = env.Bool("ENABLE_METRIC", false)
var MetricQueueSize = env.Int("METRIC_QUEUE_SIZE", 10)
var MetricSuccessRateThreshold = env.Float64("METRIC_SUCCESS_RATE_THRESHOLD", 0.8)
var MetricSuccessChanSize = env.Int("METRIC_SUCCESS_CHAN_SIZE", 1024)
var MetricFailChanSize = env.Int("METRIC_FAIL_CHAN_SIZE", 128)

var InitialRootToken = os.Getenv("INITIAL_ROOT_TOKEN")

// 模型监控指标配置
var ModelMetricsEnabled = env.Bool("MODEL_METRICS_ENABLED", true)
var ModelMetricsAggregationInterval = env.Int("MODEL_METRICS_AGGREGATION_INTERVAL", 300)  // 聚合间隔（秒）
var ModelMetricsRetentionDays = env.Int("MODEL_METRICS_RETENTION_DAYS", 30)               // 数据保留天数
var ModelMetricsBackfillDays = env.Int("MODEL_METRICS_BACKFILL_DAYS", 7)                  // 首次回填天数

// Claude Thinking 模型配置
var ClaudeThinkingEnabled = true                      // 是否启用 Claude 思考适配（-thinking 后缀）
var ClaudeThinkingBudgetRatio = 0.8                   // 默认思考 token 百分比（80%）
var ClaudeDefaultMaxTokens map[string]int             // 模型默认 MaxTokens
var ClaudeReasoningEffortMap map[string]float64       // reasoning_effort 到百分比的映射
var ClaudeRequestHeaders map[string]map[string]string // 请求头覆盖（模型名 -> 请求头键值对）

// 日志标识，用于 JSON 日志中的 service/instance 字段
var ServiceName = env.String("SERVICE_NAME", "one-api")
var InstanceId = env.String("INSTANCE_ID", getHostname())

// 慢请求阈值：200 请求超过此毫秒数才写 access log，0 表示不记录 200
var SlowRequestThresholdMs = env.Int("SLOW_REQUEST_THRESHOLD_MS", 5000)

func getHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}
