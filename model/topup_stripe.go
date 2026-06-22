package model

import (
	"errors"
	"strings"

	"github.com/songquanpeng/one-api/common/helper"
	"gorm.io/gorm"
)

const StripeTopUpPaymentMethod = "stripe"

func CreateStripeTopUp(userID int, amount int64, money float64, tradeNo string) error {
	topUp := &TopUp{
		UserId:        userID,
		Amount:        amount,
		Money:         money,
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

// CompleteStripeTopUpFromCheckout 使用 Stripe Checkout Session 回调中的 amount_total、currency 写回订单并入账
func CompleteStripeTopUpFromCheckout(tradeNo string, amountTotal int64, currency string) error {
	major := StripeAmountTotalToMajor(amountTotal, currency)
	m := major
	cur := strings.ToUpper(strings.TrimSpace(currency))
	var cPtr *string
	if cur != "" {
		cPtr = &cur
	}
	return completeTopUpOrder(tradeNo, &m, cPtr, "")
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
		if err := tx.Set("gorm:query_option", "FOR UPDATE").
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
