package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type TopUp struct {
	Id            int     `json:"id"`
	UserId        int     `json:"user_id" gorm:"index"`
	Amount        int64   `json:"amount"`
	Money         float64 `json:"money"`
	TradeNo       string  `json:"trade_no" gorm:"uniqueIndex;type:varchar(255)"`
	PaymentMethod string  `json:"payment_method" gorm:"type:varchar(50)"`
	Currency      string  `json:"currency" gorm:"type:varchar(10);default:''"`
	CreateTime    int64   `json:"create_time"`
	CompleteTime  int64   `json:"complete_time"`
	Status        string  `json:"status" gorm:"type:varchar(20);default:'pending'"`
	// Other 扩展 JSON：管理员补单时写入 TopUpManualCompleteMeta 等，支付回调留空
	Other string `json:"other" gorm:"type:text"`
}

// TopUpManualCompleteMeta 补单入账详情（写入 other，可继续加字段）
type TopUpManualCompleteMeta struct {
	Source              string `json:"source"` // 固定 manual_complete
	OperatorUserId      int    `json:"operator_user_id"`
	OperatorUsername    string `json:"operator_username,omitempty"`
	OperatorDisplayName string `json:"operator_display_name,omitempty"`
	CompletedAt         int64  `json:"completed_at"`
}

func (topUp *TopUp) Insert() error {
	return DB.Create(topUp).Error
}

func (topUp *TopUp) Update() error {
	return DB.Save(topUp).Error
}

func GetTopUpByTradeNo(tradeNo string) *TopUp {
	var topUp TopUp
	err := DB.Where("trade_no = ?", tradeNo).First(&topUp).Error
	if err != nil {
		return nil
	}
	return &topUp
}

const maxPageSize = 100

func SearchTopUps(userId int, tradeNo string, page int, pageSize int) (topups []*TopUp, total int64, err error) {
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	tx := DB.Model(&TopUp{})
	if userId > 0 {
		tx = tx.Where("user_id = ?", userId)
	}
	if tradeNo != "" {
		tx = tx.Where("trade_no "+likeOp()+" ?", "%"+tradeNo+"%")
	}
	err = tx.Count(&total).Error
	if err != nil {
		return nil, 0, err
	}
	offset := (page - 1) * pageSize
	err = tx.Order("id desc").Limit(pageSize).Offset(offset).Find(&topups).Error
	if err != nil {
		return nil, total, err
	}
	return topups, total, nil
}

// CompleteTopUpOrder 易支付等回调完成订单（保留创建时的 money / currency）
func CompleteTopUpOrder(tradeNo string) error {
	return completeTopUpOrder(tradeNo, nil, nil, "")
}

// CompleteTopUpOrderManual 管理员补单：将详情序列化写入 other
func CompleteTopUpOrderManual(tradeNo string, meta TopUpManualCompleteMeta) error {
	if meta.OperatorUserId <= 0 {
		return errors.New("invalid operator")
	}
	if meta.Source == "" {
		meta.Source = "manual_complete"
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return completeTopUpOrder(tradeNo, nil, nil, string(b))
}

// completeTopUpOrder 完成充值；moneyOverride / currencyOverride 非空时写回（Stripe 以 Checkout 回调为准）
// manualOtherJSON 非空时表示管理员补单，写入 other
func completeTopUpOrder(tradeNo string, moneyOverride *float64, currencyOverride *string, manualOtherJSON string) error {
	if tradeNo == "" {
		return errors.New("trade number not provided")
	}

	var userId int
	var quotaToAdd int64
	var money float64
	var currency string
	var commissionInviterId int
	var commissionQuota int64

	// 管理员手工补单是运营白送的额度：既不计入 topup_quota（等级判定基准），
	// 也不产生邀请返现 —— 否则等于白送两次。设计文档 §1.3 明确排除。
	// manualOtherJSON 只在 CompleteTopUpOrderManual 路径下非空，
	// 两条 Stripe 链路传的都是空串。
	isRealPayment := manualOtherJSON == ""

	err := DB.Transaction(func(tx *gorm.DB) error {
		var topUp TopUp
		// 行锁：原先写的 Set("gorm:query_option", "FOR UPDATE") 是 GORM v1
		// 的 API，在 v2 里只是往 Statement 里存了个值、不生成任何 SQL ——
		// 实测生成的是裸 SELECT，行锁从来就不存在，并发全靠后面的
		// status != "pending" 早退兜底。
		//
		// clause.Locking 无需方言分支：sqlite 驱动会静默剥离它
		// （driver/sqlite/sqlite.go 的 "FOR" ClauseBuilder 里注释
		// "SQLite3 does not support row-level locking" 后直接 return），
		// PG 无覆盖、走默认渲染出 FOR UPDATE，MySQL 的覆盖只改写 SHARE。
		if err := tx.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
			Where("trade_no = ?", tradeNo).First(&topUp).Error; err != nil {
			return errors.New("top-up order not found")
		}

		if topUp.Status != "pending" {
			return nil
		}

		quotaToAdd = AmountToQuota(float64(topUp.Amount))
		if quotaToAdd <= 0 {
			return errors.New("invalid top-up quota")
		}

		if moneyOverride != nil {
			topUp.Money = *moneyOverride
		}
		if currencyOverride != nil && *currencyOverride != "" {
			topUp.Currency = *currencyOverride
		}

		if manualOtherJSON != "" {
			topUp.Other = manualOtherJSON
		}

		topUp.Status = "success"
		topUp.CompleteTime = helper.GetTimestamp()
		if err := tx.Save(&topUp).Error; err != nil {
			return err
		}

		// quota 是可用余额；topup_quota 是累计真实充值，等级判定的唯一依据，
		// 管理员补单不计入
		userUpdates := map[string]interface{}{
			"quota": gorm.Expr("quota + ?", quotaToAdd),
		}
		if isRealPayment {
			userUpdates["topup_quota"] = gorm.Expr("topup_quota + ?", quotaToAdd)
		}
		if err := tx.Model(&User{}).Where("id = ?", topUp.UserId).
			Updates(userUpdates).Error; err != nil {
			return err
		}

		if isRealPayment {
			// 邀请返现与充值入账同事务，靠 trade_no 唯一索引保证 webhook 重放幂等。
			// 除唯一键冲突外的错误会回滚整笔充值 —— Stripe 会重试，最终一致。
			//
			// 用 := 接局部变量再赋给外层：闭包签名是 func(tx *gorm.DB) error，
			// 内部没有名为 err 的变量可供 = 赋值。
			//
			// 返现基数用 topUp.Money（用户实付货币金额）而非 topUp.Amount
			// （充值的额度单位数）—— 设计文档 §5.1 明确按用户实付金额算。
			inviterId, cq, gErr := GrantCommission(
				tx, topUp.UserId, topUp.Money, quotaToAdd,
				SourceTypeStripeCheckout, topUp.TradeNo)
			if gErr != nil {
				return gErr
			}
			commissionInviterId = inviterId
			commissionQuota = cq
		}

		userId = topUp.UserId
		money = topUp.Money
		currency = topUp.Currency
		return nil
	})

	if err != nil {
		return err
	}

	if userId > 0 && quotaToAdd > 0 {
		curNote := ""
		if currency != "" {
			curNote = " " + currency
		}
		RecordLog(userId, LogTypeTopup, fmt.Sprintf("Top-up successful, quota added: %d, amount paid: %.2f%s", quotaToAdd, money, curNote))
		if manualOtherJSON != "" {
			logger.SysLog(fmt.Sprintf("管理员补单入账: other=%s, buyerUserId=%d, tradeNo=%s, quota=%d, money=%.2f %s", manualOtherJSON, userId, tradeNo, quotaToAdd, money, currency))
		} else {
			logger.SysLog(fmt.Sprintf("在线充值成功: userId=%d, tradeNo=%s, quota=%d, money=%.2f %s", userId, tradeNo, quotaToAdd, money, currency))
		}

		// 以下都在事务外做：Redis 不参与事务回滚，放在事务内会在回滚时留下脏缓存
		if err := CacheUpdateUserQuota2(userId); err != nil {
			logger.SysError("failed to refresh quota cache: " + err.Error())
		}

		if commissionInviterId > 0 && commissionQuota > 0 {
			if err := CacheUpdateUserQuota2(commissionInviterId); err != nil {
				logger.SysError("failed to refresh inviter quota cache: " + err.Error())
			}
			RecordLog(commissionInviterId, LogTypeAffCommission, fmt.Sprintf(
				"referral commission %s from invitee %d top-up",
				common.LogQuota(commissionQuota), userId))
		}

		// 等级重算放事务外：等级变化不影响资金正确性，失败可由下次充值自愈。
		// 管理员补单不改 topup_quota，重算是无操作，但调用无害且保持路径统一。
		RecalcUserLevelAndRefreshCache(userId)
	}
	return nil
}

// StripeAmountTotalToMajor 将 Checkout Session 的 amount_total（最小货币单位）转为展示用主单位金额
func StripeAmountTotalToMajor(amountTotal int64, currency string) float64 {
	c := strings.ToLower(strings.TrimSpace(currency))
	// https://docs.stripe.com/currencies#minor-units 零小数货币
	zeroDecimal := map[string]bool{
		"bif": true, "clp": true, "djf": true, "gnf": true, "jpy": true,
		"kmf": true, "krw": true, "mga": true, "pyg": true, "rwf": true,
		"ugx": true, "vnd": true, "vuv": true, "xaf": true, "xof": true, "xpf": true,
	}
	if zeroDecimal[c] {
		return float64(amountTotal)
	}
	return float64(amountTotal) / 100.0
}
