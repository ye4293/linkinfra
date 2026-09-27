package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestSystemOneRouteAuthRetryAndLogs(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Log{}))
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldRedis, oldBatch, oldLog := common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled
	oldRetry, oldDisable, oldMemory := config.RetryTimes, config.AutomaticDisableChannelEnabled, config.MemoryCacheEnabled
	oldClient, oldGroups := util.HTTPClient, common.GroupRatio
	oldMetrics, oldMetricsV2 := config.ModelMetricsEnabled, config.ModelMetricsV2Enabled
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled = oldRedis, oldBatch, oldLog
		config.RetryTimes, config.AutomaticDisableChannelEnabled, config.MemoryCacheEnabled = oldRetry, oldDisable, oldMemory
		util.HTTPClient, common.GroupRatio = oldClient, oldGroups
		config.ModelMetricsEnabled, config.ModelMetricsV2Enabled = oldMetrics, oldMetricsV2
	})
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled = false, false, true
	config.RetryTimes, config.AutomaticDisableChannelEnabled, config.MemoryCacheEnabled = 1, false, false
	common.GroupRatio = map[string]float64{"systemone-route": 1}
	config.ModelMetricsEnabled, config.ModelMetricsV2Enabled = true, true
	user := model.User{Username: "systemone-route", Group: "systemone-route", Status: common.UserStatusEnabled, Quota: 100000}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "systemoneroute", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 100000}
	require.NoError(t, db.Create(&token).Error)
	var calls [2]atomic.Int32
	const response = `{"model":"jev-1.13.0","answers":{"check":{"type":"noul","noul":0.99}},"usage":{"input_tokens":1000,"output_tokens":200}}`
	for index := 0; index < 2; index++ {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls[index].Add(1)
			assert.Equal(t, "/v1/systemone", r.URL.Path)
			if index == 0 {
				assert.Equal(t, "Bearer first-channel-override", r.Header.Get("Authorization"))
			} else {
				assert.Equal(t, "Bearer upstream-test", r.Header.Get("Authorization"))
				assert.Empty(t, r.Header.Get("X-First-Only"))
			}
			var body struct {
				Model string `json:"model"`
				State string `json:"state"`
			}
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, "jev-latest", body.Model)
			if body.State == "rate" {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(429)
				_, _ = io.WriteString(w, `{"detail":"TypeSafe rate limit reached"}`)
				return
			}
			if body.State == "forbidden" {
				w.WriteHeader(403)
				_, _ = io.WriteString(w, `{"detail":"content violates usage guidelines"}`)
				return
			}
			if body.State == "invalid" {
				w.WriteHeader(422)
				_, _ = io.WriteString(w, `{"detail":"invalid question"}`)
				return
			}
			if body.State == "retry" && index == 0 {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(529)
				_, _ = io.WriteString(w, `{"detail":"overloaded"}`)
				return
			}
			_, _ = io.WriteString(w, response)
		}))
		t.Cleanup(server.Close)
		util.HTTPClient = server.Client()
		priority := int64(2 - index)
		ch := model.Channel{Type: common.ChannelTypeCustom, Key: "upstream-test", Name: "TypeSafe", Models: "jev-latest", Group: user.Group, BaseURL: &server.URL, Priority: &priority, Config: `{"provider":"typesafe"}`}
		if index == 0 {
			override := `{"Authorization":"Bearer first-channel-override","X-First-Only":"first"}`
			ch.HeaderOverride = &override
		}
		require.NoError(t, ch.Insert())
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if c.GetHeader("X-Test-Skip-Retry") == "true" {
			c.Set("affinity_skip_retry", true)
		}
		c.Next()
	})
	SetRelayRouter(r)
	for _, auth := range []string{"", "Bearer invalid", "Bearer sk-systemoneroute"} {
		req := httptest.NewRequest("POST", "/v1/systemone", strings.NewReader(`{"model":"jev-latest","state":"hello","questions":{"check":{"type":"noul","instructions":"Greeting?"}}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", auth)
		req.Header.Set("X-Request-ID", "route-success")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if auth != "Bearer sk-systemoneroute" {
			assert.Equal(t, 401, w.Code)
			assert.Zero(t, calls[0].Load())
		} else {
			require.Equal(t, 200, w.Code, w.Body.String())
			assert.Equal(t, response, w.Body.String())
			assert.Empty(t, w.Header().Get("Retry-After"))
		}
	}
	for _, state := range []string{"invalid", "retry"} {
		req := httptest.NewRequest("POST", "/v1/systemone", strings.NewReader(`{"model":"jev-latest","state":"`+state+`","questions":{"check":{"type":"noul","instructions":"Greeting?"}}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer sk-systemoneroute")
		req.Header.Set("X-Request-ID", "route-"+state)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if state == "invalid" {
			require.Equal(t, 422, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), "invalid question")
		} else {
			require.Equal(t, 200, w.Code, w.Body.String())
			assert.Equal(t, response, w.Body.String())
		}
	}
	assert.EqualValues(t, 3, calls[0].Load())
	assert.EqualValues(t, 1, calls[1].Load())
	require.NoError(t, db.First(&user, user.Id).Error)
	require.NoError(t, db.First(&token, token.Id).Error)
	assert.EqualValues(t, 99958, user.Quota)
	assert.EqualValues(t, 99958, token.RemainQuota)
	assert.EqualValues(t, 42, user.UsedQuota)
	var logs []model.Log
	require.NoError(t, db.Order("id").Find(&logs).Error)
	require.Len(t, logs, 3)
	assert.Equal(t, model.LogTypeConsume, logs[0].Type)
	assert.Equal(t, 1000, logs[0].PromptTokens)
	assert.Equal(t, 200, logs[0].CompletionTokens)
	assert.Equal(t, model.LogTypeError, logs[1].Type)
	assert.Zero(t, logs[1].Quota)
	assert.Equal(t, "route-invalid", logs[1].XRequestID)
	assert.Equal(t, model.LogTypeConsume, logs[2].Type)
	assert.Equal(t, 21, logs[2].Quota)
	assert.Equal(t, "route-retry", logs[2].XRequestID)
	assert.Contains(t, logs[2].Other, "retryHistory:")
	assert.Equal(t, 2, logs[2].MetricsVersion)
	assert.Equal(t, "typesafe", logs[2].MetricsSourceKey)
	var attempts []model.MetricsAttempt
	require.NoError(t, json.Unmarshal([]byte(logs[2].MetricsAttempts), &attempts))
	require.Len(t, attempts, 2)
	assert.Equal(t, "error", attempts[0].Outcome)
	assert.Equal(t, "success", attempts[1].Outcome)
	assert.EqualValues(t, 1000, attempts[1].Prompt)
	assert.EqualValues(t, 200, attempts[1].Completion)
	for _, tc := range []struct {
		name, state string
		skip        bool
		status      int
		message     string
		calls       int32
	}{
		{"rate", "rate", false, 429, "TypeSafe rate limit reached", 2},
		{"forbidden", "forbidden", false, 403, "content violates usage guidelines", 1},
		{"skip", "rate", true, 429, "TypeSafe rate limit reached", 1},
		{"quota", "hello", false, 403, "insufficient", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls[0].Load() + calls[1].Load()
			if tc.name == "quota" {
				require.NoError(t, db.Model(&model.Token{}).Where("id = ?", token.Id).Update("remain_quota", 1).Error)
			}
			req := httptest.NewRequest("POST", "/v1/systemone", strings.NewReader(`{"model":"jev-latest","state":"`+tc.state+`","questions":{"check":{"type":"noul","instructions":"Greeting?"}}}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer sk-systemoneroute")
			req.Header.Set("X-Request-ID", "review-"+tc.name)
			if tc.skip {
				req.Header.Set("X-Test-Skip-Retry", "true")
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), tc.message)
			require.Equal(t, "review-"+tc.name, w.Header().Get("X-Request-ID"))
			if tc.state == "rate" {
				require.Equal(t, "7", w.Header().Get("Retry-After"))
			}
			require.Equal(t, tc.calls, calls[0].Load()+calls[1].Load()-before)
			require.NoError(t, db.First(&user, user.Id).Error)
			require.EqualValues(t, 99958, user.Quota)
			var entries []model.Log
			require.NoError(t, db.Where("x_request_id = ?", "review-"+tc.name).Find(&entries).Error)
			require.Len(t, entries, 1)
			require.Equal(t, model.LogTypeError, entries[0].Type)
			require.Zero(t, entries[0].Quota)
		})
	}
}
