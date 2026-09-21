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
