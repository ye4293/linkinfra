package model

import (
	"errors"
	"strings"

	"github.com/songquanpeng/one-api/common/helper"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const StripeTopUpPaymentMethod = "stripe"

func CreateStripeTopUp(userID int, amount int64, tradeNo string) error {
	// StripePriceId 单价为 $1/unit，amount 即美金数量；webhook 回调会用
	// 真实 amount_total 覆盖此字段，这里仅作下单时的预期金额。
	topUp := &TopUp{
		UserId:        userID,
		Amount:        amount,
		Money:         float64(amount),
		TradeNo:       tradeNo,
		PaymentMethod: StripeTopUpPaymentMethod,
		CreateTime:    helper.GetTimestamp(),
		Status:        "pending",
		Currency:      "USD",
	}
	return topUp.Insert()
}

// CompleteStripeTopUp 无 Checkout 金额信息时完成订单（兼容旧逻辑）
func CompleteStripeTopUp(tradeNo string) error {
	return CompleteTopUpOrder(tradeNo)
}

// CompleteStripeTopUpFromCheckout 用 Stripe 扣手续费后的净额（balance_transaction.net）
// 折算额度并入账；netTotal 为最小货币单位（cents），currency 为结算货币。
// receiptUrl 为 charge.receipt_url，写入订单便于对账。
func CompleteStripeTopUpFromCheckout(tradeNo string, netTotal int64, currency string, receiptUrl string) error {
	netMajor := StripeAmountTotalToMajor(netTotal, currency)
	quota := AmountToQuota(netMajor)
	m := netMajor
	cur := strings.ToUpper(strings.TrimSpace(currency))
	var cPtr *string
	if cur != "" {
		cPtr = &cur
	}
	var rPtr *string
	if receiptUrl != "" {
		rPtr = &receiptUrl
	}
	return completeTopUpOrder(tradeNo, &m, cPtr, &quota, rPtr, "")
}

func ExpireStripeTopUp(tradeNo string) error {
	return ExpireTopUpOrder(tradeNo)
}

func ExpireTopUpOrder(tradeNo string) error {
	if tradeNo == "" {
		return errors.New("trade number not provided")
	}

	return DB.Transaction(func(tx *gorm.DB) error {
		var topUp TopUp
		// clause.Locking 而非 GORM v1 的 Set("gorm:query_option", ...)，
		// 后者在 v2 里是 no-op、不生成任何 SQL。理由同 topup.go 的注释。
		if err := tx.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
			Where("trade_no = ?", tradeNo).First(&topUp).Error; err != nil {
			return errors.New("top-up order not found")
		}

		if topUp.Status != "pending" {
			return nil
		}

		topUp.Status = "expired"
		topUp.CompleteTime = helper.GetTimestamp()
		return tx.Save(&topUp).Error
	})
}
