package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

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
	if charge.Status == "succeeded" {
		//获取meta数据里的订单id
		orderId := charge.Metadata["appOrderId"]
		userId := charge.Metadata["userId"]

		// 使用原子性数据库操作防止分布式并发
		// 只有成功状态的订单才能退款
		success := UpdateChargeOrderStatusWithCondition(orderId, userId, StatusMap["success"], StatusMap["refund"])
		if !success {
			// 订单已被处理或状态不符合预期，直接返回
			return nil
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
		if err := DB.Transaction(func(tx *gorm.DB) error {
			// 使用原子性数据库操作防止分布式并发
			success := UpdateChargeOrderStatusWithCondition(orderId, userId, StatusMap["create"], StatusMap["success"])
			if !success {
				// 订单已被处理或状态不符合预期，直接返回
				return errors.New("order has already been processed or has an unexpected status")
			}
			//更新订单详细信息
			amount := float64(charge.Amount / 100)
			orderCost := amount*0.029 + 0.3
			realAmount := amount - orderCost
			if err := DB.Model(&chargeOrder).Updates(ChargeOrder{Status: StatusMap["success"], RealAmount: realAmount, OrderCost: orderCost, OrderNo: charge.ID, Amount: amount}).Error; err != nil {
				return err
			}
			//更新余额 待定手续费和用户组别的变更
			err := IncreaseUserQuota(chargeOrder.UserId, int64(amount*500000))
			if err != nil {
				return err
			}

			if err := DB.Model(&Bill{}).Where("source_id = ?", orderId).First(&bill).Error; err != nil {
				return err
			}
			if err := DB.Model(&bill).Updates(Bill{Status: StatusMap["success"]}).Error; err != nil {
				return err
			}

			return nil
		}); err != nil {
			return err
		}
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
	// 使用WHERE条件确保原子性更新
	result := DB.Model(&ChargeOrder{}).
		Where("app_order_id = ? AND user_id = ? AND status = ?", appOrderId, userId, expectedStatus).
		Update("status", newStatus)

	// 如果RowsAffected为1，说明更新成功
	return result.RowsAffected == 1
}
