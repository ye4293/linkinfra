package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/stripe/stripe-go/v78"
	"github.com/stripe/stripe-go/v78/paymentlink"
	"github.com/stripe/stripe-go/v78/webhook"
	"gorm.io/gorm"
)

type ChargeOrder struct {
	Id         int     `json:"id"`
	UserId     int     `json:"user_id"`
	AppOrderId string  `json:"app_order_id"`
	OrderNo    string  `json:"order_no"`
	ChargeId   int     `json:"charge_id"`
	Status     int     `json:"status"`
	Currency   string  `json:"currency"`
	Extension  string  `json:"extension"`
	Amount     float64 `json:"amount"`
	RealAmount float64 `json:"real_amount"`
	OrderCost  float64 `json:"order_cost"`
	Ip         string  `json:"ip"`
	SourceName string  `json:"source_name"`
	UpdatedAt  string  `json:"updated_at"`
	CreatedAt  string  `json:"created_at"`
}

type OrderInfo struct {
	ChargeUrl string
}

var StatusMap = map[string]int{
	"create":  1, //待支付
	"success": 3, //成功
	"fail":    4, //失败
	"refund":  5, //退款
	"dispute": 6, //争议
	"fraud":   7, //欺诈
}

func GetUserChargeOrdersAndCount(conditions map[string]interface{}, page int, pageSize int) (chargeOrders []*ChargeOrder, total int64, err error) {
	var chargeOrder ChargeOrder
	for k, v := range conditions {
		if k == "userId" {
			DB.Where("user_id = ?", v)
		}
		if k == "appOrderId" {
			DB.Where("app_order_id = ?", v)
		}
		if k == "status" {
			DB.Where("status = ?", v)
		}
	}
	err = DB.Model(&chargeOrder).Count(&total).Error
	if err != nil {
		return nil, 0, err
	}
	// 计算起始索引。第一页的起始索引为0。
	offset := (page - 1) * pageSize

	// 然后获取满足条件的订单数据
	err = DB.Model(&chargeOrder).Limit(pageSize).Offset(offset).Find(&chargeOrders).Error
	if err != nil {
		return nil, total, err
	}

	// 返回日志数据、总数以及错误信息
	return chargeOrders, total, nil
}

func CreateStripOrder(userId, chargeId int) (string, string, error) {
	//查询配置项
	chargeConfig, err := GetChargeConfigById(chargeId)
	if err != nil {
		return "", "", err
	}
	appOrderId := helper.GetRandomString(16)
	chargeOrder := ChargeOrder{
		UserId:     userId,
		ChargeId:   chargeConfig.Id,
		Currency:   chargeConfig.Currency,
		AppOrderId: appOrderId,
		Status:     StatusMap["create"],
		Amount:     chargeConfig.Amount,
		Ip:         helper.GetIp(),
		UpdatedAt:  helper.GetFormatTimeString(),
		CreatedAt:  helper.GetFormatTimeString(),
	}

	//创建订单
	err = DB.Model(&ChargeOrder{}).Create(&chargeOrder).Error
	if err != nil {
		return "", "", err
	}

	bill := Bill{
		Username:  GetUsernameById(userId),
		UserId:    userId,
		Type:      "Credits",
		UpdatedAt: helper.GetTimestamp(),
		CreatedAt: helper.GetTimestamp(),
		Amount:    chargeConfig.Amount,
		Status:    StatusMap["create"],
		SourceId:  appOrderId,
	}
	err = DB.Model(&Bill{}).Create(&bill).Error
	if err != nil {
		return "", "", err
	}
	//创建价格
	stripe.Key = config.StripePrivateKey
	params := &stripe.PaymentLinkParams{
		LineItems: []*stripe.PaymentLinkLineItemParams{
			{
				Price:    stripe.String(chargeConfig.Price),
				Quantity: stripe.Int64(1),
			},
		},
		PaymentIntentData: &stripe.PaymentLinkPaymentIntentDataParams{
			Metadata: map[string]string{
				"userId":     fmt.Sprintf("%d", userId),
				"appOrderId": appOrderId,
			},
		},
		Restrictions: &stripe.PaymentLinkRestrictionsParams{
			CompletedSessions: &stripe.PaymentLinkRestrictionsCompletedSessionsParams{
				Limit: stripe.Int64(1),
			},
		},
	}

	result, err := paymentlink.New(params)
	if err != nil {
		return "", "", err
	}
	return result.URL, appOrderId, nil
}
func stripeChargeFail(charge *stripe.Charge) error {
	return nil
}
func stripeChargeDispute() {

}
func stripeChargeFraud() {

}

func stripeChargeRefund(charge *stripe.Charge) error {
	if charge.Status != "succeeded" {
		return nil
	}

	//获取meta数据里的订单id
	orderId := charge.Metadata["appOrderId"]
	userId := charge.Metadata["userId"]

	var reversedInviterId int
	var refundedUserId int

	err := DB.Transaction(func(tx *gorm.DB) error {
		// 原子改单：只有成功状态的订单才能退款
		if !UpdateChargeOrderStatusWithConditionTx(tx, orderId, userId,
			StatusMap["success"], StatusMap["refund"]) {
			// 订单已被处理或状态不符合预期，直接返回
			return nil
		}

		var order ChargeOrder
		if err := tx.Where("app_order_id = ? AND user_id = ?", orderId, userId).
			First(&order).Error; err != nil {
			return err
		}
		refundedUserId = order.UserId

		// 扣回充值方自己的额度与累计充值。原实现只改订单状态，导致退款用户的
		// 余额与 topup_quota 都虚高，等级也虚高。
		// 余额不足时扣到 0 为止，绝不产生负余额。
		quotaToRevoke := AmountToQuota(order.Amount)
		var u User
		if err := tx.Where("id = ?", order.UserId).First(&u).Error; err != nil {
			return err
		}
		actualQuota := quotaToRevoke
		if u.Quota < actualQuota {
			actualQuota = u.Quota
		}
		if actualQuota < 0 {
			actualQuota = 0
		}
		actualTopup := quotaToRevoke
		if u.TopupQuota < actualTopup {
			actualTopup = u.TopupQuota
		}
		if actualTopup < 0 {
			actualTopup = 0
		}
		if err := tx.Model(&User{}).Where("id = ?", order.UserId).
			Updates(map[string]interface{}{
				"quota":       gorm.Expr("quota - ?", actualQuota),
				"topup_quota": gorm.Expr("topup_quota - ?", actualTopup),
			}).Error; err != nil {
			return err
		}
		if actualQuota < quotaToRevoke {
			logger.SysError(fmt.Sprintf(
				"refund for order %s: revoked %d of %d quota from user %d (balance insufficient)",
				orderId, actualQuota, quotaToRevoke, order.UserId))
		}

		// 冲正这笔充值产生的邀请返现，防「充值→拿返现→退款」套利
		inviterId, reversed, err := ReverseCommission(tx, orderId)
		if err != nil {
			return err
		}
		if inviterId > 0 && reversed > 0 {
			reversedInviterId = inviterId
			RecordLog(inviterId, LogTypeAffCommission, fmt.Sprintf(
				"referral commission reversed %s due to order %s refund",
				common.LogQuota(reversed), orderId))
		}
		return nil
	})
	if err != nil {
		return err
	}

	// 事务外刷缓存。
	// 刻意不调 RecalcUserLevelAndRefreshCache：等级只升不降是有意的产品决定，
	// 避免用户等级反复跳动引发客诉。若将来要支持降级，需显式实现而非依赖此处副作用。
	if refundedUserId > 0 {
		if err := CacheUpdateUserQuota2(refundedUserId); err != nil {
			logger.SysError("failed to refresh quota cache after refund: " + err.Error())
		}
	}
	if reversedInviterId > 0 {
		if err := CacheUpdateUserQuota2(reversedInviterId); err != nil {
			logger.SysError("failed to refresh inviter quota cache after refund: " + err.Error())
		}
	}
	return nil
}
func stripeChargeSuccess(charge *stripe.Charge) error {
	//获取meta数据里的订单id
	if charge.Status == "succeeded" {
		orderId := charge.Metadata["appOrderId"]
		userId := charge.Metadata["userId"]

		// 获取更新后的订单信息
		var chargeOrder ChargeOrder
		var bill Bill
		if err := DB.Model(&ChargeOrder{}).Where("app_order_id = ? ", orderId).Where("user_id = ?", userId).First(&chargeOrder).Error; err != nil {
			return err
		}

		var commissionInviterId int
		var commissionQuota int64

		if err := DB.Transaction(func(tx *gorm.DB) error {
			// 使用原子性数据库操作防止分布式并发。
			// 必须用 tx 版本：原实现调的是走全局 DB 的版本，改单动作根本不在
			// 事务里，事务回滚时不会被撤销。
			success := UpdateChargeOrderStatusWithConditionTx(tx, orderId, userId,
				StatusMap["create"], StatusMap["success"])
			if !success {
				// 订单已被处理或状态不符合预期，直接返回
				return errors.New("order has already been processed or has an unexpected status")
			}
			//更新订单详细信息
			amount := float64(charge.Amount / 100)
			orderCost := amount*0.029 + 0.3
			realAmount := amount - orderCost
			if err := tx.Model(&chargeOrder).Updates(ChargeOrder{Status: StatusMap["success"], RealAmount: realAmount, OrderCost: orderCost, OrderNo: charge.ID, Amount: amount}).Error; err != nil {
				return err
			}

			// quota 是可用余额，topup_quota 是累计真实充值（等级判定的唯一依据）。
			// 不用 IncreaseUserQuota：它走全局 DB 且受 BatchUpdateEnabled 影响，
			// 会脱离当前事务。
			quotaToAdd := AmountToQuota(amount)
			if err := tx.Model(&User{}).Where("id = ?", chargeOrder.UserId).
				Updates(map[string]interface{}{
					"quota":       gorm.Expr("quota + ?", quotaToAdd),
					"topup_quota": gorm.Expr("topup_quota + ?", quotaToAdd),
				}).Error; err != nil {
				return err
			}

			// 返现与入账同事务，source_no 用 app_order_id。
			// 用 := 接局部变量再赋给外层：闭包签名是 func(tx *gorm.DB) error。
			inviterId, cq, gErr := GrantCommission(
				tx, chargeOrder.UserId, amount, quotaToAdd,
				SourceTypeStripeCharge, chargeOrder.AppOrderId)
			if gErr != nil {
				return gErr
			}
			commissionInviterId = inviterId
			commissionQuota = cq

			if err := tx.Model(&Bill{}).Where("source_id = ?", orderId).First(&bill).Error; err != nil {
				return err
			}
			if err := tx.Model(&bill).Updates(Bill{Status: StatusMap["success"]}).Error; err != nil {
				return err
			}

			return nil
		}); err != nil {
			return err
		}

		// 以下都在事务外：Redis 不参与事务回滚
		if err := CacheUpdateUserQuota2(chargeOrder.UserId); err != nil {
			logger.SysError("failed to refresh quota cache: " + err.Error())
		}
		if commissionInviterId > 0 && commissionQuota > 0 {
			if err := CacheUpdateUserQuota2(commissionInviterId); err != nil {
				logger.SysError("failed to refresh inviter quota cache: " + err.Error())
			}
			RecordLog(commissionInviterId, LogTypeAffCommission, fmt.Sprintf(
				"referral commission %s from invitee %d top-up",
				common.LogQuota(commissionQuota), chargeOrder.UserId))
		}
		RecalcUserLevelAndRefreshCache(chargeOrder.UserId)

		//支付成功处理一下其它
		AfterChargeSuccess(chargeOrder.UserId, float64(charge.Amount/100-charge.ApplicationFeeAmount/100))
	}
	return nil
}

func HandleStripeCallback(req *http.Request) error {
	payload, err := io.ReadAll(req.Body)
	// logger.SysLog(fmt.Sprintf("stripePayload:%s\n", payload))
	// logger.SysLog(fmt.Sprintf("stripePayloaderr:%+v\n", err))
	// logger.SysLog(fmt.Sprintf("stripePayloadheader:%+v\n", req.Header.Get("Stripe-Signature")))
	if err != nil {
		//fmt.Fprintf(os.Stderr, "Error reading request body: %v\n", err)
		//w.WriteHeader(http.StatusServiceUnavailable)
		return err
	}

	event := stripe.Event{}

	if err := json.Unmarshal(payload, &event); err != nil {
		return err
	}
	endpointSecret := config.StripeEndpointSecret
	signatureHeader := req.Header.Get("Stripe-Signature")
	event, err = webhook.ConstructEventWithOptions(
		payload,
		signatureHeader,
		endpointSecret,
		webhook.ConstructEventOptions{
			IgnoreAPIVersionMismatch: true,
		},
	)
	if err != nil {
		logger.SysError(fmt.Sprintf("stripe webhook construct event failed: %v", err))
		return err
	}
	switch event.Type {
	case "payment_intent.succeeded":
		//var paymentIntent stripe.PaymentIntent
		var charge stripe.Charge
		err := json.Unmarshal(event.Data.Raw, &charge)
		if err != nil {
			logger.SysError(fmt.Sprintf("error parsing stripe webhook JSON: %v", err))
			return err
		}
		err = stripeChargeSuccess(&charge)
		if err != nil {
			return err
		}
	case "charge.refunded":
		var charge stripe.Charge
		err := json.Unmarshal(event.Data.Raw, &charge)
		if err != nil {
			logger.SysError(fmt.Sprintf("error parsing stripe webhook JSON: %v", err))
			return err
		}
		err = stripeChargeRefund(&charge)
		if err != nil {
			return err
		}
	case "charge.failed":
		var charge stripe.Charge
		err := json.Unmarshal(event.Data.Raw, &charge)
		if err != nil {
			logger.SysError(fmt.Sprintf("error parsing stripe webhook JSON: %v", err))
			return err
		}
		err = stripeChargeFail(&charge)
		if err != nil {
			return err
		}
	default:
		logger.SysLog(fmt.Sprintf("Unhandled event type: %s", event.Type))
	}
	return nil
}

// UpdateChargeOrderStatusWithCondition 原子性更新订单状态，防止分布式并发冲突
// 只有当当前状态等于expectedStatus时才更新为newStatus
func UpdateChargeOrderStatusWithCondition(appOrderId, userId string, expectedStatus, newStatus int) bool {
	return UpdateChargeOrderStatusWithConditionTx(DB, appOrderId, userId, expectedStatus, newStatus)
}

// UpdateChargeOrderStatusWithConditionTx 是上面那个的事务版本，也是真正的实现。
//
// 退款冲正与充值入账都需要「改单状态」与后续的额度变动原子。原实现在事务
// 闭包内调用走全局 DB 的版本，导致改单动作根本不在事务里 —— 事务回滚时
// 状态改动不会被撤销，订单会停在已处理状态而额度却没到账。
func UpdateChargeOrderStatusWithConditionTx(tx *gorm.DB, appOrderId, userId string,
	expectedStatus, newStatus int) bool {
	// 使用WHERE条件确保原子性更新
	result := tx.Model(&ChargeOrder{}).
		Where("app_order_id = ? AND user_id = ? AND status = ?", appOrderId, userId, expectedStatus).
		Update("status", newStatus)

	// 如果RowsAffected为1，说明更新成功
	return result.RowsAffected == 1
}
