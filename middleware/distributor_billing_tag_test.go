package middleware

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/util"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// 同一请求先后选中不同渠道（含重试）时，用户渠道折扣和命中的计费标签都跟随当前渠道。
func TestUserChannelRatioFollowsSelectedChannelBillingTag(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.User{}))
	oldDB, oldRedis := model.DB, common.RedisEnabled
	model.DB, common.RedisEnabled = db, false
	t.Cleanup(func() {
		model.DB, common.RedisEnabled = oldDB, oldRedis
		_ = sqlDB.Close()
	})
	user := model.User{Username: "billing-tag", ChannelRatios: `{"48":0.9,"tag:vertex-gemini":0.7,"tag:vertex-claude":0.85}`}
	require.NoError(t, db.Create(&user).Error)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	c.Set("id", user.Id)

	// 顺序有意安排：每个“不带标签”的结果都紧跟在一次命中标签之后，才测得到上一渠道的标签被清掉。
	for _, tc := range []struct {
		name    string
		channel *model.Channel
		ratio   float64
		tag     string
	}{
		{"标签折扣覆盖类型折扣", &model.Channel{Id: 1, Type: common.ChannelTypeVertexAI, Key: "k1", Config: `{"billing_tag":"vertex-gemini"}`}, 0.7, "vertex-gemini"},
		{"无标签渠道清空上一渠道的标签", &model.Channel{Id: 2, Type: common.ChannelTypeVertexAI, Key: "k2"}, 0.9, ""},
		{"同类型另一个标签，匹配忽略大小写和空格", &model.Channel{Id: 3, Type: common.ChannelTypeVertexAI, Key: "k3", Config: `{"billing_tag":" Vertex-Claude "}`}, 0.85, "vertex-claude"},
		{"用户未配置该标签，回落类型折扣且不带标签", &model.Channel{Id: 4, Type: common.ChannelTypeVertexAI, Key: "k4", Config: `{"billing_tag":"vertex-other"}`}, 0.9, ""},
		{"区域映射写坏的渠道，标签照常生效", &model.Channel{Id: 5, Type: common.ChannelTypeVertexAI, Key: "k5", Config: `{"billing_tag":"vertex-gemini","vertex_model_region":"{bad json"}`}, 0.7, "vertex-gemini"},
		{"重试到其他类型且未配置折扣", &model.Channel{Id: 6, Type: common.ChannelTypeAnthropic, Key: "k6"}, 1.0, ""},
		{"其他类型的渠道打了同一标签", &model.Channel{Id: 7, Type: common.ChannelTypeAnthropic, Key: "k7", Config: `{"billing_tag":"vertex-claude"}`}, 0.85, "vertex-claude"},
	} {
		SetupContextForSelectedChannel(c, tc.channel, "test-model")
		meta := util.GetRelayMeta(c)
		require.Equal(t, tc.ratio, meta.UserChannelRatio, tc.name)
		require.Equal(t, tc.tag, meta.MatchedBillingTag, tc.name)
		require.Equal(t, tc.ratio, c.GetFloat64("user_channel_ratio"), tc.name)
		require.Equal(t, tc.tag, c.GetString("billing_tag"), tc.name)
	}
}
