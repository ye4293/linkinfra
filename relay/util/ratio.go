package util

import (
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
)

// GetBillingGroupRatio 返回组合倍率：等级折扣 × 渠道折扣 × 用户渠道折扣 × 模型折扣。
//
// 历史上各 controller 里的 `groupRatio` 只包含等级折扣。为了一次性引入
// 渠道折扣与用户针对渠道类型的折扣，让所有老 call site 直接把原来的
// common.GetGroupRatio(group) 替换为本函数即可，语义变成"组合后的总折扣"。
// 日志里仍然显示为"分组倍率"，但数值已是融合后的结果。
//
// channel_discount 和 user_channel_ratio 由 middleware/distributor 在选中渠道时
// 写入 c，缺省 1.0。拆开三个维度分别打印/调试时调 GetBillingFactors。
func GetBillingGroupRatio(c *gin.Context, group string, modelName string) float64 {
	groupRatio, channelDiscount, userChannelRatio := GetBillingFactors(c, group)
	return groupRatio * channelDiscount * userChannelRatio * common.GetModelDiscount(modelName)
}

// GetBillingFactors 返回三段折扣分量：等级折扣、渠道折扣、用户渠道折扣。
// 任何一段未设置都回退到 1.0。
func GetBillingFactors(c *gin.Context, group string) (groupRatio, channelDiscount, userChannelRatio float64) {
	groupRatio = common.GetGroupRatio(group)

	channelDiscount = 1.0
	if c != nil {
		if v := c.GetFloat64("channel_discount"); v > 0 {
			channelDiscount = v
		}
	}

	userChannelRatio = 1.0
	if c != nil {
		if v := c.GetFloat64("user_channel_ratio"); v > 0 {
			userChannelRatio = v
		}
	}
	return
}

// GetAsyncBillingGroupRatio 在异步回调等场景（没有活的 gin.Context）计算组合倍率。
// 通过 userId 直查缓存，通过 channelId 从 DB 拉渠道拿 Discount。
// 任何一段失败都回退到 1.0，不会因为取不到数据阻塞计费。
func GetAsyncBillingGroupRatio(group string, userId int, channelId int, channelType int, modelName string) float64 {
	groupRatio := common.GetGroupRatio(group)

	channelDiscount := 1.0
	if channelId > 0 {
		if channel, err := model.GetChannelById(channelId, false); err == nil && channel != nil {
			if channel.Discount != nil && *channel.Discount > 0 {
				channelDiscount = *channel.Discount
			}
		}
	}

	userChannelRatio := 1.0
	if userId > 0 && channelType > 0 {
		if ratios, err := model.CacheGetUserChannelRatios(userId); err == nil {
			if r, ok := ratios[channelType]; ok && r > 0 {
				userChannelRatio = r
			}
		}
	}
	return groupRatio * channelDiscount * userChannelRatio * common.GetModelDiscount(modelName)
}
