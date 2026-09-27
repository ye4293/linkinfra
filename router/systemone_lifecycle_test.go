package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
)

// 默认模拟上游；显式提供环境变量时验证真实 TypeSafe，全程使用隔离业务库。
func TestSystemOneChannelLifecycle(t *testing.T) {
	runSystemOneChannelLifecycle(t, false)
}
func TestSystemOneChannelLifecycleLive(t *testing.T) {
	if os.Getenv("TYPESAFE_LIVE_API_KEY") == "" {
		t.Skip("TYPESAFE_LIVE_API_KEY is not set")
	}
	runSystemOneChannelLifecycle(t, true)
}
func runSystemOneChannelLifecycle(t *testing.T, live bool) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Log{}))
	oldDB, oldLog, oldClient := model.DB, model.LOG_DB, util.HTTPClient
	oldRedis, oldBatch, oldMemory, oldLogs := common.RedisEnabled, config.BatchUpdateEnabled, config.MemoryCacheEnabled, config.LogConsumeEnabled
	oldRetry, oldDisable, oldMetrics := config.RetryTimes, config.AutomaticDisableChannelEnabled, config.ModelMetricsV2Enabled
	oldGroups := common.GroupRatio
	t.Cleanup(func() {
		model.DB, model.LOG_DB, util.HTTPClient = oldDB, oldLog, oldClient
		common.RedisEnabled, config.BatchUpdateEnabled, config.MemoryCacheEnabled, config.LogConsumeEnabled = oldRedis, oldBatch, oldMemory, oldLogs
		config.RetryTimes, config.AutomaticDisableChannelEnabled, config.ModelMetricsV2Enabled = oldRetry, oldDisable, oldMetrics
		common.GroupRatio = oldGroups
		sqlDB.Close()
	})
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled, config.BatchUpdateEnabled, config.MemoryCacheEnabled, config.LogConsumeEnabled = false, true, false, true
	config.RetryTimes, config.AutomaticDisableChannelEnabled, config.ModelMetricsV2Enabled = 0, false, false
	common.GroupRatio = map[string]float64{"typesafe-e2e": 1}
	admin := model.User{Username: "typesafe-admin", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, AccessToken: "typesafe-admin-access", Group: "typesafe-e2e", Quota: 1000000}
	require.NoError(t, db.Create(&admin).Error)
	token := model.Token{UserId: admin.Id, Key: "typesafee2e", Name: "typesafe-e2e", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000000}
	require.NoError(t, db.Create(&token).Error)
	key, baseURL := "upstream-e2e-key", ""
	if live {
		key = os.Getenv("TYPESAFE_LIVE_API_KEY")
	} else {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "Bearer "+key, r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/v1/models" {
				_, _ = io.WriteString(w, `{"models":[{"name":"jev-latest"},{"name":"jev-preview"}]}`)
				return
			}
			require.Equal(t, "/v1/systemone", r.URL.Path)
			var body struct {
				Model     string                 `json:"model"`
				Questions map[string]interface{} `json:"questions"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "jev-latest", body.Model)
			answers := map[string]interface{}{}
			for id := range body.Questions {
				answers[id] = map[string]interface{}{"type": "noul", "noul": 0.99}
			}
			require.NoError(t, json.NewEncoder(w).Encode(map[string]interface{}{"model": "jev-1.13.0", "answers": answers, "usage": map[string]int{"input_tokens": 3000, "output_tokens": 20}}))
		}))
		t.Cleanup(upstream.Close)
		baseURL = upstream.URL
	}
	util.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	r := gin.New()
	r.Use(sessions.Sessions("e2e", cookie.NewStore([]byte("test-session-secret-only"))))
	SetApiRouter(r)
	SetRelayRouter(r)
	gateway := httptest.NewServer(r)
	t.Cleanup(gateway.Close)
	call := func(method, path, auth string, payload interface{}) (int, []byte) {
		var data []byte
		if payload != nil {
			data, err = json.Marshal(payload)
			require.NoError(t, err)
		}
		req, err := http.NewRequest(method, gateway.URL+path, bytes.NewReader(data))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", auth)
		req.Header.Set("X-Request-ID", "typesafe-lifecycle")
		res, err := gateway.Client().Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return res.StatusCode, body
	}
	status, _ := call("POST", "/api/channel", "", map[string]interface{}{"type": 50})
	require.Equal(t, 401, status)
	auth := "Bearer typesafe-admin-access"
	status, body := call("GET", "/api/channel/types", auth, nil)
	require.Equal(t, 200, status)
	require.Contains(t, string(body), `"text":"TypeSafe"`)
	require.Contains(t, string(body), "https://api.typesafe.ai")
	_, body = call("POST", "/api/channel", auth, map[string]interface{}{"name": "TypeSafe lifecycle", "type": 50, "key": key, "base_url": baseURL, "models": "jev-latest,jev-preview,jev-1.13.0", "group": "typesafe-e2e", "test_model": "jev-latest", "discount": 1})
	var created struct {
		Success   bool `json:"success"`
		ChannelID int  `json:"channel_id"`
	}
	require.NoError(t, json.Unmarshal(body, &created))
	require.True(t, created.Success)
	require.Positive(t, created.ChannelID)
	var abilities int64
	require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", created.ChannelID).Count(&abilities).Error)
	require.EqualValues(t, 3, abilities)
	_, body = call("GET", fmt.Sprintf("/api/channel/fetch_models/%d", created.ChannelID), auth, nil)
	var discovered struct {
		Success bool     `json:"success"`
		Data    []string `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &discovered))
	require.True(t, discovered.Success)
	require.Contains(t, discovered.Data, "jev-latest")
	_, body = call("POST", fmt.Sprintf("/api/channel/test/%d", created.ChannelID), auth, map[string]string{"model": "jev-latest"})
	var checked struct {
		Success bool `json:"success"`
	}
	require.NoError(t, json.Unmarshal(body, &checked))
	require.True(t, checked.Success, string(body))
	// 后台测试有异步响应时间更新；等待该更新完成再销毁隔离数据库。
	require.Eventually(t, func() bool {
		var ch model.Channel
		return db.First(&ch, created.ChannelID).Error == nil && ch.TestTime > 0
	}, time.Second, 10*time.Millisecond)
	request := map[string]interface{}{"model": "jev-latest", "state": "The sky is blue.", "questions": map[string]interface{}{"color": map[string]string{"type": "noul", "instructions": "Does the text mention a color?"}}}
	status, body = call("POST", "/v1/systemone", "Bearer sk-typesafee2e", request)
	require.Equal(t, 200, status, string(body))
	var response struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Type string  `json:"type"`
			Noul float64 `json:"noul"`
		} `json:"answers"`
		Usage struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(body, &response))
	require.Equal(t, "noul", response.Answers["color"].Type)
	require.Positive(t, response.Usage.Input)
	require.NotContains(t, string(body), key)
	// 0.021 = 21 / 1000，使用整数公式独立校验计费。
	expected := (int64(response.Usage.Input)*21 + 999) / 1000
	require.NoError(t, db.First(&admin, admin.Id).Error)
	require.NoError(t, db.First(&token, token.Id).Error)
	require.EqualValues(t, 1000000-expected, admin.Quota)
	require.Equal(t, admin.Quota, token.RemainQuota)
	var logs []model.Log
	require.NoError(t, db.Where("x_request_id = ?", "typesafe-lifecycle").Find(&logs).Error)
	require.Len(t, logs, 1)
	require.EqualValues(t, expected, logs[0].Quota)
	require.Equal(t, response.Usage.Output, logs[0].CompletionTokens)
	require.Equal(t, response.Usage.Input, logs[0].PromptTokens)
	require.False(t, math.IsNaN(response.Answers["color"].Noul))
	require.True(t, strings.HasPrefix(response.Model, "jev-"))
	t.Logf("lifecycle complete: live=%v model=%s input=%d output=%d quota=%d", live, response.Model, response.Usage.Input, response.Usage.Output, expected)
}
