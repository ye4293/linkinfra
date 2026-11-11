package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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

// 计费标签全链路：管理 API 建渠道并校验标签 → 标签列表及权限 → 用户按标签的折扣 →
// 首选渠道失败后跨渠道重试，折扣跟随最终成功的渠道。两个渠道都不配置 provider，
// 验证打标签不会产生重试边界。
func TestBillingTagDiscountFollowsFinalChannel(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Log{}))
	oldDB, oldLogDB, oldClient := model.DB, model.LOG_DB, util.HTTPClient
	oldRedis, oldBatch, oldLog := common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled
	oldRetry, oldDisable, oldMemory := config.RetryTimes, config.AutomaticDisableChannelEnabled, config.MemoryCacheEnabled
	oldMetricsV2, oldGroups := config.ModelMetricsV2Enabled, common.GroupRatio
	t.Cleanup(func() {
		model.DB, model.LOG_DB, util.HTTPClient = oldDB, oldLogDB, oldClient
		common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled = oldRedis, oldBatch, oldLog
		config.RetryTimes, config.AutomaticDisableChannelEnabled, config.MemoryCacheEnabled = oldRetry, oldDisable, oldMemory
		config.ModelMetricsV2Enabled, common.GroupRatio = oldMetricsV2, oldGroups
		sqlDB.Close()
	})
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled = false, false, true
	config.RetryTimes, config.AutomaticDisableChannelEnabled, config.MemoryCacheEnabled = 1, false, false
	config.ModelMetricsV2Enabled = false
	const group = "billing-tag-e2e"
	common.GroupRatio = map[string]float64{group: 1}

	// 备用渠道类型（Custom）折扣 0.9，标签 ts-primary 折扣 0.5。
	ratios := fmt.Sprintf(`{"%d":0.9,"tag:ts-primary":0.5}`, common.ChannelTypeCustom)
	admin := model.User{Username: "tag-admin", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, AccessToken: "billing-tag-admin-access", Group: group, Quota: 100000, ChannelRatios: ratios}
	require.NoError(t, db.Create(&admin).Error)
	member := model.User{Username: "tag-member", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AccessToken: "billing-tag-member-access", AffCode: "member", Group: group}
	require.NoError(t, db.Create(&member).Error)
	token := model.Token{UserId: admin.Id, Key: "billingtagroute", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 100000}
	require.NoError(t, db.Create(&token).Error)

	// 1000 输入 token 在折扣为 1 时计 21 quota（见 TestSystemOneRouteAuthRetryAndLogs）。
	const upstreamResponse = `{"model":"jev-1.13.0","answers":{"check":{"type":"noul","noul":0.99}},"usage":{"input_tokens":1000,"output_tokens":200}}`
	newUpstream := func(failOnRetryState bool) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				State string `json:"state"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if failOnRetryState && body.State == "retry" {
				w.WriteHeader(529)
				_, _ = io.WriteString(w, `{"detail":"overloaded"}`)
				return
			}
			_, _ = io.WriteString(w, upstreamResponse)
		}))
		t.Cleanup(server.Close)
		return server
	}
	primary, fallback := newUpstream(true), newUpstream(false)
	util.HTTPClient = primary.Client()

	r := gin.New()
	r.Use(sessions.Sessions("billing-tag", cookie.NewStore([]byte("test-session-secret-only"))))
	SetApiRouter(r)
	SetRelayRouter(r)
	gateway := httptest.NewServer(r)
	t.Cleanup(gateway.Close)
	call := func(method, path, auth, requestID string, payload interface{}) (int, string) {
		t.Helper()
		var data []byte
		if payload != nil {
			data, err = json.Marshal(payload)
			require.NoError(t, err)
		}
		req, err := http.NewRequest(method, gateway.URL+path, bytes.NewReader(data))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if requestID != "" {
			req.Header.Set("X-Request-ID", requestID)
		}
		res, err := gateway.Client().Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return res.StatusCode, string(body)
	}
	adminAuth := "Bearer " + admin.AccessToken
	createChannel := func(name string, channelType int, baseURL string, priority int, channelConfig string) (bool, int, string) {
		t.Helper()
		_, body := call("POST", "/api/channel/", adminAuth, "", map[string]interface{}{
			"name": name, "type": channelType, "key": "upstream-key", "base_url": baseURL,
			"models": "jev-latest", "group": group, "priority": priority, "config": channelConfig,
		})
		var created struct {
			Success   bool   `json:"success"`
			Message   string `json:"message"`
			ChannelID int    `json:"channel_id"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &created), body)
		return created.Success, created.ChannelID, created.Message
	}

	// 非法标签在保存时被拒绝，而不是保存后静默失效。
	ok, _, message := createChannel("invalid", common.ChannelTypeTypesafe, primary.URL, 3, `{"billing_tag":"bad tag"}`)
	require.False(t, ok)
	require.Contains(t, message, "Billing tag")
	ok, _, message = createChannel("invalid-type", common.ChannelTypeTypesafe, primary.URL, 3, `{"billing_tag":123}`)
	require.False(t, ok)
	require.Contains(t, message, "Billing tag")
	ok, primaryID, message := createChannel("primary", common.ChannelTypeTypesafe, primary.URL, 2, `{"billing_tag":" TS-Primary "}`)
	require.True(t, ok, message)
	ok, fallbackID, message := createChannel("fallback", common.ChannelTypeCustom, fallback.URL, 1, "")
	require.True(t, ok, message)

	// 更新渠道：提交了非法标签时拒绝且不改动已有配置；不带 config 的局部更新不受影响。
	_, body := call("PUT", "/api/channel/", adminAuth, "", map[string]interface{}{"id": primaryID, "config": `{"billing_tag":"bad tag"}`})
	require.Contains(t, body, "Billing tag")
	_, body = call("PUT", "/api/channel/", adminAuth, "", map[string]interface{}{"id": primaryID, "name": "primary-renamed"})
	require.Contains(t, body, `"success":true`)
	var stored model.Channel
	require.NoError(t, db.First(&stored, primaryID).Error)
	require.Equal(t, "primary-renamed", stored.Name)
	require.Equal(t, "ts-primary", model.ChannelBillingTag(&stored))

	// 标签列表：仅管理员可读，标签已规范化并带渠道数。
	status, _ := call("GET", "/api/channel/billing_tags", "", "", nil)
	require.Equal(t, 401, status)
	status, _ = call("GET", "/api/channel/billing_tags", "Bearer "+member.AccessToken, "", nil)
	require.Equal(t, 401, status)
	status, body = call("GET", "/api/channel/billing_tags", adminAuth, "", nil)
	require.Equal(t, 200, status)
	require.JSONEq(t, `{"success":true,"message":"","data":[{"tag":"ts-primary","channel_count":1}]}`, body)

	relay := func(state, requestID string) model.Log {
		t.Helper()
		status, body := call("POST", "/v1/systemone", "Bearer sk-"+token.Key, requestID, map[string]interface{}{
			"model": "jev-latest", "state": state,
			"questions": map[string]interface{}{"check": map[string]string{"type": "noul", "instructions": "Greeting?"}},
		})
		require.Equal(t, 200, status, body)
		var logs []model.Log
		require.NoError(t, db.Where("x_request_id = ? AND type = ?", requestID, model.LogTypeConsume).Find(&logs).Error)
		require.Len(t, logs, 1)
		return logs[0]
	}

	// 首选渠道成功：命中标签折扣 0.5，21 × 0.5 向上取整为 11。
	direct := relay("hello", "billing-tag-direct")
	require.Equal(t, primaryID, direct.ChannelId)
	require.Equal(t, 11, direct.Quota)
	require.Contains(t, direct.Other, `"billing_tag":"ts-primary"`)
	require.Contains(t, direct.Other, `"user_channel_ratio":0.5`)

	// 首选渠道失败后重试到无标签的备用渠道：按备用渠道的类型折扣 0.9，21 × 0.9 向上取整为 19，
	// 日志不再带上一渠道的标签。
	retried := relay("retry", "billing-tag-retry")
	require.Equal(t, fallbackID, retried.ChannelId)
	require.Equal(t, 19, retried.Quota)
	require.Contains(t, retried.Other, "retryHistory:")
	require.NotContains(t, retried.Other, "billing_tag")
	require.Contains(t, retried.Other, `"user_channel_ratio":0.9`)

	require.NoError(t, db.First(&admin, admin.Id).Error)
	require.EqualValues(t, 100000-11-19, admin.Quota)
}
