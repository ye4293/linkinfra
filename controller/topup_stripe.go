package controller

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/model"
	"github.com/stripe/stripe-go/v78"
	"github.com/stripe/stripe-go/v78/checkout/session"
	"github.com/stripe/stripe-go/v78/paymentintent"
	"github.com/stripe/stripe-go/v78/webhook"
)

const PaymentMethodStripe = "stripe"

type StripePayRequest struct {
	Amount        int64  `json:"amount"`
	PaymentMethod string `json:"payment_method"`
	SuccessURL    string `json:"success_url,omitempty"`
	CancelURL     string `json:"cancel_url,omitempty"`
}

func getStripeAvailability() (bool, string) {
	if !config.StripePaymentEnabled {
		return false, "Stripe payments are not enabled."
	}
	if config.StripeApiSecret == "" {
		return false, "Stripe API Secret is not configured."
	}
	if config.StripeWebhookSecret == "" {
		return false, "Stripe Webhook Secret is not configured."
	}
	if config.StripePriceId == "" {
		return false, "Stripe Price ID is not configured."
	}
	return true, ""
}

func genStripeTradeNo(userId int) string {
	raw := fmt.Sprintf("one-api-ref-%d-%d-%s", userId, time.Now().UnixMilli(), helper.GetRandomString(6))
	hash := sha256.Sum256([]byte(raw))
	return "ref_" + fmt.Sprintf("%x", hash[:16])
}

func RequestStripePay(c *gin.Context) {
	var req StripePayRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "Invalid parameters."})
		return
	}

	enabled, reason := getStripeAvailability()
	if !enabled {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": reason})
		return
	}

	if req.PaymentMethod == "" {
		req.PaymentMethod = PaymentMethodStripe
	}
	if req.PaymentMethod != PaymentMethodStripe {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "Unsupported payment method."})
		return
	}
	if req.Amount < int64(config.StripeMinTopUp) {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": fmt.Sprintf("Minimum top-up amount is %d.", config.StripeMinTopUp)})
		return
	}
	if req.Amount > 10000 {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "Top-up amount cannot exceed 10,000."})
		return
	}

	userId := c.GetInt("id")
	tradeNo := genStripeTradeNo(userId)

	payLink, err := genStripeCheckoutLink(tradeNo, req.Amount, req.SuccessURL, req.CancelURL)
	if err != nil {
		log.Printf("创建 Stripe Checkout 失败: %v\n", err)
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "Failed to initiate payment."})
		return
	}

	if err := model.CreateStripeTopUp(userId, req.Amount, tradeNo); err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "Failed to create order."})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"pay_link": payLink,
		},
	})
}

func genStripeCheckoutLink(referenceId string, amount int64, successURL string, cancelURL string) (string, error) {
	if !strings.HasPrefix(config.StripeApiSecret, "sk_") && !strings.HasPrefix(config.StripeApiSecret, "rk_") {
		return "", fmt.Errorf("invalid Stripe API key")
	}

	stripe.Key = config.StripeApiSecret

	
	if config.FrontendServerAddress != "" {
		successURL = config.FrontendServerAddress + "/dashboard/topup"
		cancelURL = config.FrontendServerAddress + "/dashboard/topup"
	}else if config.ServerAddress != "" {
		successURL = config.ServerAddress + "/dashboard/topup"
		cancelURL = config.ServerAddress + "/dashboard/topup"
	}

	params := &stripe.CheckoutSessionParams{
		ClientReferenceID: stripe.String(referenceId),
		SuccessURL:        stripe.String(successURL),
		CancelURL:         stripe.String(cancelURL),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(config.StripePriceId),
				Quantity: stripe.Int64(amount),
			},
		},
		Mode:                stripe.String(string(stripe.CheckoutSessionModePayment)),
		AllowPromotionCodes: stripe.Bool(config.StripePromotionCodesEnabled),
	}

	params.CustomerCreation = stripe.String(string(stripe.CheckoutSessionCustomerCreationAlways))

	result, err := session.New(params)
	if err != nil {
		return "", err
	}

	return result.URL, nil
}

func StripeWebhook(c *gin.Context) {
	payload, err := io.ReadAll(c.Request.Body)
	if err != nil {
		log.Printf("读取 Stripe Webhook 请求体失败: %v\n", err)
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}

	signature := c.GetHeader("Stripe-Signature")
	event, err := webhook.ConstructEventWithOptions(payload, signature, config.StripeWebhookSecret, webhook.ConstructEventOptions{
		IgnoreAPIVersionMismatch: true,
	})
	if err != nil {
		log.Printf("Stripe Webhook 验签失败: %v\n", err)
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	switch event.Type {
	case stripe.EventTypeCheckoutSessionCompleted:
		if err := stripeSessionCompleted(event); err != nil {
			// 返回非 2xx 让 Stripe 重试（拿不到净额、入账失败等）。
			// 已完成的订单重试时由 status != pending 幂等早退，不会重复加额度。
			log.Printf("Stripe Webhook 处理失败，将重试: %v\n", err)
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
	case stripe.EventTypeCheckoutSessionExpired:
		stripeSessionExpired(event)
	default:
		log.Printf("不支持的 Stripe Webhook 事件类型: %s\n", event.Type)
	}

	c.Status(http.StatusOK)
}

func stripeSessionCompleted(event stripe.Event) error {
	referenceId := event.GetObjectValue("client_reference_id")
	status := event.GetObjectValue("status")
	if status != "complete" {
		log.Printf("Stripe Checkout 状态异常: %s, 订单: %s\n", status, referenceId)
		return nil
	}

	if referenceId == "" {
		log.Println("Stripe Webhook 未提供 client_reference_id")
		return nil
	}

	LockOrder(referenceId)
	defer UnlockOrder(referenceId)

	piId := event.GetObjectValue("payment_intent")
	if piId == "" {
		log.Printf("Stripe Webhook 未提供 payment_intent: tradeNo=%s\n", referenceId)
		return errors.New("missing payment_intent")
	}

	// 取扣手续费后的净额（balance_transaction.net）作为额度基准，
	// 而非 Checkout 的 amount_total 毛额。
	netTotal, currency, err := fetchStripeNetAmount(piId)
	if err != nil {
		log.Printf("Stripe 获取净额失败: %s, payment_intent=%s, 错误: %v\n", referenceId, piId, err)
		return err
	}

	if strings.ToUpper(currency) != "USD" {
		// QuotaPerUnit 按 USD 定义，非 USD 结算需人工核查。不阻断但告警。
		log.Printf("Stripe 结算货币非 USD: %s, tradeNo=%s, 需人工核查\n", currency, referenceId)
	}

	if err := model.CompleteStripeTopUpFromCheckout(referenceId, netTotal, currency); err != nil {
		log.Printf("Stripe 充值完成失败: %s, 错误: %v\n", referenceId, err)
		return err
	}

	netMajor := model.StripeAmountTotalToMajor(netTotal, currency)
	log.Printf("Stripe 净收入账: %s, %.2f %s\n", referenceId, netMajor, strings.ToUpper(currency))
	return nil
}

// fetchStripeNetAmount 通过 PaymentIntent 取扣手续费后的净额。
// 路径：payment_intent → latest_charge → balance_transaction → net。
// checkout.session.completed 事件对象不含 balance_transaction，需主动检索。
// 异步支付方式下 balance_transaction 可能尚未就绪，此时返回错误让 webhook 重试。
func fetchStripeNetAmount(paymentIntentId string) (netTotal int64, currency string, err error) {
	stripe.Key = config.StripeApiSecret

	params := &stripe.PaymentIntentParams{}
	params.AddExpand("latest_charge.balance_transaction")

	pi, err := paymentintent.Get(paymentIntentId, params)
	if err != nil {
		return 0, "", err
	}
	if pi.LatestCharge == nil || pi.LatestCharge.BalanceTransaction == nil {
		return 0, "", errors.New("balance_transaction not ready")
	}

	bt := pi.LatestCharge.BalanceTransaction
	return bt.Net, string(bt.Currency), nil
}

func stripeSessionExpired(event stripe.Event) {
	referenceId := event.GetObjectValue("client_reference_id")
	if referenceId == "" {
		log.Println("Stripe Webhook 过期事件未提供订单号")
		return
	}

	LockOrder(referenceId)
	defer UnlockOrder(referenceId)

	if err := model.ExpireStripeTopUp(referenceId); err != nil {
		log.Printf("Stripe 订单过期处理失败: %s, 错误: %v\n", referenceId, err)
		return
	}

	log.Printf("Stripe 订单已过期: %s\n", referenceId)
}
