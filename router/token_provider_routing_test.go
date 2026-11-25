package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/redistest"
	"github.com/songquanpeng/one-api/middleware"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/service"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const (
	routingGroup = "routing-e2e"
	routingModel = "claude-test"
)

// routingEnv 是令牌来源路由测试的环境：真实的管理接口，加上只经过鉴权和分发中间件的转发入口
// （处理函数返回选中的渠道，不请求上游）。
type routingEnv struct {
	t       *testing.T
	db      *gorm.DB
	gateway *httptest.Server
	user    model.User
	other   model.User
	admin   model.User
	root    model.User
	// lastContext 是最近一次到达转发处理函数的请求上下文里的令牌路由。
	lastRouting model.TokenProviderRouting
}

// 渠道：anthropic #1（优先级 100）、#2（50）；vertex #3（100）、#4（50）；aws #5（100，没填 provider）。
// 另有 #6 属于别的分组、#7 已禁用，以及 Kling #8 和阿里 #9（各自的默认模型）。
func newRoutingEnv(t *testing.T) *routingEnv {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Log{}, &model.Option{}))
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldRedis, oldMemory, oldAffinity := common.RedisEnabled, config.MemoryCacheEnabled, common.ChannelAffinityConfig
	oldOptions := config.OptionMap
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.RedisEnabled, config.MemoryCacheEnabled, common.ChannelAffinityConfig = oldRedis, oldMemory, oldAffinity
		config.OptionMap = oldOptions
		// 互通规则是进程内的全局状态，测试结束后清空。
		require.NoError(t, common.UpdateProviderInteropRulesByJSONString(""))
		_ = sqlDB.Close()
	})
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled, config.MemoryCacheEnabled = false, false
	config.OptionMap = map[string]string{}

	env := &routingEnv{t: t, db: db}
	env.user = model.User{Username: "route-user", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AccessToken: "routing-user-access", AffCode: "route-user", Group: routingGroup}
	env.other = model.User{Username: "route-other", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AccessToken: "routing-other-access", AffCode: "route-other", Group: "other-group"}
	env.admin = model.User{Username: "route-admin", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, AccessToken: "routing-admin-access", AffCode: "route-admin", Group: routingGroup}
	env.root = model.User{Username: "route-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AccessToken: "routing-root-access", AffCode: "route-root", Group: routingGroup}
	for _, user := range []*model.User{&env.user, &env.other, &env.admin, &env.root} {
		require.NoError(t, db.Create(user).Error)
	}

	add := func(id, channelType int, channelConfig, group, models string, priority int64, enabled bool) {
		t.Helper()
		weight := uint(1)
		status := common.ChannelStatusEnabled
		if !enabled {
			status = common.ChannelStatusManuallyDisabled
		}
		channel := model.Channel{Id: id, Name: fmt.Sprintf("ch-%d", id), Type: channelType, Status: status, Config: channelConfig, Group: group, Models: models, Priority: &priority, Weight: &weight, Key: fmt.Sprintf("upstream-key-%d", id)}
		require.NoError(t, db.Create(&channel).Error)
		for _, name := range strings.Split(models, ",") {
			require.NoError(t, db.Create(&model.Ability{ChannelId: id, Group: group, Model: name, Enabled: enabled, Priority: &priority}).Error)
		}
	}
	add(1, common.ChannelTypeAnthropic, `{"provider":"anthropic"}`, routingGroup, routingModel+",claude-direct-only", 100, true)
	add(2, common.ChannelTypeAnthropic, `{"provider":"anthropic"}`, routingGroup, routingModel, 50, true)
	add(3, common.ChannelTypeVertexAI, `{"provider":"vertex"}`, routingGroup, routingModel, 100, true)
	add(4, common.ChannelTypeVertexAI, `{"provider":"vertex"}`, routingGroup, routingModel, 50, true)
	add(5, common.ChannelTypeAwsClaude, ``, routingGroup, routingModel+",arn:aws:bedrock:internal", 100, true)
	add(6, common.ChannelTypeVertexAI, `{"provider":"vertex"}`, "other-group", routingModel, 100, true)
	add(7, common.ChannelTypeOpenRouter, `{"provider":"openrouter"}`, routingGroup, routingModel, 300, false)
	add(8, common.ChannelTypeKeling, `{"provider":"kling-direct"}`, routingGroup, "kling-v1", 100, true)
	add(9, common.ChannelTypeAli, `{"provider":"ali-direct"}`, routingGroup, "wan2.6-i2v", 100, true)

	r := gin.New()
	r.Use(sessions.Sessions("routing", cookie.NewStore([]byte("test-session-secret-only"))))
	SetApiRouter(r)
	selected := func(c *gin.Context) {
		env.lastRouting = model.TokenProviderRoutingFrom(c.Request.Context())
		channel, err := model.GetChannelById(c.GetInt("channel_id"), true)
		require.NoError(t, err)
		// 模拟转发成功：标记后分发中间件会写回亲和缓存。
		if c.GetHeader("X-Test-Mark-Success") != "" {
			service.MarkAffinityRelaySuccess(c)
		}
		c.JSON(http.StatusOK, gin.H{
			"channel": channel.Id, "source": model.ChannelSource(channel),
			// 首选渠道确定后固定下来的重试范围。
			"retry_provider": model.RetryProvider(c.Request.Context()),
			"retry_scope":    model.RetryProviderScope(c.Request.Context()),
		})
	}
	relay := r.Group("/", middleware.TokenAuth(), middleware.Distribute())
	relay.POST("/v1/chat/completions", selected)
	relay.POST("/v1/messages", selected)
	relay.POST("/v1/responses", selected)
	relay.POST("/kling/v1/videos/text2video", selected)
	relay.GET("/kling/v1/videos/*path", selected)
	relay.POST("/ali/api/v1/services/aigc/video-generation/video-synthesis", selected)
	relay.GET("/ali/api/v1/tasks/:taskId", selected)
	env.gateway = httptest.NewServer(r)
	t.Cleanup(env.gateway.Close)
	return env
}

// call 发请求并返回状态码和响应体。payload 是字符串时原样作为请求体。
func (env *routingEnv) call(method, path, auth string, payload interface{}, headers ...string) (int, string) {
	env.t.Helper()
	var data []byte
	switch body := payload.(type) {
	case nil:
	case string:
		data = []byte(body)
	default:
		var err error
		data, err = json.Marshal(payload)
		require.NoError(env.t, err)
	}
	req, err := http.NewRequest(method, env.gateway.URL+path, bytes.NewReader(data))
	require.NoError(env.t, err)
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := env.gateway.Client().Do(req)
	require.NoError(env.t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	require.NoError(env.t, err)
	return res.StatusCode, string(body)
}

type tokenResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		Id              int                        `json:"id"`
		Key             string                     `json:"key"`
		Name            string                     `json:"name"`
		Status          int                        `json:"status"`
		ProviderRouting model.TokenProviderRouting `json:"provider_routing"`
	} `json:"data"`
}

// tokenAPI 调用令牌接口。body 是完整的 JSON 请求体。
func (env *routingEnv) tokenAPI(method string, user model.User, body string) tokenResult {
	env.t.Helper()
	status, response := env.call(method, "/api/token/", "Bearer "+user.AccessToken, body)
	require.Equal(env.t, http.StatusOK, status, response)
	var result tokenResult
	require.NoError(env.t, json.Unmarshal([]byte(response), &result), response)
	return result
}

// newToken 通过接口创建一个带来源路由的令牌（routing 为空串时不带该字段），返回 "Bearer sk-<key>"。
func (env *routingEnv) newToken(user model.User, routing string) (auth string, id int) {
	env.t.Helper()
	body := `{"name":"routing","expired_time":-1,"unlimited_quota":true}`
	if routing != "" {
		body = `{"name":"routing","expired_time":-1,"unlimited_quota":true,"provider_routing":` + routing + `}`
	}
	created := env.tokenAPI("POST", user, body)
	require.True(env.t, created.Success, created.Message)
	return "Bearer sk-" + created.Data.Key, created.Data.Id
}

// selections 重复发转发请求，返回选中过的渠道 ID（升序）。
func (env *routingEnv) selections(auth, path, body string, headers ...string) []int {
	env.t.Helper()
	seen := map[int]bool{}
	for i := 0; i < 60; i++ {
		status, response := env.call("POST", path, auth, body, headers...)
		require.Equal(env.t, http.StatusOK, status, response)
		var selected struct {
			Channel int `json:"channel"`
		}
		require.NoError(env.t, json.Unmarshal([]byte(response), &selected), response)
		seen[selected.Channel] = true
	}
	ids := make([]int, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

const chatBody = `{"model":"` + routingModel + `","messages":[{"role":"user","content":"hi"}]}`

// 首选渠道按令牌的来源路由决定；路由之内没有可用渠道时返回 503 并说明原因，不换到路由之外的来源。
func TestTokenProviderRoutingDecidesInitialChannel(t *testing.T) {
	env := newRoutingEnv(t)

	plain, _ := env.newToken(env.user, "")
	require.Equal(t, []int{1, 3, 5}, env.selections(plain, "/v1/chat/completions", chatBody), "没有设置路由：管理员的最高档里三个来源都参与")
	require.True(t, env.lastRouting.IsEmpty())

	for name, tc := range map[string]struct {
		routing string
		want    []int
	}{
		"只选 vertex，其余作后备":       {`{"order":["vertex"]}`, []int{3}},
		"vertex 在前，只用选中的来源":     {`{"order":["Vertex","anthropic"],"only":true}`, []int{3}},
		"anthropic 在前":          {`{"order":["anthropic","vertex"],"only":true}`, []int{1}},
		"没填 provider 的渠道按渠道类型名": {`{"order":["aws"],"only":true}`, []int{5}},
		"忽略 aws": {`{"ignore":["AWS"]}`, []int{1, 3}},
		"忽略两个来源": {`{"ignore":["anthropic","vertex"]}`, []int{5}},
		"排在前面的来源当前没有渠道":          {`{"order":["openrouter","anthropic"],"only":true}`, []int{1}},
		"选中的来源没有渠道，only 关闭时用后备层": {`{"order":["openrouter"],"ignore":["aws"]}`, []int{1, 3}},
	} {
		auth, _ := env.newToken(env.user, tc.routing)
		require.Equal(t, tc.want, env.selections(auth, "/v1/chat/completions", chatBody), name)
		require.Equal(t, tc.want, env.selections(auth, "/v1/messages", chatBody), name+"（Claude 原生接口）")
		require.False(t, env.lastRouting.IsEmpty(), name)
	}

	// only 会挡掉所选来源不提供的模型：claude-direct-only 只有 anthropic 有。
	vertexOnly, _ := env.newToken(env.user, `{"order":["vertex"],"only":true}`)
	directOnly := strings.Replace(chatBody, routingModel, "claude-direct-only", 1)
	status, body := env.call("POST", "/v1/chat/completions", vertexOnly, directOnly)
	require.Equal(t, http.StatusServiceUnavailable, status, body)
	require.Contains(t, body, "provider routing of this API key")
	require.Contains(t, body, "order=[vertex] only=true")
	require.Contains(t, body, "claude-direct-only")
	require.Equal(t, []int{1}, env.selections(plain, "/v1/chat/completions", directOnly), "没有设置路由的令牌不受影响")

	// 选中的来源全部下线：同样是 503，不放开到其他来源。
	gone, _ := env.newToken(env.user, `{"order":["openrouter"],"only":true}`)
	status, body = env.call("POST", "/v1/chat/completions", gone, chatBody)
	require.Equal(t, http.StatusServiceUnavailable, status, body)
	require.Contains(t, body, "provider routing of this API key")

	// 忽略了全部来源。
	allIgnored, _ := env.newToken(env.user, `{"ignore":["anthropic","vertex","aws"]}`)
	status, body = env.call("POST", "/v1/chat/completions", allIgnored, chatBody)
	require.Equal(t, http.StatusServiceUnavailable, status, body)
	require.Contains(t, body, "provider routing of this API key")

	// 模型本身不存在时仍是原来的报错，不提路由。
	status, body = env.call("POST", "/v1/chat/completions", vertexOnly, strings.Replace(chatBody, routingModel, "no-such-model", 1))
	require.Equal(t, http.StatusServiceUnavailable, status, body)
	require.Contains(t, body, "There are no channels available for model no-such-model under the current group")
	require.NotContains(t, body, "provider routing")
}

// 不适用令牌路由的请求：管理员在 key 后面指定渠道；Kling 和阿里视频的 GET 查询。
func TestTokenProviderRoutingExemptions(t *testing.T) {
	env := newRoutingEnv(t)

	// 管理员指定渠道：即使令牌忽略了这个渠道的来源，也直接使用它。
	adminAuth, _ := env.newToken(env.admin, `{"ignore":["aws"]}`)
	require.Equal(t, []int{1, 3}, env.selections(adminAuth, "/v1/chat/completions", chatBody))
	require.Equal(t, []int{5}, env.selections(adminAuth+"-5", "/v1/chat/completions", chatBody))
	require.True(t, env.lastRouting.IsEmpty(), "指定渠道的请求不带令牌路由，后续重试也不受它限制")
	// 普通用户不能指定渠道（原有行为）。
	userAuth, _ := env.newToken(env.user, `{"ignore":["aws"]}`)
	status, body := env.call("POST", "/v1/chat/completions", userAuth+"-5", chatBody)
	require.Equal(t, http.StatusForbidden, status, body)

	// 只允许 anthropic 的令牌：Kling 和阿里的提交被挡住，任务查询不受影响。
	strict, _ := env.newToken(env.user, `{"order":["anthropic"],"only":true}`)
	for _, path := range []string{"/kling/v1/videos/text2video", "/ali/api/v1/services/aigc/video-generation/video-synthesis"} {
		status, body = env.call("POST", path, strict, `{"prompt":"a cat"}`)
		require.Equal(t, http.StatusServiceUnavailable, status, path+": "+body)
		require.Contains(t, body, "provider routing of this API key", path)
	}
	for path, channel := range map[string]int{"/kling/v1/videos/text2video/task-1": 8, "/ali/api/v1/tasks/task-1": 9} {
		status, body = env.call("GET", path, strict, nil)
		require.Equal(t, http.StatusOK, status, path+": "+body)
		require.Contains(t, body, fmt.Sprintf(`"channel":%d`, channel), path)
		require.True(t, env.lastRouting.IsEmpty(), path)
	}
	// 允许对应来源时，提交照常通过并带着路由。
	klingOK, _ := env.newToken(env.user, `{"order":["kling-direct","ali-direct"],"only":true}`)
	require.Equal(t, []int{8}, env.selections(klingOK, "/kling/v1/videos/text2video", `{"prompt":"a cat"}`))
	require.False(t, env.lastRouting.IsEmpty())
	require.Equal(t, []int{9}, env.selections(klingOK, "/ali/api/v1/services/aigc/video-generation/video-synthesis", `{"prompt":"a cat"}`))
}

// Responses 历史要求的 provider 或渠道被令牌路由排除：返回 409 并说明原因，不丢弃历史继续请求。
func TestTokenProviderRoutingRejectsExcludedResponsesHistory(t *testing.T) {
	env := newRoutingEnv(t)
	plain, _ := env.newToken(env.user, "")

	// 在 vertex 的 #3 上留下一段历史：一个可携带的密文（只绑定 provider），一个 response id（绑定原渠道）。
	origin, _ := gin.CreateTestContext(httptest.NewRecorder())
	origin.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	origin.Request = origin.Request.WithContext(model.WithRetryProvider(origin.Request.Context(), "vertex"))
	origin.Set("id", env.user.Id)
	origin.Set("channel_id", 3)
	origin.Set("key_index", 0)
	origin.Set("actual_key", "upstream-key-3")
	require.NoError(t, service.RememberResponsesState(origin, []byte(`{"id":"resp_routing","output":[{"type":"reasoning","encrypted_content":"routing-thinking"}]}`), false))

	portable := `{"model":"` + routingModel + `","input":[{"type":"reasoning","encrypted_content":"routing-thinking"}]}`
	bound := `{"model":"` + routingModel + `","previous_response_id":"resp_routing","input":"next"}`

	require.Equal(t, []int{3}, env.selections(plain, "/v1/responses", portable), "没有设置路由：续接在原 provider 的最高档")
	require.Equal(t, []int{3}, env.selections(plain, "/v1/responses", bound))

	// 允许 vertex 的令牌照常续接，即使 vertex 排在后面。
	allowed, _ := env.newToken(env.user, `{"order":["anthropic","vertex"],"only":true}`)
	require.Equal(t, []int{3}, env.selections(allowed, "/v1/responses", portable))
	require.Equal(t, []int{3}, env.selections(allowed, "/v1/responses", bound))

	for name, routing := range map[string]string{
		"忽略了历史的 provider":      `{"ignore":["vertex"]}`,
		"only 里没有历史的 provider": `{"order":["anthropic"],"only":true}`,
	} {
		auth, _ := env.newToken(env.user, routing)
		status, body := env.call("POST", "/v1/responses", auth, portable)
		require.Equal(t, http.StatusConflict, status, name+": "+body)
		require.Contains(t, body, `requires the provider \"vertex\"`, name)
		require.Contains(t, body, "provider routing", name)

		status, body = env.call("POST", "/v1/responses", auth, bound)
		require.Equal(t, http.StatusConflict, status, name+": "+body)
		require.Contains(t, body, "bound to its original channel", name)
		require.Contains(t, body, `provider \"vertex\"`, name)

		// 显式指定的 provider 同样受路由约束。
		status, body = env.call("POST", "/v1/responses", auth, `{"model":"`+routingModel+`","input":"hello"}`, "X-Linkinfra-Provider", "vertex")
		require.Equal(t, http.StatusConflict, status, name+": "+body)
		require.Contains(t, body, "provider routing", name)

		// 不带历史的新请求不受影响，按路由选渠。
		status, body = env.call("POST", "/v1/responses", auth, `{"model":"`+routingModel+`","input":"hello"}`)
		require.Equal(t, http.StatusOK, status, name+": "+body)
		require.NotContains(t, body, `"source":"vertex"`, name)
	}
}

// 亲和规则命中的渠道：令牌路由不允许时跳过；设置了路由的令牌，亲和缓存按令牌分别记录，互不影响。
func TestTokenProviderRoutingAndChannelAffinity(t *testing.T) {
	env := newRoutingEnv(t)
	fake := redistest.Use(t)
	common.ChannelAffinityConfig = common.ChannelAffinitySetting{
		Enabled: true, DefaultTTLSeconds: 600,
		Rules: []common.ChannelAffinityRule{{
			Name: "session", ModelRegex: []string{"^claude-"},
			KeySources:      []common.ChannelAffinityKeySource{{Type: "gjson", Path: "metadata.session"}},
			IncludeRuleName: true,
		}},
	}
	sessionBody := `{"model":"` + routingModel + `","metadata":{"session":"s1"},"messages":[]}`
	mark := []string{"X-Test-Mark-Success", "1"}
	sharedKey := "channel_affinity:v1:session:s1"

	// 没有设置路由的令牌：key 不带令牌范围，首次成功后写入。
	plain, _ := env.newToken(env.user, "")
	first := env.selections(plain, "/v1/chat/completions", sessionBody, mark...)
	require.Len(t, first, 1, "首次成功后粘在同一个渠道上")
	cached, ok := fake.Get(sharedKey)
	require.True(t, ok)
	require.Equal(t, fmt.Sprint(first[0]), cached)

	// 把共用的粘性固定在 aws 的 #5 上。
	fake.Set(sharedKey, "5")
	require.Equal(t, []int{5}, env.selections(plain, "/v1/chat/completions", sessionBody))

	// 设置了路由的两个令牌，各自有自己的 key：一个忽略 aws，一个只用 aws。
	noAWS, noAWSID := env.newToken(env.user, `{"ignore":["aws"]}`)
	onlyAWS, onlyAWSID := env.newToken(env.user, `{"order":["aws"],"only":true}`)
	noAWSKey := fmt.Sprintf("channel_affinity:v1:token=%d:session:s1", noAWSID)
	onlyAWSKey := fmt.Sprintf("channel_affinity:v1:token=%d:session:s1", onlyAWSID)

	picked := env.selections(noAWS, "/v1/chat/completions", sessionBody, mark...)
	require.Len(t, picked, 1)
	require.Contains(t, []int{1, 3}, picked[0], "不读共用 key 里的 #5，也不会选中被忽略的来源")
	cached, ok = fake.Get(noAWSKey)
	require.True(t, ok)
	require.Equal(t, fmt.Sprint(picked[0]), cached)

	require.Equal(t, []int{5}, env.selections(onlyAWS, "/v1/chat/completions", sessionBody, mark...))
	cached, ok = fake.Get(onlyAWSKey)
	require.True(t, ok)
	require.Equal(t, "5", cached)

	// 两个令牌交替请求，各自的粘性都还在；共用的 key 也没有被清掉。
	require.Equal(t, picked, env.selections(noAWS, "/v1/chat/completions", sessionBody, mark...))
	require.Equal(t, []int{5}, env.selections(onlyAWS, "/v1/chat/completions", sessionBody, mark...))
	cached, ok = fake.Get(sharedKey)
	require.True(t, ok, "没有设置路由的令牌的粘性不受影响")
	require.Equal(t, "5", cached)

	// 令牌自己的粘性指向了路由不允许的渠道（例如用户刚把这个来源加进 ignore）：跳过并清除，按正常规则选渠。
	fake.Set(noAWSKey, "5")
	status, body := env.call("POST", "/v1/chat/completions", noAWS, sessionBody)
	require.Equal(t, http.StatusOK, status, body)
	require.NotContains(t, body, `"channel":5`)
	_, ok = fake.Get(noAWSKey)
	require.False(t, ok, "失效的粘性被清除")

	// 粘性优先于顺序：命中的渠道在允许范围内就继续用，即使它的来源排在后面。
	ordered, orderedID := env.newToken(env.user, `{"order":["vertex","anthropic"]}`)
	fake.Set(fmt.Sprintf("channel_affinity:v1:token=%d:session:s1", orderedID), "2")
	require.Equal(t, []int{2}, env.selections(ordered, "/v1/chat/completions", sessionBody))
	// 没有粘性的会话仍按顺序从 vertex 开始。
	require.Equal(t, []int{3}, env.selections(ordered, "/v1/chat/completions", strings.Replace(sessionBody, `"s1"`, `"s2"`, 1)))
}

// 令牌接口：创建和更新来源路由，保存时校验；不带该字段或为 null 时不改动；只改状态的请求不动路由。
func TestTokenProviderRoutingAPI(t *testing.T) {
	env := newRoutingEnv(t)
	stored := func(id int) model.TokenProviderRouting {
		t.Helper()
		token, err := model.GetTokenById(id)
		require.NoError(t, err)
		return token.ProviderRouting
	}
	want := model.TokenProviderRouting{Order: []string{"vertex", "anthropic"}, Only: true, Ignore: []string{"aws", "openrouter"}}

	// 创建：规范化后保存，响应里带规范化的结果。
	created := env.tokenAPI("POST", env.user, `{"name":"api","expired_time":-1,"unlimited_quota":true,"provider_routing":{"order":[" Vertex ","ANTHROPIC"],"only":true,"ignore":["OpenRouter","aws"]}}`)
	require.True(t, created.Success, created.Message)
	require.Equal(t, want, created.Data.ProviderRouting)
	require.Equal(t, want, stored(created.Data.Id))
	id := created.Data.Id

	// 不带路由、路由为 null 的创建：没有设置路由。
	for _, body := range []string{
		`{"name":"none","expired_time":-1,"unlimited_quota":true}`,
		`{"name":"null","expired_time":-1,"unlimited_quota":true,"provider_routing":null}`,
		`{"name":"empty","expired_time":-1,"unlimited_quota":true,"provider_routing":{}}`,
	} {
		none := env.tokenAPI("POST", env.user, body)
		require.True(t, none.Success, none.Message)
		require.True(t, stored(none.Data.Id).IsEmpty(), body)
	}

	// 不合法的路由在保存时被拒绝，不创建令牌，也不改动已有设置。
	var before int64
	require.NoError(t, env.db.Model(&model.Token{}).Count(&before).Error)
	for name, routing := range map[string]string{
		"同一来源既选中又忽略":     `{"order":["vertex"],"ignore":["VERTEX"]}`,
		"only 而没有 order": `{"only":true}`,
		"重复的来源":          `{"order":["vertex","Vertex"]}`,
		"拼错的字段":          `{"order":["vertex"],"ingore":["aws"]}`,
		"不是对象":           `["vertex"]`,
		"字符串":            `"vertex"`,
		"来源名不是字符串":       `{"ignore":[1]}`,
		"空的来源名":          `{"ignore":[" "]}`,
		"来源名超长":          `{"ignore":["` + strings.Repeat("a", 129) + `"]}`,
	} {
		rejected := env.tokenAPI("POST", env.user, `{"name":"bad","expired_time":-1,"unlimited_quota":true,"provider_routing":`+routing+`}`)
		require.False(t, rejected.Success, name)
		require.Contains(t, strings.ToLower(rejected.Message), "provider routing", name)

		rejected = env.tokenAPI("PUT", env.user, fmt.Sprintf(`{"id":%d,"name":"api","expired_time":-1,"unlimited_quota":true,"provider_routing":%s}`, id, routing))
		require.False(t, rejected.Success, name)
		require.Equal(t, want, stored(id), name)
	}
	var after int64
	require.NoError(t, env.db.Model(&model.Token{}).Count(&after).Error)
	require.Equal(t, before, after, "被拒绝的创建请求不留下令牌")

	// 更新时不带 provider_routing 或为 null：不改动已有设置（包括同一个 key 出现两次、后一次是 null）。
	for _, body := range []string{
		fmt.Sprintf(`{"id":%d,"name":"renamed","expired_time":-1,"unlimited_quota":true}`, id),
		fmt.Sprintf(`{"id":%d,"name":"renamed","expired_time":-1,"unlimited_quota":true,"provider_routing":null}`, id),
		fmt.Sprintf(`{"id":%d,"name":"renamed","expired_time":-1,"unlimited_quota":true,"provider_routing":{"ignore":["vertex"]},"provider_routing":null}`, id),
	} {
		updated := env.tokenAPI("PUT", env.user, body)
		require.True(t, updated.Success, updated.Message)
		require.Equal(t, "renamed", updated.Data.Name)
		require.Equal(t, want, updated.Data.ProviderRouting, body)
		require.Equal(t, want, stored(id), body)
	}

	// 只改状态的请求不动路由，即使请求里带了。
	disabled := env.tokenAPI("PUT", env.user, fmt.Sprintf(`{"id":%d,"status_only":true,"status":%d,"provider_routing":{"ignore":["vertex"]}}`, id, common.TokenStatusDisabled))
	require.True(t, disabled.Success, disabled.Message)
	require.Equal(t, common.TokenStatusDisabled, disabled.Data.Status)
	require.Equal(t, want, stored(id))
	env.tokenAPI("PUT", env.user, fmt.Sprintf(`{"id":%d,"status_only":true,"status":%d}`, id, common.TokenStatusEnabled))

	// 修改路由。
	changed := env.tokenAPI("PUT", env.user, fmt.Sprintf(`{"id":%d,"name":"renamed","expired_time":-1,"unlimited_quota":true,"provider_routing":{"ignore":["AWS"]}}`, id))
	require.True(t, changed.Success, changed.Message)
	require.Equal(t, model.TokenProviderRouting{Ignore: []string{"aws"}}, stored(id))

	// 空对象表示清空。
	cleared := env.tokenAPI("PUT", env.user, fmt.Sprintf(`{"id":%d,"name":"renamed","expired_time":-1,"unlimited_quota":true,"provider_routing":{}}`, id))
	require.True(t, cleared.Success, cleared.Message)
	require.True(t, stored(id).IsEmpty())

	// 读取接口返回路由；别的用户改不了这个令牌。
	require.True(t, env.tokenAPI("PUT", env.user, fmt.Sprintf(`{"id":%d,"name":"renamed","expired_time":-1,"unlimited_quota":true,"provider_routing":{"order":["vertex"]}}`, id)).Success)
	status, body := env.call("GET", fmt.Sprintf("/api/token/%d", id), "Bearer "+env.user.AccessToken, nil)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `"provider_routing":{"order":["vertex"],"only":false,"ignore":[]}`)
	foreign := env.tokenAPI("PUT", env.other, fmt.Sprintf(`{"id":%d,"name":"x","expired_time":-1,"unlimited_quota":true,"provider_routing":{"ignore":["vertex"]}}`, id))
	require.False(t, foreign.Success)
	require.Equal(t, model.TokenProviderRouting{Order: []string{"vertex"}}, stored(id))
}

// 更新令牌后路由立即生效（Redis 开启时令牌有缓存）。
func TestTokenProviderRoutingUpdateTakesEffectImmediately(t *testing.T) {
	env := newRoutingEnv(t)
	redistest.Use(t)
	auth, id := env.newToken(env.user, `{"order":["vertex"],"only":true}`)
	require.Equal(t, []int{3}, env.selections(auth, "/v1/chat/completions", chatBody))

	update := func(routing string) {
		t.Helper()
		updated := env.tokenAPI("PUT", env.user, fmt.Sprintf(`{"id":%d,"name":"routing","expired_time":-1,"unlimited_quota":true,"provider_routing":%s}`, id, routing))
		require.True(t, updated.Success, updated.Message)
	}
	update(`{"order":["aws"],"only":true}`)
	require.Equal(t, []int{5}, env.selections(auth, "/v1/chat/completions", chatBody), "下一次请求就按新的路由")
	update(`{}`)
	require.Equal(t, []int{1, 3, 5}, env.selections(auth, "/v1/chat/completions", chatBody), "清空后回到管理员的优先级")

	// 禁用令牌同样立即生效。
	disabled := env.tokenAPI("PUT", env.user, fmt.Sprintf(`{"id":%d,"status_only":true,"status":%d}`, id, common.TokenStatusDisabled))
	require.True(t, disabled.Success, disabled.Message)
	status, body := env.call("POST", "/v1/chat/completions", auth, chatBody)
	require.Equal(t, http.StatusUnauthorized, status, body)
}

// 现有的令牌表单会把读到的令牌原样带回来，其中包含 provider_routing。
// 带回来的内容与已保存的相同时不写这一列：发布期间这一列还不存在时，创建和编辑令牌照常工作，
// 只有真正修改路由才会报错。
func TestTokenFormRoundTripBeforeProviderRoutingColumnExists(t *testing.T) {
	env := newRoutingEnv(t)
	require.NoError(t, env.db.Migrator().DropColumn(&model.Token{}, "provider_routing"))

	created := env.tokenAPI("POST", env.user, `{"name":"legacy","expired_time":-1,"unlimited_quota":true}`)
	require.True(t, created.Success, created.Message)
	id := created.Data.Id

	// 读取时路由是空的三个字段；表单把它和其他字段一起提交回来。
	status, body := env.call("GET", fmt.Sprintf("/api/token/%d", id), "Bearer "+env.user.AccessToken, nil)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `"provider_routing":{"order":[],"only":false,"ignore":[]}`)
	for _, echoed := range []string{`{"order":[],"only":false,"ignore":[]}`, `{}`, `{"order":null,"ignore":null}`} {
		updated := env.tokenAPI("PUT", env.user, fmt.Sprintf(`{"id":%d,"name":"edited","expired_time":-1,"unlimited_quota":true,"provider_routing":%s}`, id, echoed))
		require.True(t, updated.Success, echoed+": "+updated.Message)
		require.Equal(t, "edited", updated.Data.Name)
	}
	auth := "Bearer sk-" + created.Data.Key
	require.Equal(t, []int{1, 3, 5}, env.selections(auth, "/v1/chat/completions", chatBody))

	// 真正设置路由需要这一列：保存失败并报错，不会假装成功。
	rejected := env.tokenAPI("PUT", env.user, fmt.Sprintf(`{"id":%d,"name":"edited","expired_time":-1,"unlimited_quota":true,"provider_routing":{"ignore":["aws"]}}`, id))
	require.False(t, rejected.Success)
	rejected = env.tokenAPI("POST", env.user, `{"name":"routed","expired_time":-1,"unlimited_quota":true,"provider_routing":{"ignore":["aws"]}}`)
	require.False(t, rejected.Success)
}

// GET /api/token/sources：只返回当前用户分组下可用的来源；需要登录。
func TestTokenSourcesAPI(t *testing.T) {
	env := newRoutingEnv(t)

	status, _ := env.call("GET", "/api/token/sources", "", nil)
	require.Equal(t, http.StatusUnauthorized, status)

	status, body := env.call("GET", "/api/token/sources", "Bearer "+env.user.AccessToken, nil)
	require.Equal(t, http.StatusOK, status)
	require.JSONEq(t, `{"success":true,"message":"","data":[
		{"name":"ali-direct","display_name":"ali-direct","models":["wan2.6-i2v"]},
		{"name":"anthropic","display_name":"Anthropic","models":["claude-direct-only","claude-test"]},
		{"name":"aws","display_name":"AWS","models":["claude-test"]},
		{"name":"kling-direct","display_name":"kling-direct","models":["kling-v1"]},
		{"name":"vertex","display_name":"vertex","models":["claude-test"]}
	]}`, body, "不含禁用渠道的来源（openrouter）和内部标识符（ARN）")

	status, body = env.call("GET", "/api/token/sources", "Bearer "+env.other.AccessToken, nil)
	require.Equal(t, http.StatusOK, status)
	require.JSONEq(t, `{"success":true,"message":"","data":[{"name":"vertex","display_name":"vertex","models":["claude-test"]}]}`, body)

	// 路径是静态的，不会被 /api/token/:id 当成令牌 ID。
	status, body = env.call("GET", "/api/token/sources/", "Bearer "+env.user.AccessToken, nil)
	require.Contains(t, []int{http.StatusOK, http.StatusMovedPermanently, http.StatusNotFound}, status, body)
}

// 保存渠道时拒绝不是字符串或超长的 provider；不带 config 的局部更新不受历史数据影响。
func TestChannelProviderValidatedOnSave(t *testing.T) {
	env := newRoutingEnv(t)
	adminAuth := "Bearer " + env.admin.AccessToken
	create := func(channelConfig string) (bool, string) {
		t.Helper()
		_, body := env.call("POST", "/api/channel/", adminAuth, map[string]interface{}{
			"name": "validated", "type": common.ChannelTypeVertexAI, "key": "k", "models": "m", "group": routingGroup, "config": channelConfig,
		})
		var result struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &result), body)
		return result.Success, result.Message
	}
	for _, bad := range []string{`{"provider":123}`, `{"provider":["vertex"]}`, `{"provider":"` + strings.Repeat("a", 129) + `"}`} {
		ok, message := create(bad)
		require.False(t, ok, bad)
		require.Contains(t, message, "Provider must be a string")
	}
	ok, message := create(`{"provider":" Vertex-EU "}`)
	require.True(t, ok, message)

	_, body := env.call("PUT", "/api/channel/", adminAuth, map[string]interface{}{"id": 3, "config": `{"provider":false}`})
	require.Contains(t, body, "Provider must be a string")
	var stored model.Channel
	require.NoError(t, env.db.First(&stored, 3).Error)
	require.Equal(t, "vertex", model.ChannelProvider(&stored), "被拒绝的更新不改动已有配置")

	// 库里已有的不合法 provider（历史数据）：改名这类局部更新照常成功，再次提交 config 时才校验。
	require.NoError(t, env.db.Model(&model.Channel{}).Where("id = ?", 4).Update("config", `{"provider":123}`).Error)
	_, body = env.call("PUT", "/api/channel/", adminAuth, map[string]interface{}{"id": 4, "name": "renamed"})
	require.Contains(t, body, `"success":true`)
	_, body = env.call("PUT", "/api/channel/", adminAuth, map[string]interface{}{"id": 4, "config": `{"provider":123}`})
	require.Contains(t, body, "Provider must be a string")
}
