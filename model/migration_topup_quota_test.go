package model

import (
	"testing"

	"github.com/songquanpeng/one-api/common/config"
)

func TestBackfillTopupQuota(t *testing.T) {
	setupTestDB(t, &User{}, &Option{}, &TopUp{}, &ChargeOrder{})

	orig := config.QuotaPerUnit
	config.QuotaPerUnit = 500000
	t.Cleanup(func() { config.QuotaPerUnit = orig })

	u1 := &User{Username: "u1", AffCode: "u1", AccessToken: "t1"}
	u2 := &User{Username: "u2", AffCode: "u2", AccessToken: "t2"}
	u3 := &User{Username: "u3", AffCode: "u3", AccessToken: "t3"}
	for _, u := range []*User{u1, u2, u3} {
		if err := DB.Create(u).Error; err != nil {
			t.Fatalf("create user failed: %v", err)
		}
	}

	topups := []TopUp{
		{UserId: u1.Id, Amount: 20, TradeNo: "t-a", Status: "success"},
		{UserId: u1.Id, Amount: 30, TradeNo: "t-b", Status: "success"},
		// pending 不计入
		{UserId: u1.Id, Amount: 999, TradeNo: "t-c", Status: "pending"},
		{UserId: u2.Id, Amount: 10, TradeNo: "t-d", Status: "success"},
	}
	for i := range topups {
		if err := DB.Create(&topups[i]).Error; err != nil {
			t.Fatalf("create topup failed: %v", err)
		}
	}

	orders := []ChargeOrder{
		{UserId: u1.Id, AppOrderId: "o-a", Amount: 5, Status: StatusMap["success"]},
		// 退款订单不计入
		{UserId: u1.Id, AppOrderId: "o-b", Amount: 777, Status: StatusMap["refund"]},
		{UserId: u2.Id, AppOrderId: "o-c", Amount: 1, Status: StatusMap["success"]},
	}
	for i := range orders {
		if err := DB.Create(&orders[i]).Error; err != nil {
			t.Fatalf("create charge order failed: %v", err)
		}
	}

	if err := BackfillTopupQuota(DB); err != nil {
		t.Fatalf("BackfillTopupQuota failed: %v", err)
	}

	want := map[int]int64{
		u1.Id: (20 + 30 + 5) * 500000, // 27500000
		u2.Id: (10 + 1) * 500000,      // 5500000
		u3.Id: 0,                      // 无任何充值
	}
	for id, wantQuota := range want {
		var u User
		if err := DB.First(&u, id).Error; err != nil {
			t.Fatalf("read user %d failed: %v", id, err)
		}
		if u.TopupQuota != wantQuota {
			t.Errorf("user %d TopupQuota = %d, want %d", id, u.TopupQuota, wantQuota)
		}
	}
}

// TestBackfillTopupQuotaIdempotent 回填只能跑一次，重复调用不能翻倍。
func TestBackfillTopupQuotaIdempotent(t *testing.T) {
	setupTestDB(t, &User{}, &Option{}, &TopUp{}, &ChargeOrder{})

	orig := config.QuotaPerUnit
	config.QuotaPerUnit = 500000
	t.Cleanup(func() { config.QuotaPerUnit = orig })

	u := &User{Username: "u1", AffCode: "u1", AccessToken: "t1"}
	if err := DB.Create(u).Error; err != nil {
		t.Fatalf("create user failed: %v", err)
	}
	if err := DB.Create(&TopUp{UserId: u.Id, Amount: 20, TradeNo: "t-a", Status: "success"}).Error; err != nil {
		t.Fatalf("create topup failed: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := BackfillTopupQuota(DB); err != nil {
			t.Fatalf("第 %d 次回填报错: %v", i+1, err)
		}
	}

	var after User
	_ = DB.First(&after, u.Id).Error
	if after.TopupQuota != 10000000 {
		t.Errorf("TopupQuota = %d, want 10000000（重复回填不该累加）", after.TopupQuota)
	}

	var opt Option
	if err := DB.Where("key = ?", migratedTopupQuotaOptionKey).First(&opt).Error; err != nil {
		t.Errorf("标记位未写入 options 表: %v", err)
	}
}

// TestBackfillTopupQuotaMissingChargeOrders charge_orders 表不在
// AutoMigrate 清单里（model/main.go 只迁移了 Order），全新部署上不存在。
// 回填必须容忍，否则启动迁移直接失败。
func TestBackfillTopupQuotaMissingChargeOrders(t *testing.T) {
	// 刻意不迁移 ChargeOrder
	setupTestDB(t, &User{}, &Option{}, &TopUp{})

	orig := config.QuotaPerUnit
	config.QuotaPerUnit = 500000
	t.Cleanup(func() { config.QuotaPerUnit = orig })

	u := &User{Username: "u1", AffCode: "u1", AccessToken: "t1"}
	if err := DB.Create(u).Error; err != nil {
		t.Fatalf("create user failed: %v", err)
	}
	if err := DB.Create(&TopUp{UserId: u.Id, Amount: 20, TradeNo: "t-a", Status: "success"}).Error; err != nil {
		t.Fatalf("create topup failed: %v", err)
	}

	if err := BackfillTopupQuota(DB); err != nil {
		t.Fatalf("charge_orders 表缺失时不应报错: %v", err)
	}

	var after User
	_ = DB.First(&after, u.Id).Error
	if after.TopupQuota != 10000000 {
		t.Errorf("TopupQuota = %d, want 10000000（topups 部分仍应回填）", after.TopupQuota)
	}
}
