package model

import (
	"testing"
)

// TestAffCommissionRecordUniqueSourceNo source_no 的唯一索引是幂等的核心：
// Stripe 会重放 webhook，唯一索引是防重复发放的最后一道保险。
func TestAffCommissionRecordUniqueSourceNo(t *testing.T) {
	db := setupTestDB(t, &AffCommissionRecord{})

	first := &AffCommissionRecord{
		InviterId: 1, InviteeId: 2,
		SourceType: SourceTypeStripeCheckout, SourceNo: "trade-001",
		TopupAmount: 100, TopupQuota: 50000000,
		Rate: 0.08, InviterGroup: "Lv4", CommissionQuota: 4000000,
		Status: AffCommissionStatusGranted, CreatedAt: 1000,
	}
	if err := db.Create(first).Error; err != nil {
		t.Fatalf("first insert should succeed: %v", err)
	}

	// 同一个 source_no 再插一次必须失败
	dup := *first
	dup.Id = 0
	if err := db.Create(&dup).Error; err == nil {
		t.Fatal("duplicate source_no was accepted; unique index missing")
	}
}

// TestGetAffCommissionRecordBySourceNo 冲正逻辑要按 source_no 定位记录。
func TestGetAffCommissionRecordBySourceNo(t *testing.T) {
	setupTestDB(t, &AffCommissionRecord{})

	rec := &AffCommissionRecord{
		InviterId: 7, InviteeId: 8,
		SourceType: SourceTypeStripeCharge, SourceNo: "order-042",
		TopupAmount: 20, TopupQuota: 10000000,
		Rate: 0.05, InviterGroup: "Lv3", CommissionQuota: 500000,
		Status: AffCommissionStatusGranted, CreatedAt: 2000,
	}
	if err := DB.Create(rec).Error; err != nil {
		t.Fatalf("create failed: %v", err)
	}

	got, err := GetAffCommissionRecordBySourceNo("order-042")
	if err != nil {
		t.Fatalf("lookup failed: %v", err)
	}
	if got == nil {
		t.Fatal("got nil record for existing source_no")
	}
	if got.InviterId != 7 || got.CommissionQuota != 500000 {
		t.Errorf("got InviterId=%d CommissionQuota=%d, want 7 / 500000",
			got.InviterId, got.CommissionQuota)
	}

	// 不存在的 source_no 必须返回 (nil, nil) 而非 error —— 调用方据此判断
	// 「那笔充值本来就没有返现」，不该当成故障处理
	missing, err := GetAffCommissionRecordBySourceNo("does-not-exist")
	if err != nil {
		t.Errorf("missing record should not error, got: %v", err)
	}
	if missing != nil {
		t.Errorf("missing record should be nil, got %+v", missing)
	}
}

// TestGetAffCommissionSummary 邀请汇总接口的数据来源。
func TestGetAffCommissionSummary(t *testing.T) {
	setupTestDB(t, &AffCommissionRecord{})

	rows := []AffCommissionRecord{
		{InviterId: 1, InviteeId: 2, SourceNo: "a", CommissionQuota: 100, Status: AffCommissionStatusGranted, CreatedAt: 1},
		{InviterId: 1, InviteeId: 3, SourceNo: "b", CommissionQuota: 200, Status: AffCommissionStatusGranted, CreatedAt: 2},
		// 已冲正的不计入累计收益
		{InviterId: 1, InviteeId: 4, SourceNo: "c", CommissionQuota: 900, Status: AffCommissionStatusReversed, CreatedAt: 3},
		// 别人的记录不能串进来
		{InviterId: 99, InviteeId: 5, SourceNo: "d", CommissionQuota: 700, Status: AffCommissionStatusGranted, CreatedAt: 4},
	}
	for i := range rows {
		if err := DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("create failed: %v", err)
		}
	}

	total, count, err := GetAffCommissionSummary(1)
	if err != nil {
		t.Fatalf("summary failed: %v", err)
	}
	if total != 300 {
		t.Errorf("total = %d, want 300 (已冲正的 900 不计入)", total)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}

	// 无任何记录的用户：SUM 返回 NULL，必须被 COALESCE 兜成 0 而非报错
	zeroTotal, zeroCount, err := GetAffCommissionSummary(12345)
	if err != nil {
		t.Errorf("summary for user with no records should not error, got: %v", err)
	}
	if zeroTotal != 0 || zeroCount != 0 {
		t.Errorf("got total=%d count=%d, want 0 / 0", zeroTotal, zeroCount)
	}
}
