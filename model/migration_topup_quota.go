package model

import (
	"errors"
	"fmt"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"gorm.io/gorm"
)

// migratedTopupQuotaOptionKey options 表里的一次性迁移标记位。
const migratedTopupQuotaOptionKey = "MigratedTopupQuotaV1"

// BackfillTopupQuota 从历史订单聚合回填 users.topup_quota。
//
// topup_quota 是 RecalcUserLevel 的唯一基准。若不回填，所有历史用户的
// 累计充值都是 0，等级判定会认为他们只够最低等级。
//
// 幂等：靠 options 表的标记位保证只执行一次。用 SET（而非 +=）赋值，
// 即使标记位被人手工删掉再跑一次也不会翻倍。
//
// 已知缺口：gift_quota 无法回填。历史赠额（注册奖励）只有 logs 表里的
// 文本日志、没有结构化金额，无法可靠还原。历史用户的 gift_quota 一律为 0，
// 上线后新产生的赠额全部准确。这个缺口是显式接受的，写在这里避免后人
// 误以为该字段自诞生起就数据完整。
func BackfillTopupQuota(db *gorm.DB) error {
	var opt Option
	err := db.Where("key = ?", migratedTopupQuotaOptionKey).First(&opt).Error
	if err == nil {
		return nil // 已迁移过
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	if config.QuotaPerUnit <= 0 {
		return errors.New("QuotaPerUnit must be positive to backfill topup_quota")
	}

	// 逐用户聚合而非一条带子查询的大 UPDATE：三种数据库对
	// UPDATE ... FROM (SELECT ...) 的语法差异很大，逐用户是唯一三库通吃的
	// 写法。这是一次性迁移，用户量级下性能不是瓶颈。
	var userIds []int
	if err := db.Model(&User{}).Pluck("id", &userIds).Error; err != nil {
		return err
	}

	// charge_orders 不在 AutoMigrate 清单里（main.go 只迁移了 Order），
	// 全新部署上该表不存在，此时跳过这部分聚合而不是报错
	hasChargeOrders := db.Migrator().HasTable(&ChargeOrder{})
	if !hasChargeOrders {
		logger.SysLog("backfill topup_quota: charge_orders table not found, skipping that source")
	}

	updated := 0
	for _, userId := range userIds {
		var topupSum float64
		if err := db.Model(&TopUp{}).
			Where("user_id = ? AND status = ?", userId, "success").
			Select("COALESCE(SUM(amount), 0)").Scan(&topupSum).Error; err != nil {
			return err
		}

		var chargeSum float64
		if hasChargeOrders {
			if err := db.Model(&ChargeOrder{}).
				Where("user_id = ? AND status = ?", userId, StatusMap["success"]).
				Select("COALESCE(SUM(amount), 0)").Scan(&chargeSum).Error; err != nil {
				return err
			}
		}

		total := AmountToQuota(topupSum + chargeSum)
		if total <= 0 {
			continue
		}
		if err := db.Model(&User{}).Where("id = ?", userId).
			Update("topup_quota", total).Error; err != nil {
			return err
		}
		updated++
	}

	if err := db.Create(&Option{Key: migratedTopupQuotaOptionKey, Value: "1"}).Error; err != nil {
		return err
	}

	logger.SysLog(fmt.Sprintf(
		"backfill topup_quota done: %d of %d users updated", updated, len(userIds)))
	return nil
}
