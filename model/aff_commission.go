package model

import (
	"errors"

	"gorm.io/gorm"
)

// 返现记录的来源渠道。用字符串而非枚举整数，为加密货币等新渠道
// 预留扩展位而不需要数据迁移。
const (
	SourceTypeStripeCheckout = "stripe_checkout" // Stripe Checkout 链路（model/topup.go）
	SourceTypeStripeCharge   = "stripe_charge"   // Stripe 套餐链路（model/charge_order.go）
)

// 返现记录状态
const (
	AffCommissionStatusGranted  = 1 // 已发放
	AffCommissionStatusReversed = 2 // 已冲正（对应充值被退款）
)

// AffCommissionRecord 邀请返现明细。每一笔返现都有一条记录，用于对账与展示。
//
// Rate 与 InviterGroup 是快照：后台改了比例不影响已发放记录的解释，
// 历史记录永远可复现当时的计算过程。
// 用户名也是快照，用户改名后对账不断链（用户 id 同时保留，两者互补）。
type AffCommissionRecord struct {
	Id              int    `json:"id" gorm:"primaryKey;autoIncrement"`
	InviterId       int    `json:"inviter_id" gorm:"index;not null"`
	InviteeId       int    `json:"invitee_id" gorm:"index;not null"`
	InviterUsername string `json:"inviter_username" gorm:"type:varchar(64)"`
	InviteeUsername string `json:"invitee_username" gorm:"type:varchar(64)"`
	SourceType      string `json:"source_type" gorm:"type:varchar(32)"`
	// SourceNo 是幂等的核心。Stripe 会重放 webhook，这个唯一索引是
	// 防重复发放的最后一道保险，比事务本身更重要。
	SourceNo        string  `json:"source_no" gorm:"type:varchar(128);uniqueIndex;not null"`
	TopupAmount     float64 `json:"topup_amount" gorm:"type:decimal(20,6)"` // 被邀请人实付金额
	TopupQuota      int64   `json:"topup_quota"`                            // 换算后的充值 quota
	Rate            float64 `json:"rate" gorm:"type:decimal(5,4)"`          // 比例快照
	InviterGroup    string  `json:"inviter_group" gorm:"type:varchar(32)"`  // 等级快照
	CommissionQuota int64   `json:"commission_quota"`                       // 实发返现 quota
	Status          int     `json:"status" gorm:"default:1;index"`
	// ReversedQuota 实际扣回的额度，可能小于 CommissionQuota——
	// 冲正时邀请人余额不足则扣到 0 为止，差额是运营的真实损失，必须可查。
	ReversedQuota int64 `json:"reversed_quota" gorm:"default:0"`
	CreatedAt     int64 `json:"created_at" gorm:"bigint;index"`
	ReversedAt    int64 `json:"reversed_at" gorm:"bigint;default:0"`
}

// GetAffCommissionRecordBySourceNo 按来源单号查询返现记录。
// 记录不存在时返回 (nil, nil)——调用方据此判断「那笔充值本来没有返现」，
// 这是正常分支而非故障。
func GetAffCommissionRecordBySourceNo(sourceNo string) (*AffCommissionRecord, error) {
	if sourceNo == "" {
		return nil, nil
	}
	var record AffCommissionRecord
	err := DB.Where("source_no = ?", sourceNo).First(&record).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &record, nil
}

// GetAffCommissionSummary 汇总某个邀请人的累计返现额与有效返现笔数。
// 已冲正（退款）的记录不计入。
func GetAffCommissionSummary(inviterId int) (totalQuota int64, count int64, err error) {
	if inviterId <= 0 {
		return 0, 0, nil
	}

	// 两次查询各自新建链式调用：GORM 的 *gorm.DB 在执行过终结方法
	// （Count / Scan 等）后会携带残留状态，复用同一个 tx 变量会让
	// 第二次查询带上第一次的 SELECT 子句。
	if err = DB.Model(&AffCommissionRecord{}).
		Where("inviter_id = ? AND status = ?", inviterId, AffCommissionStatusGranted).
		Count(&count).Error; err != nil {
		return 0, 0, err
	}

	// SUM 在无匹配行时返回 NULL，用 COALESCE 兜成 0，否则 Scan 到 int64 会失败
	if err = DB.Model(&AffCommissionRecord{}).
		Where("inviter_id = ? AND status = ?", inviterId, AffCommissionStatusGranted).
		Select("COALESCE(SUM(commission_quota), 0)").
		Scan(&totalQuota).Error; err != nil {
		return 0, count, err
	}

	return totalQuota, count, nil
}
