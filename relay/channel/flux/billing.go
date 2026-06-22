package flux

import (
	"context"
	"fmt"

	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
)

// ChargeOnSuccess 在任务成功时扣费，仅操作用户维度额度，不依赖 TokenId。
// 应在 UpdateIfNotTerminal CAS 成功（applied=true）后调用。
func ChargeOnSuccess(ctx context.Context, image *model.Image, quota int64) error {
	if quota <= 0 {
		return fmt.Errorf("invalid quota: %d", quota)
	}

	if err := model.DecreaseUserQuota(image.UserId, quota); err != nil {
		return fmt.Errorf("billing deduction failed: %w", err)
	}

	model.UpdateUserUsedQuotaAndRequestCount(image.UserId, quota)
	model.UpdateChannelUsedQuota(image.ChannelId, quota)

	logContent := fmt.Sprintf("Flux task completed, deducted quota=%d", quota)
	model.RecordConsumeLogWithRequestID(
		ctx,
		image.UserId,
		image.ChannelId,
		0, 0,
		image.Model,
		"",
		quota,
		logContent,
		float64(image.TotalDuration),
		"", "",
		false,
		0.0,
		image.RequestId,
	)

	logger.Infof(ctx, "[flux-billing] 成功扣费 user_id=%d channel_id=%d quota=%d model=%s task_id=%s",
		image.UserId, image.ChannelId, quota, image.Model, image.TaskId)
	return nil
}

// CalculateQuota 根据 BFL API 返回的 cost（美分）计算配额。
func CalculateQuota(cost float64, groupRatio float64) int64 {
	return int64(cost / 100.0 * 500000 * groupRatio)
}

// ComputeCostUSD 按模型和实际 MP 数计算 USD 费用（无 groupRatio）。
// outputMP==0 时降级到 FluxPriceMap 固定价兜底。
func ComputeCostUSD(modelName string, metrics ReplicateMetrics) float64 {
	if tier, ok := FluxMPPricingMap[modelName]; ok && metrics.ImageOutputMegapixelCount > 0 {
		outputMP := metrics.ImageOutputMegapixelCount
		inputMP := metrics.ImageInputMegapixelCount
		var costUSD float64
		if outputMP <= 1.0 {
			costUSD += tier.FirstMPPrice
		} else {
			costUSD += tier.FirstMPPrice + (outputMP-1.0)*tier.SubsequentMPPrice
		}
		if inputMP > 0 {
			costUSD += inputMP * tier.RefMPPrice
		}
		return costUSD
	}
	price, ok := FluxPriceMap[modelName]
	if !ok {
		price = 0.05
	}
	return price
}

// CalculateReplicateQuota 计算 Replicate 配额。
func CalculateReplicateQuota(modelName string, metrics ReplicateMetrics, groupRatio float64) int64 {
	return usdToQuota(ComputeCostUSD(modelName, metrics), groupRatio)
}

// usdToQuota 将 USD 金额转为内部 quota（$1 = 500000 quota）
func usdToQuota(usd float64, groupRatio float64) int64 {
	if usd <= 0 {
		return 0
	}
	return int64(usd * 500000 * groupRatio)
}
