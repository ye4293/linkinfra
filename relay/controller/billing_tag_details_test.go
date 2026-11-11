package controller

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
	relaymodel "github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// 命中计费标签折扣时，消费日志的计费明细要带上命中的标签。写入点有三处：System One
// （由 router 包的 TestBillingTagDiscountFollowsFinalChannel 覆盖）、文本路径的 postConsumeQuota，
// 以及 Claude / Gemini / Responses 共用的 enrichBillingDetailsFromContext。这里覆盖后两处。
func TestTextConsumeLogRecordsMatchedBillingTag(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Log{}))
	oldDB, oldLogs := model.DB, model.LOG_DB
	oldBatch, oldRedis, oldConsume := config.BatchUpdateEnabled, common.RedisEnabled, config.LogConsumeEnabled
	oldMetricsV2, oldGroups := config.ModelMetricsV2Enabled, common.GroupRatio
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogs
		config.BatchUpdateEnabled, common.RedisEnabled, config.LogConsumeEnabled = oldBatch, oldRedis, oldConsume
		config.ModelMetricsV2Enabled, common.GroupRatio = oldMetricsV2, oldGroups
		sqlDB.Close()
	})
	model.DB, model.LOG_DB = db, db
	config.BatchUpdateEnabled, common.RedisEnabled, config.LogConsumeEnabled = false, false, true
	config.ModelMetricsV2Enabled = false
	const group, modelName = "billing-tag-text", "billing-tag-text-model"
	common.GroupRatio = map[string]float64{group: 1}
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "tag-text", Quota: 100000}).Error)
	require.NoError(t, db.Create(&model.Token{Id: 1, UserId: 1, Key: "tag-text", RemainQuota: 100000}).Error)
	require.NoError(t, db.Create(&model.Channel{Id: 1, Name: "tag-text"}).Error)

	// 模型倍率 1、等级折扣 1、渠道折扣 1：组合折扣就是用户渠道折扣，1000 个输入 token 计 1000 × 折扣。
	consume := func(requestID, matchedTag string, userChannelRatio float64) model.Log {
		t.Helper()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
		c.Set("X-Request-ID", requestID)
		meta := &util.RelayMeta{
			UserId: 1, TokenId: 1, ChannelId: 1, Group: group,
			OriginModelName: modelName, ActualModelName: modelName,
			ChannelDiscount: 1, UserChannelRatio: userChannelRatio, MatchedBillingTag: matchedTag,
		}
		usage := &relaymodel.Usage{PromptTokens: 1000, TotalTokens: 1000}
		request := &relaymodel.GeneralOpenAIRequest{Model: modelName}
		postConsumeQuota(context.Background(), c, usage, meta, request, userChannelRatio, 0, 1, userChannelRatio, 1, "", "", 0)
		var logs []model.Log
		require.NoError(t, db.Where("x_request_id = ? AND type = ?", requestID, model.LogTypeConsume).Find(&logs).Error)
		require.Len(t, logs, 1)
		return logs[0]
	}

	hit := consume("billing-tag-text-hit", "vertex-claude", 0.85)
	require.Equal(t, 850, hit.Quota)
	require.Contains(t, hit.Other, `"billing_tag":"vertex-claude"`)
	require.Contains(t, hit.Other, `"user_channel_ratio":0.85`)

	// 折扣来自渠道类型（没有命中标签）时不带标签。
	miss := consume("billing-tag-text-miss", "", 0.9)
	require.Equal(t, 900, miss.Quota)
	require.NotContains(t, miss.Other, "billing_tag")
	require.Contains(t, miss.Other, `"user_channel_ratio":0.9`)
}

func TestEnrichBillingDetailsRecordsMatchedBillingTag(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("channel_discount", 1.0)
	c.Set("user_channel_ratio", 0.85)
	c.Set("billing_tag", "vertex-claude")
	details := enrichBillingDetailsFromContext(c, map[string]interface{}{"billing_type": "token"})
	require.Equal(t, "vertex-claude", details["billing_tag"])
	require.Equal(t, 0.85, details["user_channel_ratio"])

	// 重试换到没有命中标签的渠道后，选渠会把 billing_tag 置空，明细不再带标签。
	c.Set("user_channel_ratio", 0.9)
	c.Set("billing_tag", "")
	details = enrichBillingDetailsFromContext(c, map[string]interface{}{"billing_type": "token"})
	require.NotContains(t, details, "billing_tag")
	require.Equal(t, 0.9, details["user_channel_ratio"])
}
