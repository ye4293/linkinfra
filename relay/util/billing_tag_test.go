package util

import (
	"fmt"
	"testing"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// 异步结算没有 gin.Context，按 channelId 查渠道；取值顺序必须和同步路径一致。
func TestAsyncBillingGroupRatioUsesBillingTag(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Channel{}))
	oldDB, oldRedis, oldGroups := model.DB, common.RedisEnabled, common.GroupRatio
	model.DB, common.RedisEnabled = db, false
	common.GroupRatio = map[string]float64{"test": 0.8}
	t.Cleanup(func() {
		model.DB, common.RedisEnabled, common.GroupRatio = oldDB, oldRedis, oldGroups
		_ = sqlDB.Close()
	})

	channelType := common.ChannelTypeFlux
	user := model.User{Username: "async-tag", ChannelRatios: fmt.Sprintf(`{"%d":0.9,"tag:flux-pro":0.7}`, channelType)}
	require.NoError(t, db.Create(&user).Error)
	discount := 0.5
	require.NoError(t, db.Create(&model.Channel{Id: 1, Type: channelType, Discount: &discount, Config: `{"billing_tag":"Flux-Pro"}`}).Error)
	require.NoError(t, db.Create(&model.Channel{Id: 2, Type: channelType}).Error)

	// 渠道有标签且用户配置了该标签：标签折扣覆盖类型折扣。
	require.InDelta(t, 0.8*0.5*0.7, GetAsyncBillingGroupRatio("test", user.Id, 1, channelType, "unconfigured"), 1e-12)
	// 渠道无标签：类型折扣。
	require.InDelta(t, 0.8*0.9, GetAsyncBillingGroupRatio("test", user.Id, 2, channelType, "unconfigured"), 1e-12)
	// 渠道已删除：取不到标签，回落类型折扣，不阻塞计费。
	require.InDelta(t, 0.8*0.9, GetAsyncBillingGroupRatio("test", user.Id, 404, channelType, "unconfigured"), 1e-12)
	// 没有配置任何折扣的用户不受影响。
	require.InDelta(t, 0.8*0.5, GetAsyncBillingGroupRatio("test", 0, 1, channelType, "unconfigured"), 1e-12)
}
