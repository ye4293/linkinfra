package model

import (
	"testing"

	"github.com/songquanpeng/one-api/common/config"
)

// TestCompleteTopUpOrderWithCommission 覆盖 Stripe Checkout 的完整入账链路。
//
// 这条测试有两个目的：
//
//  1. 回归保护行锁改动。原先用的 Set("gorm:query_option", "FOR UPDATE")
//     是 GORM v1 的 API，在 v2 里是 no-op；改成 clause.Locking 后必须确认
//     真实代码路径没被打断。sqlite 驱动会静默剥离行锁子句
//     （driver/sqlite/sqlite.go 的 "FOR" ClauseBuilder 注释
//     "SQLite3 does not support row-level locking" 后直接 return），
//     所以这里验的是"加了锁子句不会让 sqlite 报错"，
//     而 PG/MySQL 上真正生成 FOR UPDATE 由驱动源码保证（两者都没有覆盖 FOR）。
//
//  2. completeTopUpOrder 此前零测试覆盖，而 P3 往里加了 topup_quota 累加
//     与返现发放。这条测试把那条链路整体锁住。
func TestCompleteTopUpOrderWithCommission(t *testing.T) {
	setupTestDB(t, &User{}, &TopUp{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})

	orig := config.QuotaPerUnit
	config.QuotaPerUnit = 500000
	t.Cleanup(func() { config.QuotaPerUnit = orig })

	// 邀请人 Lv4，返现 8%
	if err := DB.Create(&GroupConfig{
		GroupKey: "Lv4", DisplayName: "Lv4", Discount: 0.85, CommissionRate: 0.08,
	}).Error; err != nil {
		t.Fatalf("seed group failed: %v", err)
	}
	inviter := &User{Username: "inviter", Group: "Lv4", AffCode: "inv", AccessToken: "t-inv"}
	if err := DB.Create(inviter).Error; err != nil {
		t.Fatalf("create inviter failed: %v", err)
	}
	invitee := &User{Username: "invitee", Group: "Lv1", AffCode: "ive", AccessToken: "t-ive", InviterId: inviter.Id}
	if err := DB.Create(invitee).Error; err != nil {
		t.Fatalf("create invitee failed: %v", err)
	}

	// Amount 是额度单位数，Money 是实付货币金额（返现按 Money 算）
	if err := DB.Create(&TopUp{
		UserId: invitee.Id, Amount: 100, Money: 100,
		TradeNo: "trade-x", Status: "pending",
	}).Error; err != nil {
		t.Fatalf("create topup failed: %v", err)
	}

	if err := CompleteTopUpOrder("trade-x"); err != nil {
		t.Fatalf("CompleteTopUpOrder failed: %v", err)
	}

	// 充值方：quota 与 topup_quota 各 +50000000
	var afterInvitee User
	if err := DB.First(&afterInvitee, invitee.Id).Error; err != nil {
		t.Fatalf("read invitee failed: %v", err)
	}
	if afterInvitee.Quota != 50000000 {
		t.Errorf("invitee Quota = %d, want 50000000", afterInvitee.Quota)
	}
	if afterInvitee.TopupQuota != 50000000 {
		t.Errorf("invitee TopupQuota = %d, want 50000000", afterInvitee.TopupQuota)
	}

	// 邀请人：8% 返现 = 4000000
	var afterInviter User
	if err := DB.First(&afterInviter, inviter.Id).Error; err != nil {
		t.Fatalf("read inviter failed: %v", err)
	}
	if afterInviter.Quota != 4000000 {
		t.Errorf("inviter Quota = %d, want 4000000", afterInviter.Quota)
	}
	if afterInviter.GiftQuota != 4000000 {
		t.Errorf("inviter GiftQuota = %d, want 4000000", afterInviter.GiftQuota)
	}

	// 订单状态已改
	var topUp TopUp
	if err := DB.Where("trade_no = ?", "trade-x").First(&topUp).Error; err != nil {
		t.Fatalf("read topup failed: %v", err)
	}
	if topUp.Status != "success" {
		t.Errorf("Status = %q, want success", topUp.Status)
	}

	// 重复调用必须无副作用（status != pending 早退）
	if err := CompleteTopUpOrder("trade-x"); err != nil {
		t.Fatalf("重复调用不应报错: %v", err)
	}
	var replayInviter User
	_ = DB.First(&replayInviter, inviter.Id).Error
	if replayInviter.Quota != 4000000 {
		t.Errorf("重复调用后 inviter Quota = %d, want 4000000（不该累加）", replayInviter.Quota)
	}
}

// TestCompleteTopUpOrderManualNoCommission 管理员手工补单是运营白送的额度，
// 既不计入 topup_quota（等级判定基准）也不产生返现。
func TestCompleteTopUpOrderManualNoCommission(t *testing.T) {
	setupTestDB(t, &User{}, &TopUp{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})

	orig := config.QuotaPerUnit
	config.QuotaPerUnit = 500000
	t.Cleanup(func() { config.QuotaPerUnit = orig })

	if err := DB.Create(&GroupConfig{
		GroupKey: "Lv4", DisplayName: "Lv4", Discount: 0.85, CommissionRate: 0.08,
	}).Error; err != nil {
		t.Fatalf("seed group failed: %v", err)
	}
	inviter := &User{Username: "inviter", Group: "Lv4", AffCode: "inv", AccessToken: "t-inv"}
	if err := DB.Create(inviter).Error; err != nil {
		t.Fatalf("create inviter failed: %v", err)
	}
	invitee := &User{Username: "invitee", Group: "Lv1", AffCode: "ive", AccessToken: "t-ive", InviterId: inviter.Id}
	if err := DB.Create(invitee).Error; err != nil {
		t.Fatalf("create invitee failed: %v", err)
	}
	if err := DB.Create(&TopUp{
		UserId: invitee.Id, Amount: 100, Money: 100,
		TradeNo: "trade-manual", Status: "pending",
	}).Error; err != nil {
		t.Fatalf("create topup failed: %v", err)
	}

	err := CompleteTopUpOrderManual("trade-manual", TopUpManualCompleteMeta{
		OperatorUserId: 1, Source: "manual_complete",
	})
	if err != nil {
		t.Fatalf("CompleteTopUpOrderManual failed: %v", err)
	}

	var afterInvitee User
	_ = DB.First(&afterInvitee, invitee.Id).Error
	// 余额照常到账
	if afterInvitee.Quota != 50000000 {
		t.Errorf("invitee Quota = %d, want 50000000", afterInvitee.Quota)
	}
	// 但不计入累计真实充值
	if afterInvitee.TopupQuota != 0 {
		t.Errorf("invitee TopupQuota = %d, want 0（补单不算真实充值）", afterInvitee.TopupQuota)
	}

	var afterInviter User
	_ = DB.First(&afterInviter, inviter.Id).Error
	if afterInviter.Quota != 0 {
		t.Errorf("inviter Quota = %d, want 0（补单不产生返现）", afterInviter.Quota)
	}

	var recordCount int64
	DB.Model(&AffCommissionRecord{}).Count(&recordCount)
	if recordCount != 0 {
		t.Errorf("返现记录数 = %d, want 0", recordCount)
	}
}
