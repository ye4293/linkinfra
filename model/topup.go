package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"gorm.io/gorm"
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
	Other string `json:"other" gorm:"type:longtext"`
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
		tx = tx.Where("trade_no LIKE ?", "%"+tradeNo+"%")
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

	err := DB.Transaction(func(tx *gorm.DB) error {
		var topUp TopUp
		if err := tx.Set("gorm:query_option", "FOR UPDATE").
			Where("trade_no = ?", tradeNo).First(&topUp).Error; err != nil {
			return errors.New("top-up order not found")
		}

		if topUp.Status != "pending" {
			return nil
		}

		quotaToAdd = int64(float64(topUp.Amount) * config.QuotaPerUnit)
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

		if err := tx.Model(&User{}).Where("id = ?", topUp.UserId).
			Update("quota", gorm.Expr("quota + ?", quotaToAdd)).Error; err != nil {
			return err
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
