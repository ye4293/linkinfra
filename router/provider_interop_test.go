package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/util"
	"github.com/songquanpeng/one-api/service"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const claudeInteropRules = `[{"models":["^claude-"],"providers":["anthropic","vertex"]}]`

// 互通规则作为系统设置保存：只有 root 可以改，不合法的规则被拒绝且当前规则不变。
func TestProviderInteropRulesOptionAPI(t *testing.T) {
	env := newRoutingEnv(t)
	save := func(user model.User, value string) (int, string) {
		t.Helper()
		return env.call("PUT", "/api/option/", "Bearer "+user.AccessToken, map[string]string{"key": common.ProviderInteropRulesOption, "value": value})
	}

	status, _ := save(env.admin, claudeInteropRules)
	require.Equal(t, http.StatusUnauthorized, status, "管理员不能改系统设置")
	require.Nil(t, common.InteropProviders("claude-test", "anthropic"))

	status, body := save(env.root, claudeInteropRules)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `"success":true`)
	require.Equal(t, []string{"anthropic", "vertex"}, common.InteropProviders("claude-test", "vertex"), "保存后立即生效")
	var stored model.Option
	require.NoError(t, env.db.First(&stored, "key = ?", common.ProviderInteropRulesOption).Error)
	require.Equal(t, claudeInteropRules, stored.Value)
	status, body = env.call("GET", "/api/option/", "Bearer "+env.root.AccessToken, nil)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `"key":"ProviderInteropRules"`)
	require.Contains(t, body, `^claude-`)

	// 不合法的规则在保存时被拒绝：库里和当前生效的规则都不变。
	for name, value := range map[string]string{
		"只有一个 provider": `[{"models":["^claude-"],"providers":["anthropic"]}]`,
		"正则不能编译":        `[{"models":["claude-("],"providers":["anthropic","vertex"]}]`,
		"拼错的字段":         `[{"model":["^claude-"],"providers":["anthropic","vertex"]}]`,
		"不是数组":          `{"models":["^claude-"],"providers":["anthropic","vertex"]}`,
		"空的正则":          `[{"models":[""],"providers":["anthropic","vertex"]}]`,
	} {
		status, body = save(env.root, value)
		require.Equal(t, http.StatusOK, status, name)
		require.Contains(t, body, `"success":false`, name)
		require.Contains(t, body, "provider interop", name)
		require.Error(t, model.UpdateOption(common.ProviderInteropRulesOption, value), name)
	}
	require.NoError(t, env.db.First(&stored, "key = ?", common.ProviderInteropRulesOption).Error)
	require.Equal(t, claudeInteropRules, stored.Value)
	require.Equal(t, []string{"anthropic", "vertex"}, common.InteropProviders("claude-test", "anthropic"))

	// 库里的规则不合法（只可能来自直接改库）：从库里加载时报错并保留当前生效的规则。
	require.NoError(t, env.db.Model(&model.Option{}).Where("key = ?", common.ProviderInteropRulesOption).Update("value", `[{"models":["("],"providers":["a","b"]}]`).Error)
	model.InitOptionMap()
	require.Equal(t, []string{"anthropic", "vertex"}, common.InteropProviders("claude-test", "anthropic"))
	// 其他实例按同步周期从库里加载：合法的规则加载后生效。
	require.NoError(t, env.db.Model(&model.Option{}).Where("key = ?", common.ProviderInteropRulesOption).Update("value", `[{"models":["^claude-"],"providers":["anthropic","aws-east"]}]`).Error)
	model.InitOptionMap()
	require.Equal(t, []string{"anthropic", "aws-east"}, common.InteropProviders("claude-test", "anthropic"))
	require.Nil(t, common.InteropProviders("claude-test", "vertex"))

	// 保存 [] 清空规则。
	status, body = save(env.root, "[]")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `"success":true`)
	require.Nil(t, common.InteropProviders("claude-test", "anthropic"))
}

// GET /api/channel/providers：渠道上填写过的 provider 及渠道数，只有管理员可读。
func TestChannelProvidersAPI(t *testing.T) {
	env := newRoutingEnv(t)
	status, _ := env.call("GET", "/api/channel/providers", "", nil)
	require.Equal(t, http.StatusUnauthorized, status)
	status, _ = env.call("GET", "/api/channel/providers", "Bearer "+env.user.AccessToken, nil)
	require.Equal(t, http.StatusUnauthorized, status)

	// 写法不同的同一个 provider 合并计数；config 的其他字段写坏了也照常统计。
	require.NoError(t, env.db.Model(&model.Channel{}).Where("id = ?", 4).Update("config", `{"provider":" VERTEX ","vertex_model_region":"{bad json"}`).Error)
	status, body := env.call("GET", "/api/channel/providers", "Bearer "+env.admin.AccessToken, nil)
	require.Equal(t, http.StatusOK, status)
	require.JSONEq(t, `{"success":true,"message":"","data":[
		{"provider":"ali-direct","channel_count":1,"enabled_count":1},
		{"provider":"anthropic","channel_count":2,"enabled_count":2},
		{"provider":"kling-direct","channel_count":1,"enabled_count":1},
		{"provider":"openrouter","channel_count":1,"enabled_count":0},
		{"provider":"vertex","channel_count":3,"enabled_count":3}
	]}`, body, "没有填 provider 的渠道不计入；禁用的渠道计入总数")
}

// 首选渠道确定后，互通规则决定本次请求的重试范围：模型和首选渠道的 provider 都命中规则才扩大；
// 没有填 provider 的渠道、带历史状态或显式指定 provider 的 Responses 请求不扩大。
func TestInteropRulesSetRetryScopeAtDistribution(t *testing.T) {
	env := newRoutingEnv(t)
	type selection struct {
		Channel       int      `json:"channel"`
		RetryProvider string   `json:"retry_provider"`
		RetryScope    []string `json:"retry_scope"`
	}
	distribute := func(auth, path, body string, headers ...string) selection {
		t.Helper()
		status, response := env.call("POST", path, auth, body, headers...)
		require.Equal(t, http.StatusOK, status, response)
		var selected selection
		require.NoError(t, json.Unmarshal([]byte(response), &selected), response)
		return selected
	}
	vertexOnly, _ := env.newToken(env.user, `{"order":["vertex"],"only":true}`)
	anthropicOnly, _ := env.newToken(env.user, `{"order":["anthropic"],"only":true}`)
	awsOnly, _ := env.newToken(env.user, `{"order":["aws"],"only":true}`)
	klingOnly, _ := env.newToken(env.user, `{"order":["kling-direct"],"only":true}`)

	// 没有规则：重试范围只有首选渠道的 provider。
	require.Equal(t, selection{Channel: 3, RetryProvider: "vertex"}, distribute(vertexOnly, "/v1/chat/completions", chatBody))

	require.NoError(t, model.UpdateOption(common.ProviderInteropRulesOption, `[
		{"models":["^claude-"],"providers":["anthropic","vertex"]},
		{"models":["^kling-"],"providers":["anthropic","openrouter"]}
	]`))
	scope := []string{"anthropic", "vertex"}
	require.Equal(t, selection{Channel: 3, RetryProvider: "vertex", RetryScope: scope}, distribute(vertexOnly, "/v1/chat/completions", chatBody))
	require.Equal(t, selection{Channel: 1, RetryProvider: "anthropic", RetryScope: scope}, distribute(anthropicOnly, "/v1/messages", chatBody))
	require.Equal(t, selection{Channel: 3, RetryProvider: "vertex", RetryScope: scope},
		distribute(vertexOnly, "/v1/responses", `{"model":"`+routingModel+`","input":"hello"}`), "不带历史的 Responses 请求同样扩大")

	// 没有填 provider 的渠道：没有重试范围，规则不适用。
	require.Equal(t, selection{Channel: 5}, distribute(awsOnly, "/v1/chat/completions", chatBody))
	// 模型匹配的规则不包含首选渠道的 provider。
	require.Equal(t, selection{Channel: 8, RetryProvider: "kling-direct"}, distribute(klingOnly, "/kling/v1/videos/text2video", `{"prompt":"a cat"}`))

	// Responses 历史决定的重试范围不扩大：仍只用原 provider。
	origin, _ := gin.CreateTestContext(httptest.NewRecorder())
	origin.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	origin.Request = origin.Request.WithContext(model.WithRetryProvider(origin.Request.Context(), "vertex"))
	origin.Set("id", env.user.Id)
	origin.Set("channel_id", 3)
	origin.Set("key_index", 0)
	origin.Set("actual_key", "upstream-key-3")
	require.NoError(t, service.RememberResponsesState(origin, []byte(`{"id":"resp_interop","output":[{"type":"reasoning","encrypted_content":"interop-thinking"}]}`), false))
	plain, _ := env.newToken(env.user, "")
	require.Equal(t, selection{Channel: 3, RetryProvider: "vertex"},
		distribute(plain, "/v1/responses", `{"model":"`+routingModel+`","input":[{"type":"reasoning","encrypted_content":"interop-thinking"}]}`))
	require.Equal(t, selection{Channel: 3, RetryProvider: "vertex"},
		distribute(plain, "/v1/responses", `{"model":"`+routingModel+`","previous_response_id":"resp_interop","input":"next"}`))
	// 显式指定 provider 的请求也不扩大。
	require.Equal(t, selection{Channel: 3, RetryProvider: "vertex"},
		distribute(plain, "/v1/responses", `{"model":"`+routingModel+`","input":"hello"}`, "X-Linkinfra-Provider", "vertex"))

	// 清空规则后恢复原样。
	require.NoError(t, model.UpdateOption(common.ProviderInteropRulesOption, "[]"))
	require.Equal(t, selection{Channel: 3, RetryProvider: "vertex"}, distribute(vertexOnly, "/v1/chat/completions", chatBody))
}

// 全链路：真实的鉴权、分发和重试循环，上游用本地服务模拟（每个渠道一个，记录被请求的先后）。
// 对照计划里的示例表，逐行核对首选和失败重试的去向。
//
// 渠道（优先级互不相同，先后因此是确定的）：
//
//	anthropic #1（100）、#2（40）；vertex #3（90）、#4（30）；aws #5（80）、#6（20）
func TestInteropRetryOrderEndToEnd(t *testing.T) {
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
	t.Cleanup(func() {
		model.DB, model.LOG_DB, util.HTTPClient = oldDB, oldLogDB, oldClient
		common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled = oldRedis, oldBatch, oldLog
		config.RetryTimes, config.AutomaticDisableChannelEnabled, config.MemoryCacheEnabled = oldRetry, oldDisable, oldMemory
		config.ModelMetricsEnabled, config.ModelMetricsV2Enabled, common.GroupRatio = oldMetrics, oldMetricsV2, oldGroups
		require.NoError(t, common.UpdateProviderInteropRulesByJSONString(""))
		_ = sqlDB.Close()
	})
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled = false, false, true
	config.RetryTimes, config.AutomaticDisableChannelEnabled, config.MemoryCacheEnabled = 6, false, false
	config.ModelMetricsEnabled, config.ModelMetricsV2Enabled = false, false
	const group = "interop-e2e"
	common.GroupRatio = map[string]float64{group: 1}

	user := model.User{Username: "interop-e2e", Group: group, Status: common.UserStatusEnabled, Quota: 100000000, AccessToken: "interop-e2e-access", AffCode: "interop-e2e"}
	require.NoError(t, db.Create(&user).Error)

	// 每个渠道一个上游：记录请求到达的先后；healthy 里的渠道返回成功，其余返回 529。
	var mu sync.Mutex
	var hits []int
	healthy := map[int]bool{}
	const upstreamResponse = `{"model":"jev-1.13.0","answers":{"check":{"type":"noul","noul":0.99}},"usage":{"input_tokens":1000,"output_tokens":200}}`
	for _, spec := range []struct {
		id       int
		provider string
		priority int64
	}{
		{1, "anthropic", 100}, {2, "anthropic", 40},
		{3, "vertex", 90}, {4, "vertex", 30},
		{5, "aws", 80}, {6, "aws", 20},
	} {
		id := spec.id
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits = append(hits, id)
			ok := healthy[id]
			mu.Unlock()
			_, _ = io.Copy(io.Discard, r.Body)
			if !ok {
				w.WriteHeader(529)
				_, _ = io.WriteString(w, `{"detail":"overloaded"}`)
				return
			}
			_, _ = io.WriteString(w, upstreamResponse)
		}))
		t.Cleanup(server.Close)
		util.HTTPClient = server.Client()
		weight, baseURL := uint(1), server.URL
		channel := model.Channel{Id: id, Name: fmt.Sprintf("%s-%d", spec.provider, id), Type: common.ChannelTypeTypesafe, Key: fmt.Sprintf("upstream-key-%d", id), BaseURL: &baseURL,
			Status: common.ChannelStatusEnabled, Models: "jev-latest", Group: group, Priority: &spec.priority, Weight: &weight, Config: `{"provider":"` + spec.provider + `"}`}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, db.Create(&model.Ability{ChannelId: id, Group: group, Model: "jev-latest", Enabled: true, Priority: &spec.priority}).Error)
	}

	r := gin.New()
	r.Use(sessions.Sessions("interop", cookie.NewStore([]byte("test-session-secret-only"))))
	SetRelayRouter(r)
	gateway := httptest.NewServer(r)
	t.Cleanup(gateway.Close)

	tokenCount := 0
	// attempt 用带指定路由的新令牌发一次请求，返回状态码、上游被请求的先后和请求 ID。
	attempt := func(routing model.TokenProviderRouting, healthyChannels ...int) (int, []int, string) {
		t.Helper()
		tokenCount++
		token := model.Token{UserId: user.Id, Name: fmt.Sprintf("t%d", tokenCount), Key: fmt.Sprintf("interope2e%d", tokenCount), Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true, ProviderRouting: routing}
		require.NoError(t, token.Insert())
		mu.Lock()
		hits = nil
		healthy = map[int]bool{}
		for _, id := range healthyChannels {
			healthy[id] = true
		}
		mu.Unlock()
		requestID := fmt.Sprintf("interop-e2e-%d", tokenCount)
		payload, err := json.Marshal(map[string]interface{}{
			"model": "jev-latest", "state": "hello",
			"questions": map[string]interface{}{"check": map[string]string{"type": "noul", "instructions": "Greeting?"}},
		})
		require.NoError(t, err)
		req, err := http.NewRequest("POST", gateway.URL+"/v1/systemone", bytes.NewReader(payload))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer sk-"+token.Key)
		req.Header.Set("X-Request-ID", requestID)
		res, err := gateway.Client().Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		mu.Lock()
		defer mu.Unlock()
		return res.StatusCode, append([]int(nil), hits...), requestID
	}
	order := func(routing model.TokenProviderRouting) []int {
		t.Helper()
		status, sequence, _ := attempt(routing)
		require.Equal(t, 529, status, "所有上游都失败时返回最后一次的错误")
		return sequence
	}
	none := model.TokenProviderRouting{}

	// 没有互通规则：重试只在首选渠道的 provider 内，令牌的顺序不能让请求中途换 provider。
	require.Equal(t, []int{1, 2}, order(none))
	require.Equal(t, []int{3, 4}, order(model.TokenProviderRouting{Order: []string{"vertex"}}))

	require.NoError(t, common.UpdateProviderInteropRulesByJSONString(`[{"models":["^jev-"],"providers":["anthropic","vertex","aws"]}]`))

	// 示例表逐行核对。
	require.Equal(t, []int{1, 3, 5, 2, 4, 6}, order(none), "不设置：三个来源之间都可以，按管理员的优先级")
	require.Equal(t, []int{3, 4, 1, 5, 2, 6}, order(model.TokenProviderRouting{Order: []string{"vertex"}}), "先 vertex，再到 anthropic、aws")
	require.Equal(t, []int{3, 4, 1, 2}, order(model.TokenProviderRouting{Order: []string{"vertex", "anthropic"}, Only: true}), "先 vertex，再到 anthropic，不会到 aws")
	require.Equal(t, []int{3, 4}, order(model.TokenProviderRouting{Order: []string{"vertex"}, Only: true}), "只在 vertex 的渠道内，用完就报错")
	require.Equal(t, []int{1, 3, 2, 4}, order(model.TokenProviderRouting{Ignore: []string{"aws"}}), "anthropic 与 vertex 之间，不含 aws")

	// 重试到另一个 provider 后成功：请求成功，消耗记在最后成功的渠道上。
	status, sequence, requestID := attempt(none, 5)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []int{1, 3, 5}, sequence)
	var logs []model.Log
	require.NoError(t, db.Where("x_request_id = ? AND type = ?", requestID, model.LogTypeConsume).Find(&logs).Error)
	require.Len(t, logs, 1)
	require.Equal(t, 5, logs[0].ChannelId)
	require.Contains(t, logs[0].Other, "retryHistory:")
	// 被忽略的来源即使是唯一健康的也不会用到。
	status, sequence, _ = attempt(model.TokenProviderRouting{Ignore: []string{"aws"}}, 5, 6)
	require.Equal(t, 529, status)
	require.Equal(t, []int{1, 3, 2, 4}, sequence)

	// 重试次数仍受系统设置限制：排在后面的来源可能轮不到。
	config.RetryTimes = 2
	require.Equal(t, []int{3, 4, 1}, order(model.TokenProviderRouting{Order: []string{"vertex"}}))
	config.RetryTimes = 0
	require.Equal(t, []int{3}, order(model.TokenProviderRouting{Order: []string{"vertex"}}))

	// 规则里的模型不匹配时不扩大。
	config.RetryTimes = 6
	require.NoError(t, common.UpdateProviderInteropRulesByJSONString(`[{"models":["^claude-"],"providers":["anthropic","vertex","aws"]}]`))
	require.Equal(t, []int{1, 2}, order(none))
}
