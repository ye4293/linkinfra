package controller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
	"github.com/stretchr/testify/require"
)

func TestRetryStaysWithinOriginalProvider(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	add := func(id int, provider, group, name string, priority int64, enabled bool) {
		t.Helper()
		weight := uint(1)
		cfg, err := json.Marshal(model.ChannelConfig{Provider: provider})
		require.NoError(t, err)
		status := common.ChannelStatusEnabled
		if !enabled {
			status = common.ChannelStatusManuallyDisabled
		}
		channel := model.Channel{Id: id, Type: common.ChannelTypeOpenAI, Status: status, Config: string(cfg), Group: group, Models: name, Priority: &priority, Weight: &weight}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, db.Create(&model.Ability{ChannelId: id, Group: group, Model: name, Enabled: enabled, Priority: &priority}).Error)
	}
	add(1, "openai", "default", "gpt-test", 100, true)
	add(2, "azure", "default", "gpt-test", 100, true)
	add(3, "", "default", "gpt-test", 100, true)
	add(4, " OpenAI ", "default", "gpt-test", 90, true)
	add(5, "openai", "default", "gpt-test", 80, true)
	add(6, "openai", "private", "gpt-test", 200, true)
	add(7, "openai", "default", "other-model", 200, true)
	add(8, "openai", "default", "gpt-test", 200, false)

	ctx := model.WithRetryProvider(context.Background(), "openai")
	failed := []int{1}
	channel, err := selectRetryChannel(ctx, "default", "gpt-test", &failed)
	require.NoError(t, err)
	require.Equal(t, 4, channel.Id, "must skip higher-priority Azure/unconfigured channels")
	failed = append(failed, channel.Id)
	channel, err = selectRetryChannel(ctx, "default", "gpt-test", &failed)
	require.NoError(t, err)
	require.Equal(t, 5, channel.Id)
	failed = append(failed, channel.Id)
	channel, err = selectRetryChannel(ctx, "default", "gpt-test", &failed)
	require.Error(t, err)
	require.Nil(t, channel)
	require.Equal(t, []int{1, 4, 5}, failed, "must not restart the failed-channel cycle")

	azure := model.WithRetryProvider(context.Background(), "AZURE")
	failed = []int{2}
	channel, err = selectRetryChannel(azure, "default", "gpt-test", &failed)
	require.Error(t, err)
	require.Nil(t, channel)

	// 未填写 provider：保留之前的跨渠道及耗尽后重置行为。
	failed = []int{1, 2, 3, 4, 5}
	channel, err = selectRetryChannel(context.Background(), "default", "gpt-test", &failed)
	require.NoError(t, err)
	require.Contains(t, []int{1, 2, 3}, channel.Id)
	require.Empty(t, failed)
}
