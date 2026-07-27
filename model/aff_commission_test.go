package model

import (
	"testing"

	"gorm.io/gorm"
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

// grantFixture 建立「邀请人 Lv4 返现 8%，被邀请人已绑定邀请人」的场景。
func grantFixture(t *testing.T) (inviter, invitee *User) {
	t.Helper()

	cfgs := []GroupConfig{
		{GroupKey: "Lv1", DisplayName: "Lv1", Discount: 1, SortOrder: 0, CommissionRate: 0},
		{GroupKey: "Lv4", DisplayName: "Lv4", Discount: 0.85, SortOrder: 3, CommissionRate: 0.08},
	}
	for i := range cfgs {
		if err := DB.Create(&cfgs[i]).Error; err != nil {
			t.Fatalf("seed group failed: %v", err)
		}
	}

	inviter = &User{Username: "inviter", Group: "Lv4", AffCode: "inv1", AccessToken: "t-inviter"}
	if err := DB.Create(inviter).Error; err != nil {
		t.Fatalf("create inviter failed: %v", err)
	}
	invitee = &User{Username: "invitee", Group: "Lv1", AffCode: "inv2", AccessToken: "t-invitee", InviterId: inviter.Id}
	if err := DB.Create(invitee).Error; err != nil {
		t.Fatalf("create invitee failed: %v", err)
	}
	return inviter, invitee
}

func TestGrantCommission(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
	inviter, invitee := grantFixture(t)

	// 被邀请人充 $100，邀请人 Lv4 返 8% = $8 = 4000000 quota
	err := DB.Transaction(func(tx *gorm.DB) error {
		gotInviter, commission, err := GrantCommission(
			tx, invitee.Id, 100, 50000000, SourceTypeStripeCheckout, "trade-100")
		if err != nil {
			return err
		}
		if gotInviter != inviter.Id {
			t.Errorf("inviterId = %d, want %d", gotInviter, inviter.Id)
		}
		if commission != 4000000 {
			t.Errorf("commission = %d, want 4000000", commission)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("transaction failed: %v", err)
	}

	// 邀请人的 quota 与 gift_quota 都要 +4000000
	var after User
	if err := DB.First(&after, inviter.Id).Error; err != nil {
		t.Fatalf("read inviter failed: %v", err)
	}
	if after.Quota != 4000000 {
		t.Errorf("inviter Quota = %d, want 4000000", after.Quota)
	}
	if after.GiftQuota != 4000000 {
		t.Errorf("inviter GiftQuota = %d, want 4000000", after.GiftQuota)
	}

	// 明细记录的快照字段
	rec, err := GetAffCommissionRecordBySourceNo("trade-100")
	if err != nil || rec == nil {
		t.Fatalf("record not found: %v", err)
	}
	if rec.Rate != 0.08 {
		t.Errorf("Rate = %v, want 0.08", rec.Rate)
	}
	if rec.InviterGroup != "Lv4" {
		t.Errorf("InviterGroup = %q, want Lv4", rec.InviterGroup)
	}
	if rec.InviterUsername != "inviter" || rec.InviteeUsername != "invitee" {
		t.Errorf("username snapshot wrong: %q / %q", rec.InviterUsername, rec.InviteeUsername)
	}
	if rec.Status != AffCommissionStatusGranted {
		t.Errorf("Status = %d, want %d", rec.Status, AffCommissionStatusGranted)
	}
}

// TestGrantCommissionIdempotent Stripe 会重放 webhook，同一个 source_no
// 重复发放必须被挡住，且余额不能二次增加。
func TestGrantCommissionIdempotent(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
	inviter, invitee := grantFixture(t)

	for i := 0; i < 3; i++ {
		err := DB.Transaction(func(tx *gorm.DB) error {
			_, _, err := GrantCommission(tx, invitee.Id, 100, 50000000,
				SourceTypeStripeCheckout, "trade-replay")
			return err
		})
		if err != nil {
			t.Fatalf("第 %d 次调用不应报错: %v", i+1, err)
		}
	}

	var after User
	_ = DB.First(&after, inviter.Id).Error
	if after.Quota != 4000000 {
		t.Errorf("Quota = %d, want 4000000（重放不该累加）", after.Quota)
	}

	var count int64
	DB.Model(&AffCommissionRecord{}).Where("source_no = ?", "trade-replay").Count(&count)
	if count != 1 {
		t.Errorf("记录数 = %d, want 1", count)
	}
}

func TestGrantCommissionEarlyReturns(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) int // 返回 inviteeId
	}{
		{
			name: "无邀请人",
			setup: func(t *testing.T) int {
				u := &User{Username: "solo", AffCode: "solo", AccessToken: "t-solo", InviterId: 0}
				if err := DB.Create(u).Error; err != nil {
					t.Fatalf("create failed: %v", err)
				}
				return u.Id
			},
		},
		{
			name: "邀请人等级返现比例为0",
			setup: func(t *testing.T) int {
				cfg := GroupConfig{GroupKey: "Lv1", DisplayName: "Lv1", Discount: 1, CommissionRate: 0}
				if err := DB.Create(&cfg).Error; err != nil {
					t.Fatalf("seed failed: %v", err)
				}
				inviter := &User{Username: "inv", Group: "Lv1", AffCode: "a1", AccessToken: "t-a1"}
				if err := DB.Create(inviter).Error; err != nil {
					t.Fatalf("create failed: %v", err)
				}
				u := &User{Username: "u", AffCode: "a2", AccessToken: "t-a2", InviterId: inviter.Id}
				if err := DB.Create(u).Error; err != nil {
					t.Fatalf("create failed: %v", err)
				}
				return u.Id
			},
		},
		{
			name: "邀请人分组配置缺失时降级为不返现",
			setup: func(t *testing.T) int {
				inviter := &User{Username: "inv", Group: "GhostGroup", AffCode: "b1", AccessToken: "t-b1"}
				if err := DB.Create(inviter).Error; err != nil {
					t.Fatalf("create failed: %v", err)
				}
				u := &User{Username: "u", AffCode: "b2", AccessToken: "t-b2", InviterId: inviter.Id}
				if err := DB.Create(u).Error; err != nil {
					t.Fatalf("create failed: %v", err)
				}
				return u.Id
			},
		},
		{
			name: "自邀请",
			setup: func(t *testing.T) int {
				cfg := GroupConfig{GroupKey: "Lv4", DisplayName: "Lv4", Discount: 0.85, CommissionRate: 0.08}
				if err := DB.Create(&cfg).Error; err != nil {
					t.Fatalf("seed failed: %v", err)
				}
				u := &User{Username: "self", Group: "Lv4", AffCode: "c1", AccessToken: "t-c1"}
				if err := DB.Create(u).Error; err != nil {
					t.Fatalf("create failed: %v", err)
				}
				// 手工把 inviter_id 指向自己
				if err := DB.Model(&User{}).Where("id = ?", u.Id).
					Update("inviter_id", u.Id).Error; err != nil {
					t.Fatalf("update failed: %v", err)
				}
				return u.Id
			},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
			inviteeId := tt.setup(t)

			var gotInviter int
			var commission int64
			err := DB.Transaction(func(tx *gorm.DB) error {
				var err error
				gotInviter, commission, err = GrantCommission(
					tx, inviteeId, 100, 50000000,
					SourceTypeStripeCheckout, "trade-early-"+string(rune('a'+i)))
				return err
			})
			if err != nil {
				t.Fatalf("早退分支不应报错: %v", err)
			}
			if gotInviter != 0 || commission != 0 {
				t.Errorf("got (%d, %d), want (0, 0)", gotInviter, commission)
			}

			var count int64
			DB.Model(&AffCommissionRecord{}).Count(&count)
			if count != 0 {
				t.Errorf("记录数 = %d, want 0（早退不该产生记录）", count)
			}
		})
	}
}

// TestGrantCommissionRounding 返现额向下取整，不足 1 quota 时不产生记录。
func TestGrantCommissionRounding(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
	_, invitee := grantFixture(t)

	// $0.0000001 × 8% × 500000 = 0.004 → 取整为 0，不发放
	var commission int64
	err := DB.Transaction(func(tx *gorm.DB) error {
		var err error
		_, commission, err = GrantCommission(tx, invitee.Id, 0.0000001, 50,
			SourceTypeStripeCheckout, "trade-tiny")
		return err
	})
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if commission != 0 {
		t.Errorf("commission = %d, want 0", commission)
	}

	var count int64
	DB.Model(&AffCommissionRecord{}).Count(&count)
	if count != 0 {
		t.Errorf("记录数 = %d, want 0（返现额为 0 不该产生记录）", count)
	}
}

func TestReverseCommission(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
	inviter, invitee := grantFixture(t)

	// 先正常发放
	err := DB.Transaction(func(tx *gorm.DB) error {
		_, _, err := GrantCommission(tx, invitee.Id, 100, 50000000,
			SourceTypeStripeCharge, "order-refund")
		return err
	})
	if err != nil {
		t.Fatalf("grant failed: %v", err)
	}

	// 再冲正
	var gotInviter int
	var reversed int64
	err = DB.Transaction(func(tx *gorm.DB) error {
		var err error
		gotInviter, reversed, err = ReverseCommission(tx, "order-refund")
		return err
	})
	if err != nil {
		t.Fatalf("reverse failed: %v", err)
	}
	if gotInviter != inviter.Id {
		t.Errorf("inviterId = %d, want %d", gotInviter, inviter.Id)
	}
	if reversed != 4000000 {
		t.Errorf("reversed = %d, want 4000000", reversed)
	}

	var after User
	_ = DB.First(&after, inviter.Id).Error
	if after.Quota != 0 {
		t.Errorf("Quota = %d, want 0", after.Quota)
	}
	if after.GiftQuota != 0 {
		t.Errorf("GiftQuota = %d, want 0", after.GiftQuota)
	}

	rec, _ := GetAffCommissionRecordBySourceNo("order-refund")
	if rec == nil {
		t.Fatal("record disappeared")
	}
	if rec.Status != AffCommissionStatusReversed {
		t.Errorf("Status = %d, want %d", rec.Status, AffCommissionStatusReversed)
	}
	if rec.ReversedQuota != 4000000 {
		t.Errorf("ReversedQuota = %d, want 4000000", rec.ReversedQuota)
	}
	if rec.ReversedAt == 0 {
		t.Error("ReversedAt 未设置")
	}
}

// TestReverseCommissionInsufficientBalance 邀请人已把返现花掉时，
// 扣到 0 为止，绝不产生负余额。差额记在 reversed_quota 与 commission_quota
// 的落差里，是运营的真实损失。
func TestReverseCommissionInsufficientBalance(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
	inviter, invitee := grantFixture(t)

	err := DB.Transaction(func(tx *gorm.DB) error {
		_, _, err := GrantCommission(tx, invitee.Id, 100, 50000000,
			SourceTypeStripeCharge, "order-spent")
		return err
	})
	if err != nil {
		t.Fatalf("grant failed: %v", err)
	}

	// 邀请人把大部分返现花掉，只剩 1000000
	if err := DB.Model(&User{}).Where("id = ?", inviter.Id).
		Update("quota", 1000000).Error; err != nil {
		t.Fatalf("update quota failed: %v", err)
	}

	var reversed int64
	err = DB.Transaction(func(tx *gorm.DB) error {
		var err error
		_, reversed, err = ReverseCommission(tx, "order-spent")
		return err
	})
	if err != nil {
		t.Fatalf("reverse failed: %v", err)
	}
	if reversed != 1000000 {
		t.Errorf("reversed = %d, want 1000000（只能扣到 0）", reversed)
	}

	var after User
	_ = DB.First(&after, inviter.Id).Error
	if after.Quota != 0 {
		t.Errorf("Quota = %d, want 0（绝不能为负）", after.Quota)
	}

	rec, _ := GetAffCommissionRecordBySourceNo("order-spent")
	if rec.ReversedQuota != 1000000 {
		t.Errorf("ReversedQuota = %d, want 1000000", rec.ReversedQuota)
	}
	if rec.CommissionQuota != 4000000 {
		t.Errorf("CommissionQuota 应保留原值 4000000, got %d", rec.CommissionQuota)
	}
}

func TestReverseCommissionIdempotentAndMissing(t *testing.T) {
	t.Run("重复冲正只生效一次", func(t *testing.T) {
		setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
		inviter, invitee := grantFixture(t)

		_ = DB.Transaction(func(tx *gorm.DB) error {
			_, _, err := GrantCommission(tx, invitee.Id, 100, 50000000,
				SourceTypeStripeCharge, "order-twice")
			return err
		})
		// 额外给邀请人一些余额，确保第二次冲正若生效会被察觉
		_ = DB.Model(&User{}).Where("id = ?", inviter.Id).
			Update("quota", gorm.Expr("quota + ?", 9000000)).Error

		for i := 0; i < 3; i++ {
			err := DB.Transaction(func(tx *gorm.DB) error {
				_, _, err := ReverseCommission(tx, "order-twice")
				return err
			})
			if err != nil {
				t.Fatalf("第 %d 次冲正报错: %v", i+1, err)
			}
		}

		var after User
		_ = DB.First(&after, inviter.Id).Error
		// 4000000 + 9000000 - 4000000 = 9000000
		if after.Quota != 9000000 {
			t.Errorf("Quota = %d, want 9000000（只该扣一次）", after.Quota)
		}
	})

	t.Run("记录不存在时按无返现处理", func(t *testing.T) {
		setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
		err := DB.Transaction(func(tx *gorm.DB) error {
			inviterId, reversed, err := ReverseCommission(tx, "never-existed")
			if inviterId != 0 || reversed != 0 {
				t.Errorf("got (%d, %d), want (0, 0)", inviterId, reversed)
			}
			return err
		})
		if err != nil {
			t.Errorf("记录不存在不该报错: %v", err)
		}
	})

	t.Run("空sourceNo直接返回", func(t *testing.T) {
		setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
		err := DB.Transaction(func(tx *gorm.DB) error {
			_, _, err := ReverseCommission(tx, "")
			return err
		})
		if err != nil {
			t.Errorf("空 sourceNo 不该报错: %v", err)
		}
	})
}
