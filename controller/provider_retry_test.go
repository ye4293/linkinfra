package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

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

func TestResponsesRetryDelay(t *testing.T) {
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, seconds, milliseconds string
		attempt                     int
		want                        time.Duration
		blocked                     bool
	}{
		{name: "default", attempt: 1, want: 2 * time.Second},
		{name: "backoff", attempt: 4, want: 16 * time.Second},
		{name: "backoff_bounded", attempt: 100, want: 16 * time.Second},
		{name: "seconds", seconds: "12", want: 12 * time.Second},
		{name: "milliseconds", seconds: "12", milliseconds: "250", want: 250 * time.Millisecond},
		{name: "invalid", seconds: "invalid", milliseconds: "-1", want: 2 * time.Second},
		{name: "date", seconds: now.Add(10 * time.Second).Format(http.TimeFormat), want: 10 * time.Second},
		{name: "past_date", seconds: now.Add(-time.Second).Format(http.TimeFormat), want: time.Millisecond},
		{name: "zero", seconds: "0", want: time.Millisecond},
		{name: "long_seconds", seconds: "31", blocked: true},
		{name: "long_milliseconds", milliseconds: "30001", blocked: true},
		{name: "overflow_duration", seconds: "9223372036854775807", blocked: true},
		{name: "long_date", seconds: now.Add(time.Minute).Format(http.TimeFormat), blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delay, err := responsesRetryDelay(tc.seconds, tc.milliseconds, tc.attempt, now)
			if tc.blocked {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, delay)
		})
	}
}

func TestProviderRetryPrefersLowerPriority(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	for _, spec := range []struct {
		id       int
		priority int64
		provider string
	}{
		{1, 100, "openai"}, {2, 100, "openai"},
		{3, 90, "azure"}, {4, 80, " OpenAI "}, {5, 80, "openai"},
		{6, 0, "openai"}, {7, 0, "openai"}, {8, -10, "azure"},
	} {
		weight := uint(1)
		channel := model.Channel{Id: spec.id, Status: common.ChannelStatusEnabled, Group: "default", Models: "gpt-test", Priority: &spec.priority, Weight: &weight, Config: `{"provider":"` + spec.provider + `"}`}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, db.Create(&model.Ability{ChannelId: spec.id, Group: "default", Model: "gpt-test", Enabled: true, Priority: &spec.priority}).Error)
	}
	ctx := model.WithRetryProvider(context.Background(), "openai")
	failed := []int{1}
	channel, err := selectRetryChannel(ctx, "default", "gpt-test", &failed)
	require.NoError(t, err)
	require.Contains(t, []int{4, 5}, channel.Id, "同档仍有渠道时也应优先降级，并跳过其他 provider")
	failed = append(failed, channel.Id)
	// 自动禁用失败渠道后，仍以它的优先级继续降级，不能从最高档重新开始。
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("status", common.ChannelStatusAutoDisabled).Error)
	require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", channel.Id).Update("enabled", false).Error)
	channel, err = selectRetryChannel(ctx, "default", "gpt-test", &failed)
	require.NoError(t, err)
	require.Contains(t, []int{6, 7}, channel.Id)
	failed = append(failed, channel.Id)
	channel, err = selectRetryChannel(ctx, "default", "gpt-test", &failed)
	require.NoError(t, err)
	require.Contains(t, []int{6, 7}, channel.Id, "最低可用档仍可重试未失败的同档渠道")
	require.NotContains(t, failed, channel.Id)
	failed = append(failed, channel.Id)
	channel, err = selectRetryChannel(ctx, "default", "gpt-test", &failed)
	require.Error(t, err)
	require.Nil(t, channel, "降级耗尽后不能返回高档或跨 provider")
	require.Len(t, failed, 4)

	// 不带 provider 时保留同档补重试；首次选渠也仍从最高档开始。
	failed = []int{1}
	channel, err = selectRetryChannel(context.Background(), "default", "gpt-test", &failed)
	require.NoError(t, err)
	require.Equal(t, 2, channel.Id)
	channel, _, err = model.CacheGetRandomSatisfiedChannel(ctx, "default", "gpt-test", 0, "")
	require.NoError(t, err)
	require.Contains(t, []int{1, 2}, channel.Id)

	// 原渠道绑定不能因优先级降级而被放宽。
	failed = []int{1}
	channel, err = selectRetryChannel(model.WithResponseStateChannel(ctx, 1), "default", "gpt-test", &failed)
	require.Error(t, err)
	require.Nil(t, channel)
}
