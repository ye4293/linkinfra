package controller

import (
	"context"

	dbmodel "github.com/songquanpeng/one-api/model"
)

func appendUniqueChannelID(channelIDs []int, channelID int) []int {
	if channelID <= 0 {
		return channelIDs
	}
	for _, existingChannelID := range channelIDs {
		if existingChannelID == channelID {
			return channelIDs
		}
	}
	return append(channelIDs, channelID)
}

// selectRetryChannel 选择重试渠道，始终从最高优先级开始（skipPriorityLevels=0），
// 依靠 CacheGetRandomSatisfiedChannel 内部 fallback 自然降级。
// 若当前轮次所有渠道已耗尽，则重置 failedChannelIds 从头循环。
func selectRetryChannel(ctx context.Context, group string, model string, failedChannelIds *[]int) (*dbmodel.Channel, error) {
	channel, _, err := dbmodel.CacheGetRandomSatisfiedChannel(
		ctx, group, model, 0, "", *failedChannelIds,
	)
	if err != nil {
		// provider 组内耗尽时明确失败，不重置失败列表或进入其他 provider。
		if dbmodel.HasRetryBoundary(ctx) {
			return nil, err
		}
		// 所有优先级均已耗尽，重置后从最高优先级重新开始
		*failedChannelIds = nil
		channel, _, err = dbmodel.CacheGetRandomSatisfiedChannel(
			ctx, group, model, 0, "", nil,
		)
	}
	return channel, err
}

func getLastRetryFallbackChannel(channelID int) *dbmodel.Channel {
	if channelID <= 0 {
		return nil
	}
	if channel, err := dbmodel.CacheGetChannel(channelID); err == nil {
		return channel
	}
	if channel, err := dbmodel.GetChannelById(channelID, true); err == nil {
		return channel
	}
	return nil
}
