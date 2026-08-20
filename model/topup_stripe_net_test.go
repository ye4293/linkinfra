package model

import (
	"testing"

	"github.com/songquanpeng/one-api/common/config"
)

// TestCompleteStripeTopUpByNetAmount 验证 Stripe 充值按「扣手续费后的净额」到账，
// 而非用户实付金额或下单 amount 数量。
//
// 实测 Stripe 测试模式（card pm_card_visa, charge $10.00）：
//   - balance_transaction.amount = 1000（毛额）
//   - balance_transaction.fee    = 59    ($0.59 = 2.9% + $0.30)
//   - balance_transaction.net     = 941   ($9.41)
//
// 因此 webhook 拿到 net=941 cents 后，到账额度应为
// AmountToQuota(9.41) = round(9.41 * 500000) = 4,705,000，
// 而非按 amount=10 算的 5,000,000 —— 这条断言锁住「手续费由用户额度承担」的语义。
func TestCompleteStripeTopUpByNetAmount(t *testing.T) {
	setupTestDB(t, &User{}, &TopUp{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})

	orig := config.QuotaPerUnit
	config.QuotaPerUnit = 500000
	t.Cleanup(func() { config.QuotaPerUnit = orig })

	// Lv1 默认组（completeTopUpOrder 走真实充值路径，需有 group）
	if err := DB.Create(&GroupConfig{
		GroupKey: "Lv1", DisplayName: "Lv1", Discount: 1, CommissionRate: 0,
	}).Error; err != nil {
		t.Fatalf("seed group failed: %v", err)
	}
	user := &User{Username: "netuser", Group: "Lv1", AccessToken: "t-net"}
	if err := DB.Create(user).Error; err != nil {
		t.Fatalf("create user failed: %v", err)
	}

	// 下单 amount=10（用户实付 $10），订单 Money 先记 $10
	if err := DB.Create(&TopUp{
		UserId: user.Id, Amount: 10, Money: 10,
		TradeNo: "trade-net", Status: "pending", Currency: "USD",
	}).Error; err != nil {
		t.Fatalf("create topup failed: %v", err)
	}

	// webhook 拿到的净额：941 cents = $9.41（扣 $0.59 手续费后）
	if err := CompleteStripeTopUpFromCheckout("trade-net", 941, "usd"); err != nil {
		t.Fatalf("CompleteStripeTopUpFromCheckout failed: %v", err)
	}

	var after User
	if err := DB.First(&after, user.Id).Error; err != nil {
		t.Fatalf("read user failed: %v", err)
	}
	// 关键断言：到账按净额 $9.41 折算 = 4,705,000，而非按 amount=10 的 5,000,000
	want := int64(4705000)
	if after.Quota != want {
		t.Errorf("user Quota = %d, want %d (net $9.41 * 500000; 不应是按 amount 的 5000000)", after.Quota, want)
	}
	if after.TopupQuota != want {
		t.Errorf("user TopupQuota = %d, want %d", after.TopupQuota, want)
	}

	var topUp TopUp
	if err := DB.Where("trade_no = ?", "trade-net").First(&topUp).Error; err != nil {
		t.Fatalf("read topup failed: %v", err)
	}
	if topUp.Status != "success" {
		t.Errorf("Status = %q, want success", topUp.Status)
	}
	// 订单 Money 被净额覆盖（从 $10 → $9.41）
	if topUp.Money != 9.41 {
		t.Errorf("TopUp.Money = %v, want 9.41 (净额覆盖)", topUp.Money)
	}
	if topUp.Currency != "USD" {
		t.Errorf("TopUp.Currency = %q, want USD", topUp.Currency)
	}

	// 重复调用幂等：status != pending 早退，不重复加额度
	if err := CompleteStripeTopUpFromCheckout("trade-net", 941, "usd"); err != nil {
		t.Fatalf("重复调用不应报错: %v", err)
	}
	var replay User
	_ = DB.First(&replay, user.Id).Error
	if replay.Quota != want {
		t.Errorf("重复调用后 Quota = %d, want %d（不该累加）", replay.Quota, want)
	}
}
