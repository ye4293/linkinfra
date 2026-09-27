package controller

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	dbmodel "github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

type systemOneTariff struct {
	input, output, fixed, group, tier, discount float64
	channel, user                               float64
}

func newSystemOneTariff(meta *util.RelayMeta) (systemOneTariff, error) {
	name := meta.BillingModelName()
	t := systemOneTariff{input: common.GetModelRatio(name), output: common.GetCompletionRatio(name), fixed: common.GetModelPrice(name, false), group: meta.CombinedGroupRatio(), tier: common.GetGroupRatio(meta.Group), discount: common.GetModelDiscount(name), channel: meta.ChannelDiscount, user: meta.UserChannelRatio}
	if t.channel <= 0 {
		t.channel = 1
	}
	if t.user <= 0 {
		t.user = 1
	}
	for _, value := range []float64{t.input, t.output, t.group, t.tier, t.discount, t.channel, t.user} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return t, fmt.Errorf("invalid systemone price")
		}
	}
	if t.fixed != -1 && (math.IsNaN(t.fixed) || math.IsInf(t.fixed, 0) || t.fixed < 0) {
		return t, fmt.Errorf("invalid systemone fixed price")
	}
	return t, nil
}

func (t systemOneTariff) quota(input, output int) (int64, error) {
	if input < 0 || output < 0 {
		return 0, fmt.Errorf("invalid systemone token usage")
	}
	// 从配置的十进制表示构建有理数，防止 3000*0.021 向上取整成 64。
	decimal := func(value float64) *big.Rat {
		result, _ := new(big.Rat).SetString(strconv.FormatFloat(value, 'f', -1, 64))
		return result
	}
	value := new(big.Rat).SetInt64(int64(input))
	value.Add(value, new(big.Rat).Mul(new(big.Rat).SetInt64(int64(output)), decimal(t.output)))
	value.Mul(value, decimal(t.input))
	if t.fixed != -1 {
		value.Mul(decimal(t.fixed), big.NewRat(500000, 1))
	}
	for _, factor := range []float64{t.tier, t.discount, t.channel, t.user} {
		value.Mul(value, decimal(factor))
	}
	whole, remainder := new(big.Int), new(big.Int)
	whole.QuoRem(value.Num(), value.Denom(), remainder)
	if t.fixed == -1 && remainder.Sign() > 0 {
		whole.Add(whole, big.NewInt(1))
	}
	if !whole.IsInt64() || whole.Sign() < 0 {
		return 0, fmt.Errorf("invalid systemone quota")
	}
	return whole.Int64(), nil
}

func recordSystemOneConsumption(ctx context.Context, c *gin.Context, meta *util.RelayMeta, usage *model.Usage, tariff systemOneTariff, quota int64, duration float64) {
	details := map[string]interface{}{
		"group_ratio": tariff.group, "tier_ratio": tariff.tier, "model_discount": tariff.discount,
		"channel_discount": meta.ChannelDiscount, "user_channel_ratio": meta.UserChannelRatio,
	}
	if meta.IsMultiKey && meta.KeyIndex != nil {
		details["is_multi_key"] = true
		details["key_index"] = *meta.KeyIndex
	}
	if tariff.fixed == -1 {
		details["billing_type"] = "token"
		details["model_ratio"] = tariff.input
		details["completion_ratio"] = tariff.output
	} else {
		details["billing_type"] = "fixed_price"
		details["model_price"] = tariff.fixed
	}
	other := appendModelMappingInfo(getChannelHistoryInfo(c), meta.OriginModelName, meta.ActualModelName)
	other = appendBillingDetails(ctx, other, details)
	other = util.AppendRetryHistoryOther(c, other, duration)
	dbmodel.RecordConsumeLogWithOtherAndRequestID(ctx, meta.UserId, meta.ChannelId, usage.PromptTokens, usage.CompletionTokens, meta.BillingModelName(), meta.TokenName, quota,
		"System One usage billing", duration, c.GetHeader("X-Title"), c.GetHeader("HTTP-Referer"), false, 0, other, c.GetString("X-Request-ID"), 0, "")
	updateMultiKeyUsage(ctx, meta, true)
}
