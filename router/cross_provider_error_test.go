package router

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/util"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// upstreamReply 是模拟上游的一次应答。contentType 为空时是 application/json。
type upstreamReply struct {
	status      int
	body        string
	contentType string
}

// crossProviderEnv 是「跨 provider 重试后返回哪个错误」的测试环境：真实的鉴权、分发和重试循环，
// 两个渠道各有一个本地模拟上游：#1（优先级 100，provider p-first）和 #2（优先级 50，provider p-second）。
type crossProviderEnv struct {
	t       *testing.T
	db      *gorm.DB
	gateway *httptest.Server
	mu      sync.Mutex
	hits    []int
	replies map[int]upstreamReply
}

func newCrossProviderEnv(t *testing.T, channelType int, modelName string) *crossProviderEnv {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Log{}))
	oldDB, oldLogDB, oldClient := model.DB, model.LOG_DB, util.HTTPClient
	oldRedis, oldBatch, oldLog := common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled
	oldRetry, oldDisable, oldMemory := config.RetryTimes, config.AutomaticDisableChannelEnabled, config.MemoryCacheEnabled
	oldMetrics, oldMetricsV2, oldGroups := config.ModelMetricsEnabled, config.ModelMetricsV2Enabled, common.GroupRatio
	oldPing, oldApproximate := config.PingIntervalEnabled, config.ApproximateTokenEnabled
	t.Cleanup(func() {
		model.DB, model.LOG_DB, util.HTTPClient = oldDB, oldLogDB, oldClient
		common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled = oldRedis, oldBatch, oldLog
		config.RetryTimes, config.AutomaticDisableChannelEnabled, config.MemoryCacheEnabled = oldRetry, oldDisable, oldMemory
		config.ModelMetricsEnabled, config.ModelMetricsV2Enabled, common.GroupRatio = oldMetrics, oldMetricsV2, oldGroups
		config.PingIntervalEnabled, config.ApproximateTokenEnabled = oldPing, oldApproximate
		require.NoError(t, common.UpdateProviderInteropRulesByJSONString(""))
		_ = sqlDB.Close()
	})
	// 部分重试循环在协程里处理渠道错误（要读库）。后注册的清理先执行：
	// 等这些协程结束，再执行上面的清理把全局的数据库还原，它们不会读到别的测试的库。
	t.Cleanup(func() { waitForChannelErrorGoroutines(t) })
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled = false, false, true
	config.RetryTimes, config.AutomaticDisableChannelEnabled, config.MemoryCacheEnabled = 3, false, false
	config.ModelMetricsEnabled, config.ModelMetricsV2Enabled = false, false
	config.PingIntervalEnabled = false
	// 测试进程没有加载分词器（服务启动时才加载），预扣费的 token 数用近似算法。
	config.ApproximateTokenEnabled = true
	const group = "cross-provider"
	common.GroupRatio = map[string]float64{group: 1}

	user := model.User{Username: "cross-provider", Group: group, Status: common.UserStatusEnabled, Quota: 100000000000, AccessToken: "cross-provider-access", AffCode: "cross-provider"}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Name: "cross", Key: "crossprovider", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, token.Insert())

	env := &crossProviderEnv{t: t, db: db, replies: map[int]upstreamReply{}}
	for _, spec := range []struct {
		id       int
		provider string
		priority int64
	}{{1, "p-first", 100}, {2, "p-second", 50}} {
		id := spec.id
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			env.mu.Lock()
			env.hits = append(env.hits, id)
			reply := env.replies[id]
			env.mu.Unlock()
			contentType := reply.contentType
			if contentType == "" {
				contentType = "application/json"
			}
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(reply.status)
			_, _ = io.WriteString(w, reply.body)
		}))
		t.Cleanup(server.Close)
		util.HTTPClient = server.Client()
		weight, baseURL := uint(1), server.URL
		channel := model.Channel{Id: id, Name: fmt.Sprintf("%s-%d", spec.provider, id), Type: channelType, Key: fmt.Sprintf("upstream-key-%d", id), BaseURL: &baseURL,
			Status: common.ChannelStatusEnabled, Models: modelName, Group: group, Priority: &spec.priority, Weight: &weight, Config: `{"provider":"` + spec.provider + `"}`}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, db.Create(&model.Ability{ChannelId: id, Group: group, Model: modelName, Enabled: true, Priority: &spec.priority}).Error)
	}

	r := gin.New()
	r.Use(sessions.Sessions("cross-provider", cookie.NewStore([]byte("test-session-secret-only"))))
	SetRelayRouter(r)
	env.gateway = httptest.NewServer(r)
	t.Cleanup(env.gateway.Close)
	return env
}

// waitForChannelErrorGoroutines 等通用（非 systemone）、Claude 和 Gemini 重试循环里
// 异步处理渠道错误的协程（go processChannelRelayError）结束。
// 它们在测试结束后才运行的话，会读到已经被还原的全局数据库。
func waitForChannelErrorGoroutines(t *testing.T) {
	t.Helper()
	const prefix = "created by github.com/songquanpeng/one-api/controller."
	require.Eventually(t, func() bool {
		buf := make([]byte, 1<<20)
		for {
			n := runtime.Stack(buf, true)
			if n < len(buf) {
				buf = buf[:n]
				break
			}
			buf = make([]byte, 2*len(buf))
		}
		// 协程的创建者：Claude 和 Gemini 是重试循环本身；通用循环是 Relay 或它里面的闭包。
		for _, creator := range []string{"RelayClaude ", "RelayGemini ", "Relay ", "Relay.func"} {
			if bytes.Contains(buf, []byte(prefix+creator)) {
				return false
			}
		}
		return true
	}, 10*time.Second, 2*time.Millisecond)
}

// setProviders 改两个渠道的 provider，空串表示不填。
func (env *crossProviderEnv) setProviders(first, second string) {
	env.t.Helper()
	for id, provider := range map[int]string{1: first, 2: second} {
		channelConfig := ""
		if provider != "" {
			channelConfig = `{"provider":"` + provider + `"}`
		}
		require.NoError(env.t, env.db.Model(&model.Channel{}).Where("id = ?", id).Update("config", channelConfig).Error)
	}
}

// send 让两个上游按指定的内容应答，发一次请求，返回状态码、响应体和上游被请求的先后。
func (env *crossProviderEnv) send(path, body string, first, second upstreamReply) (int, string, []int) {
	env.t.Helper()
	env.mu.Lock()
	env.hits = nil
	env.replies = map[int]upstreamReply{1: first, 2: second}
	env.mu.Unlock()
	req, err := http.NewRequest("POST", env.gateway.URL+path, strings.NewReader(body))
	require.NoError(env.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-crossprovider")
	res, err := env.gateway.Client().Do(req)
	require.NoError(env.t, err)
	defer res.Body.Close()
	responseBody, err := io.ReadAll(res.Body)
	require.NoError(env.t, err)
	env.mu.Lock()
	defer env.mu.Unlock()
	return res.StatusCode, string(responseBody), append([]int(nil), env.hits...)
}

// errorLog 返回唯一的一条错误日志，并清空日志表。
func (env *crossProviderEnv) errorLog() model.Log {
	env.t.Helper()
	var logs []model.Log
	require.NoError(env.t, env.db.Where("type = ?", model.LogTypeError).Find(&logs).Error)
	require.Len(env.t, logs, 1)
	require.NoError(env.t, env.db.Where("1 = 1").Delete(&model.Log{}).Error)
	return logs[0]
}

const (
	crossFirstMessage = "rate limit reached on the first provider"
	crossOtherMessage = "this provider does not support the requested feature"
	// 通用循环（systemone 除外）把 429 的文案换成这一句。
	saturatedMessage = "The current group upstream load is saturated, please try again later."
)

const crossProviderRules = `[{"models":["^(jev|claude|gemini|gpt)-"],"providers":["p-first","p-second"]}]`

// 互通规则让重试可以换到另一个 provider。换过去之后得到不可重试的错误（那个 provider 不支持请求里的功能）时，
// 客户端看到的是首次尝试的错误（限流），而不是后一个 provider 的 400：原 provider 恢复后同样的请求可以成功。
// 通用、Claude、Gemini、Responses 四个重试循环的行为一致；其余情况返回的错误与之前相同。
func TestCrossProviderRetryKeepsOriginalError(t *testing.T) {
	openAIError := func(status int, message, kind string) upstreamReply {
		return upstreamReply{status: status, body: `{"error":{"message":"` + message + `","type":"` + kind + `"}}`}
	}
	for _, protocol := range []struct {
		name                                 string
		channelType                          int
		model, path, body                    string
		rateLimited, unsupported, overloaded upstreamReply
		// rateLimitText 是首次的 429 返回给客户端时的文案。
		rateLimitText string
		// asyncChannelErrors 表示这个重试循环在协程里处理渠道错误（go processChannelRelayError）。
		asyncChannelErrors bool
	}{
		{
			name: "通用（systemone）", channelType: common.ChannelTypeTypesafe, model: "jev-latest", path: "/v1/systemone",
			body:          `{"model":"jev-latest","state":"hello","questions":{"check":{"type":"noul","instructions":"Greeting?"}}}`,
			rateLimited:   upstreamReply{status: 429, body: `{"detail":"` + crossFirstMessage + `"}`},
			unsupported:   upstreamReply{status: 400, body: `{"detail":"` + crossOtherMessage + `"}`},
			overloaded:    upstreamReply{status: 529, body: `{"detail":"overloaded"}`},
			rateLimitText: crossFirstMessage,
		},
		{
			name: "通用（chat）", channelType: common.ChannelTypeOpenAI, model: "gpt-test", path: "/v1/chat/completions", asyncChannelErrors: true,
			body:          `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`,
			rateLimited:   openAIError(429, crossFirstMessage, "rate_limit_error"),
			unsupported:   openAIError(400, crossOtherMessage, "invalid_request_error"),
			overloaded:    openAIError(503, "overloaded", "server_error"),
			rateLimitText: saturatedMessage,
		},
		{
			name: "Claude", channelType: common.ChannelTypeAnthropic, model: "claude-test", path: "/v1/messages", asyncChannelErrors: true,
			body:          `{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			rateLimited:   upstreamReply{status: 429, body: `{"type":"error","error":{"type":"rate_limit_error","message":"` + crossFirstMessage + `"}}`},
			unsupported:   upstreamReply{status: 400, body: `{"type":"error","error":{"type":"invalid_request_error","message":"` + crossOtherMessage + `"}}`},
			overloaded:    upstreamReply{status: 529, body: `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`},
			rateLimitText: crossFirstMessage,
		},
		{
			name: "Gemini", channelType: common.ChannelTypeGemini, model: "gemini-test", path: "/v1beta/models/gemini-test:generateContent", asyncChannelErrors: true,
			body:          `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			rateLimited:   upstreamReply{status: 429, body: `{"error":{"code":429,"message":"` + crossFirstMessage + `","status":"RESOURCE_EXHAUSTED"}}`},
			unsupported:   upstreamReply{status: 400, body: `{"error":{"code":400,"message":"` + crossOtherMessage + `","status":"INVALID_ARGUMENT"}}`},
			overloaded:    upstreamReply{status: 503, body: `{"error":{"code":503,"message":"overloaded","status":"UNAVAILABLE"}}`},
			rateLimitText: crossFirstMessage,
		},
		{
			name: "Responses", channelType: common.ChannelTypeOpenAI, model: "gpt-test", path: "/v1/responses",
			body:          `{"model":"gpt-test","input":"hi"}`,
			rateLimited:   openAIError(429, crossFirstMessage, "rate_limit_error"),
			unsupported:   openAIError(400, crossOtherMessage, "invalid_request_error"),
			overloaded:    openAIError(503, "overloaded", "server_error"),
			rateLimitText: crossFirstMessage,
		},
	} {
		t.Run(protocol.name, func(t *testing.T) {
			if protocol.asyncChannelErrors && raceDetectorEnabled {
				// 处理渠道错误的协程会读全局配置，测试没有办法和它们建立同步关系：
				// 竞态检测会把之后任何测试对这些全局变量的写入报成竞争，即使协程早已结束。
				t.Skip("this retry loop handles channel errors in goroutines the test cannot join; skipped under -race")
			}
			env := newCrossProviderEnv(t, protocol.channelType, protocol.model)
			send := func(first, second upstreamReply) (int, string, []int) {
				t.Helper()
				return env.send(protocol.path, protocol.body, first, second)
			}

			// 没有互通规则：重试只在首选渠道的 provider 内，返回它的错误。
			status, body, hits := send(protocol.rateLimited, protocol.unsupported)
			require.Equal(t, []int{1}, hits)
			require.Equal(t, http.StatusTooManyRequests, status, body)
			require.Contains(t, body, protocol.rateLimitText)
			env.errorLog()

			require.NoError(t, common.UpdateProviderInteropRulesByJSONString(crossProviderRules))

			// 换到另一个 provider 后得到不可重试的错误：返回首次尝试的错误。
			status, body, hits = send(protocol.rateLimited, protocol.unsupported)
			require.Equal(t, []int{1, 2}, hits)
			require.Equal(t, http.StatusTooManyRequests, status, body)
			require.Contains(t, body, protocol.rateLimitText)
			require.NotContains(t, body, crossOtherMessage)
			// 错误日志记的是返回给客户端的错误；后一个 provider 的错误保留在重试明细里。
			entry := env.errorLog()
			require.Contains(t, entry.Content, protocol.rateLimitText)
			require.NotContains(t, entry.Content, crossOtherMessage)
			require.Contains(t, entry.Other, "retryHistory:")
			require.Contains(t, entry.Other, protocol.rateLimitText)
			require.Contains(t, entry.Other, crossOtherMessage)

			// 另一个 provider 的错误是可以重试的（过载）、只是没有渠道可换了：仍返回最后一次的错误。
			status, body, hits = send(protocol.rateLimited, protocol.overloaded)
			require.Equal(t, []int{1, 2}, hits)
			require.Equal(t, protocol.overloaded.status, status, body)
			require.Contains(t, body, "overloaded")
			env.errorLog()

			// 不可重试的错误来自首选渠道的同一个 provider：请求本身有问题，照常返回它。
			env.setProviders("p-first", "p-first")
			status, body, hits = send(protocol.rateLimited, protocol.unsupported)
			require.Equal(t, []int{1, 2}, hits)
			require.Equal(t, http.StatusBadRequest, status, body)
			require.Contains(t, body, crossOtherMessage)
			require.Contains(t, env.errorLog().Content, crossOtherMessage)

			// 渠道都没有填 provider（重试不受限制，原有行为）：照常返回最后一次的错误。
			env.setProviders("", "")
			status, body, hits = send(protocol.rateLimited, protocol.unsupported)
			require.Equal(t, []int{1, 2}, hits)
			require.Equal(t, http.StatusBadRequest, status, body)
			require.Contains(t, body, crossOtherMessage)
		})
	}
}

// Responses 请求在另一个 provider 上已经开始向客户端输出之后才失败：
// 不能再换成首次的错误，错误日志记录的是实际发生的这次失败。
func TestCrossProviderRetryKeepsLateStreamError(t *testing.T) {
	env := newCrossProviderEnv(t, common.ChannelTypeOpenAI, "gpt-test")
	require.NoError(t, common.UpdateProviderInteropRulesByJSONString(crossProviderRules))
	const lateMessage = "stream failed on the second provider"
	rateLimited := upstreamReply{status: 429, body: `{"error":{"message":"` + crossFirstMessage + `","type":"rate_limit_error"}}`}
	started := upstreamReply{status: 200, contentType: "text/event-stream", body: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_late\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\"}}\n\n" +
		"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"" + lateMessage + "\"}}}\n\n"}

	status, body, hits := env.send("/v1/responses", `{"model":"gpt-test","input":"hi","stream":true}`, rateLimited, started)
	require.Equal(t, []int{1, 2}, hits)
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, "response.output_item.added")
	require.Contains(t, body, lateMessage)
	entry := env.errorLog()
	require.Contains(t, entry.Content, lateMessage)
	require.NotContains(t, entry.Content, crossFirstMessage)
	require.Contains(t, entry.Other, `"stop_reason":"response_already_started"`)
}
