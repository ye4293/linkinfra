package router

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// count_tokens 接口自己选一个支持计数的渠道。它使用和正式请求同一套令牌路由（包括顺序），
// 被忽略的来源不会用来计数；路由之内没有支持计数的渠道时报错并说明原因。
//
// 渠道（都是 Anthropic 类型，各有一个本地模拟上游）：
//
//	#1 vertex-proxy（优先级 100，不支持计数）、#2 anthropic（90，支持）、#3 partner（80，支持）
func TestCountTokensFollowsTokenProviderRouting(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Log{}))
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldRedis, oldMemory := common.RedisEnabled, config.MemoryCacheEnabled
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.RedisEnabled, config.MemoryCacheEnabled = oldRedis, oldMemory
		_ = sqlDB.Close()
	})
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled, config.MemoryCacheEnabled = false, false
	const group = "count-tokens"
	user := model.User{Username: "count-tokens", Group: group, Status: common.UserStatusEnabled, Quota: 1000000, AccessToken: "count-tokens-access", AffCode: "count-tokens"}
	require.NoError(t, db.Create(&user).Error)

	var mu sync.Mutex
	var hits []int
	for _, spec := range []struct {
		id       int
		config   string
		priority int64
	}{
		{1, `{"provider":"vertex-proxy"}`, 100},
		{2, `{"provider":"anthropic","support_count_tokens":true}`, 90},
		{3, `{"provider":"partner","support_count_tokens":true}`, 80},
	} {
		id := spec.id
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			mu.Lock()
			hits = append(hits, id)
			mu.Unlock()
			if r.URL.Path != "/v1/messages/count_tokens" || r.Header.Get("x-api-key") != fmt.Sprintf("upstream-key-%d", id) {
				w.WriteHeader(http.StatusTeapot)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"input_tokens":%d}`, 100+id)
		}))
		t.Cleanup(server.Close)
		weight, baseURL := uint(1), server.URL
		channel := model.Channel{Id: id, Name: fmt.Sprintf("count-%d", id), Type: common.ChannelTypeAnthropic, Key: fmt.Sprintf("upstream-key-%d", id), BaseURL: &baseURL,
			Status: common.ChannelStatusEnabled, Models: "claude-test", Group: group, Priority: &spec.priority, Weight: &weight, Config: spec.config}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, db.Create(&model.Ability{ChannelId: id, Group: group, Model: "claude-test", Enabled: true, Priority: &spec.priority}).Error)
	}

	r := gin.New()
	r.Use(sessions.Sessions("count-tokens", cookie.NewStore([]byte("test-session-secret-only"))))
	SetRelayRouter(r)
	gateway := httptest.NewServer(r)
	t.Cleanup(gateway.Close)

	tokens := 0
	// count 用带指定路由的新令牌请求一次计数，返回状态码、响应体和被请求的上游。
	count := func(routing model.TokenProviderRouting) (int, string, []int) {
		t.Helper()
		tokens++
		token := model.Token{UserId: user.Id, Name: fmt.Sprintf("t%d", tokens), Key: fmt.Sprintf("counttokens%d", tokens), Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true, ProviderRouting: routing}
		require.NoError(t, token.Insert())
		mu.Lock()
		hits = nil
		mu.Unlock()
		req, err := http.NewRequest("POST", gateway.URL+"/v1/messages/count_tokens", strings.NewReader(`{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "sk-"+token.Key)
		res, err := gateway.Client().Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		mu.Lock()
		defer mu.Unlock()
		return res.StatusCode, string(body), append([]int(nil), hits...)
	}

	for name, tc := range map[string]struct {
		routing model.TokenProviderRouting
		channel int
	}{
		"不设置：支持计数的渠道里优先级最高的": {model.TokenProviderRouting{}, 2},
		"忽略 anthropic": {model.TokenProviderRouting{Ignore: []string{"anthropic"}}, 3},
		"partner 排在前面": {model.TokenProviderRouting{Order: []string{"partner", "anthropic"}}, 3},
		"只用 partner":   {model.TokenProviderRouting{Order: []string{"partner"}, Only: true}, 3},
		"选中的来源不支持计数，落到后备的来源": {model.TokenProviderRouting{Order: []string{"vertex-proxy"}}, 2},
	} {
		status, body, upstream := count(tc.routing)
		require.Equal(t, http.StatusOK, status, name+": "+body)
		require.Equal(t, []int{tc.channel}, upstream, name)
		require.JSONEq(t, fmt.Sprintf(`{"input_tokens":%d}`, 100+tc.channel), body, name)
	}

	// 路由之内只有不支持计数的来源：不换到路由之外的来源，报错并说明原因。
	status, body, upstream := count(model.TokenProviderRouting{Order: []string{"vertex-proxy"}, Only: true})
	require.Equal(t, http.StatusServiceUnavailable, status, body)
	require.Empty(t, upstream)
	require.Contains(t, body, "provider routing")

	// 路由把这个模型的全部来源都排除了：在分发阶段就返回 503。
	status, body, upstream = count(model.TokenProviderRouting{Ignore: []string{"anthropic", "partner", "vertex-proxy"}})
	require.Equal(t, http.StatusServiceUnavailable, status, body)
	require.Empty(t, upstream)
	require.Contains(t, body, "provider routing of this API key")
}
