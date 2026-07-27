package model

import (
	"fmt"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/logger"
)

// RecalcUserLevel 依据累计真实充值（users.topup_quota）重算用户等级。
//
// 取「满足门槛的最高等级」，而不是重构前那样逐级 +1 —— 原实现的条件写成
// totalQuota <= levelMap[nextLevel]，导致一次大额充值的用户「超过下一级
// 门槛反而不升级」，永远卡在原地（见 controller/stripeCharge.go 重构前的
// 第 31-42 行）。
//
// 只升不降：判定依据是 upgrade_threshold 的大小关系，而非 group key 的
// 字典序，这样运营新增或重命名等级时不会破坏语义。当前分组不在
// group_configs 中时（手工改过 DB 留下的野分组），视其门槛为 0，即允许升级。
//
// 必须在充值事务**提交后**调用：等级变化不影响资金正确性，失败可由下一次
// 充值自愈；放在事务内会让分组表的任何异常都阻塞充值入账。
//
// 返回 changed 表示等级是否真的变了，调用方据此决定是否失效 Redis 缓存。
func RecalcUserLevel(userId int) (changed bool, err error) {
	if userId <= 0 {
		return false, nil
	}

	var user User
	if err = DB.Where("id = ?", userId).First(&user).Error; err != nil {
		return false, err
	}

	configs, err := GetGroupConfigsByThresholdDesc()
	if err != nil {
		return false, err
	}
	if len(configs) == 0 {
		// 分组表为空（未初始化或被清空）：不做任何判断，保持现状
		return false, nil
	}

	// configs 已按 upgrade_threshold 降序、并列时 sort_order 升序排列，
	// 因此第一个满足门槛的就是目标等级
	var target *GroupConfig
	for i := range configs {
		if user.TopupQuota >= configs[i].UpgradeThreshold {
			target = &configs[i]
			break
		}
	}
	if target == nil {
		// 连最低门槛都不满足（所有门槛都 > 0 且用户充值为 0）
		return false, nil
	}
	if target.GroupKey == user.Group {
		return false, nil
	}

	// 只升不降：比较门槛大小，而非 group key 字典序。
	// 当前分组不在表中时 currentThreshold 保持 0，即允许升级。
	currentThreshold := int64(0)
	for i := range configs {
		if configs[i].GroupKey == user.Group {
			currentThreshold = configs[i].UpgradeThreshold
			break
		}
	}
	if target.UpgradeThreshold <= currentThreshold {
		return false, nil
	}

	oldGroup := user.Group
	if err = DB.Model(&User{}).Where("id = ?", userId).
		Update("group", target.GroupKey).Error; err != nil {
		return false, err
	}

	RecordLog(userId, LogTypeSystem, fmt.Sprintf(
		"level upgraded from %s to %s (cumulative top-up %s)",
		oldGroup, target.GroupKey, common.LogQuota(user.TopupQuota)))

	return true, nil
}

// RecalcUserLevelAndRefreshCache 重算等级，并在等级确实变化时失效分组缓存。
//
// 分组缓存不失效会让计费在一个 config.SyncFrequency 周期内继续用旧折扣。
// 缓存操作失败只记 log 不返回错误：等级已落库，缓存最多陈旧一个周期，
// 不能因为 Redis 抖动就让充值回调返回失败触发 Stripe 重试。
func RecalcUserLevelAndRefreshCache(userId int) {
	changed, err := RecalcUserLevel(userId)
	if err != nil {
		logger.SysError(fmt.Sprintf("failed to recalc level for user %d: %s", userId, err.Error()))
		return
	}
	if changed {
		InvalidateUserGroupCache(userId)
	}
}
