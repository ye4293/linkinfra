package controller

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/middleware"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/service"
	"github.com/stretchr/testify/require"
)

func TestThinkingConstrainsInitialDistribution(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	oldRedis, oldMemory := common.RedisEnabled, config.MemoryCacheEnabled
	common.RedisEnabled, config.MemoryCacheEnabled = false, false
	t.Cleanup(func() { common.RedisEnabled, config.MemoryCacheEnabled = oldRedis, oldMemory })
	require.NoError(t, db.Create(&model.User{Id: 3141, Username: "state-route-test", Group: "default"}).Error)
	for id, provider := range []string{"openai", "azure"} {
		priority := int64(100 - id*10)
		weight := uint(1)
		channel := model.Channel{Id: id + 1, Type: common.ChannelTypeOpenAI, Key: "test-key", Models: "gpt-test", Group: "default", Status: common.ChannelStatusEnabled, Priority: &priority, Weight: &weight, Config: `{"provider":"` + provider + `"}`}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, db.Create(&model.Ability{ChannelId: id + 1, Group: "default", Model: "gpt-test", Enabled: true, Priority: &priority}).Error)
	}
	origin, _ := gin.CreateTestContext(httptest.NewRecorder())
	origin.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	origin.Request = origin.Request.WithContext(model.WithRetryProvider(origin.Request.Context(), "azure"))
	origin.Set("id", 3141)
	origin.Set("channel_id", 2)
	origin.Set("key_index", 0)
	origin.Set("actual_key", "test-key")
	require.NoError(t, service.RememberResponsesState(origin, []byte(`{"id":"resp_route_test","output":[{"type":"reasoning","encrypted_content":"route-thinking"}]}`), false))
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("id", 3141)
		if forced := c.GetHeader("X-Test-Channel"); forced != "" {
			c.Set("specific_channel_id", forced)
		}
	})
	r.POST("/v1/responses", middleware.Distribute(), func(c *gin.Context) {
		raw, err := common.GetRequestBody(c)
		require.NoError(t, err)
		c.JSON(200, gin.H{"channel": c.GetInt("channel_id"), "body": string(raw)})
	})
	for _, test := range []struct {
		body            string
		channel, status int
		forced          int
	}{
		{`{"model":"gpt-test","input":"hello"}`, 1, 200, 0},
		{`{"model":"gpt-test","input":[{"type":"reasoning","encrypted_content":"route-thinking"}]}`, 2, 200, 0},
		{`{"model":"gpt-test","previous_response_id":"resp_route_test","input":"next"}`, 2, 200, 0},
		{`{"model":"gpt-test","input":[{"type":"reasoning","encrypted_content":"route-thinking"}]}`, 0, 409, 1},
		{`{"model":"gpt-test","input":[{"type":"reasoning","encrypted_content":"unknown"}]}`, 0, 409, 0},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(test.body))
		req.Header.Set("Content-Type", "application/json")
		if test.forced != 0 {
			req.Header.Set("X-Test-Channel", strconv.Itoa(test.forced))
		}
		r.ServeHTTP(w, req)
		require.Equal(t, test.status, w.Code, w.Body.String())
		if test.status == 200 {
			var result struct {
				Channel int    `json:"channel"`
				Body    string `json:"body"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
			require.Equal(t, test.channel, result.Channel)
			require.Equal(t, test.body, result.Body)
		}
	}
}

func TestProviderBackfillKeepsExistingResponsesConversation(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	oldRedis, oldMemory := common.RedisEnabled, config.MemoryCacheEnabled
	common.RedisEnabled, config.MemoryCacheEnabled = false, false
	t.Cleanup(func() { common.RedisEnabled, config.MemoryCacheEnabled = oldRedis, oldMemory })
	const userID = 3142
	require.NoError(t, db.Create(&model.User{Id: userID, Username: "provider-backfill-test", Group: "default"}).Error)
	priority, weight := int64(100), uint(1)
	channel := model.Channel{Id: 1, Type: common.ChannelTypeOpenAI, Key: "original-key", Models: "gpt-test", Group: "default", Status: common.ChannelStatusEnabled, Priority: &priority, Weight: &weight}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, db.Create(&model.Ability{ChannelId: 1, Group: "default", Model: "gpt-test", Enabled: true, Priority: &priority}).Error)
	// 同 provider 的另一渠道也不能承接原来没有 provider 的历史。
	other := channel
	other.Id, other.Config = 2, `{"provider":"azure"}`
	require.NoError(t, db.Create(&other).Error)
	require.NoError(t, db.Create(&model.Ability{ChannelId: 2, Group: "default", Model: "gpt-test", Enabled: true, Priority: &priority}).Error)
	origin, _ := gin.CreateTestContext(httptest.NewRecorder())
	origin.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	origin.Set("id", userID)
	origin.Set("channel_id", 1)
	origin.Set("key_index", 0)
	origin.Set("actual_key", "original-key")
	require.NoError(t, service.RememberResponsesState(origin, []byte(`{"id":"resp_backfill_old","output":[{"type":"reasoning","encrypted_content":"backfill-old"}]}`), false))
	require.NoError(t, db.Model(&channel).Update("config", `{"provider":"azure"}`).Error)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("id", userID)
		if forced := c.GetHeader("X-Test-Channel"); forced != "" {
			c.Set("specific_channel_id", forced)
		}
	})
	r.POST("/v1/responses", middleware.Distribute(), func(c *gin.Context) {
		require.Equal(t, 1, c.GetInt("channel_id"))
		require.Equal(t, "original-key", c.GetString("actual_key"))
		require.Equal(t, "azure", model.RetryProvider(c.Request.Context()))
		require.Equal(t, 1, model.ResponseStateChannel(c.Request.Context()))
		require.NoError(t, service.RememberResponsesState(c, []byte(`{"output":[{"type":"compaction","encrypted_content":"backfill-new"}]}`), false))
		c.Status(200)
	})
	oldBody := `{"model":"gpt-test","input":[{"type":"reasoning","encrypted_content":"backfill-old"}]}`
	mixedBody := `{"model":"gpt-test","input":[{"type":"reasoning","encrypted_content":"backfill-old"},{"type":"compaction","encrypted_content":"backfill-new"}]}`
	request := func(body, forced, provider string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-Channel", forced)
		req.Header.Set("X-Linkinfra-Provider", provider)
		r.ServeHTTP(w, req)
		require.Equal(t, want, w.Code, w.Body.String())
	}
	request(oldBody, "", "", 200)   // 补填后的第一轮成功，产生带 provider 的新状态。
	request(mixedBody, "", "", 200) // 下一轮携带新旧状态也应成功。
	request(oldBody, "", "azure", 200)
	request(`{"model":"gpt-test","previous_response_id":"resp_backfill_old","input":[{"type":"compaction","encrypted_content":"backfill-new"}]}`, "", "", 200)
	request(mixedBody, "2", "", 409)
	config.MemoryCacheEnabled = true
	model.InitChannelCache()
	request(mixedBody, "", "", 200)
	require.NoError(t, db.Model(&channel).Update("status", common.ChannelStatusManuallyDisabled).Error)
	request(mixedBody, "1", "", 403)
	request(mixedBody, "", "", 503)
	require.NoError(t, db.Model(&channel).Update("status", common.ChannelStatusEnabled).Error)
	require.NoError(t, db.Model(&channel).Update("key", "rotated-key").Error)
	request(mixedBody, "", "", 409)
}
