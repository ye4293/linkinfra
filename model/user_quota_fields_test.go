package model

import (
	"testing"

	"gorm.io/gorm"
)

// TestUserGiftAndTopupQuotaFields 验证两个累计字段能建表、默认为 0、能原子累加。
func TestUserGiftAndTopupQuotaFields(t *testing.T) {
	setupTestDB(t, &User{})

	u := &User{Username: "alice", AffCode: "aaaa", AccessToken: "t-alice"}
	if err := DB.Create(u).Error; err != nil {
		t.Fatalf("create user failed: %v", err)
	}

	// 默认值必须是 0，而不是 NULL 导致的扫描错误
	var fresh User
	if err := DB.First(&fresh, u.Id).Error; err != nil {
		t.Fatalf("read user failed: %v", err)
	}
	if fresh.GiftQuota != 0 {
		t.Errorf("GiftQuota default = %d, want 0", fresh.GiftQuota)
	}
	if fresh.TopupQuota != 0 {
		t.Errorf("TopupQuota default = %d, want 0", fresh.TopupQuota)
	}

	// 两个字段能在一条 SQL 内原子累加（P3 的返现发放依赖这个用法）
	err := DB.Model(&User{}).Where("id = ?", u.Id).Updates(map[string]interface{}{
		"gift_quota":  gorm.Expr("gift_quota + ?", 4000000),
		"topup_quota": gorm.Expr("topup_quota + ?", 50000000),
	}).Error
	if err != nil {
		t.Fatalf("atomic update failed: %v", err)
	}

	var after User
	if err := DB.First(&after, u.Id).Error; err != nil {
		t.Fatalf("read back failed: %v", err)
	}
	if after.GiftQuota != 4000000 {
		t.Errorf("GiftQuota = %d, want 4000000", after.GiftQuota)
	}
	if after.TopupQuota != 50000000 {
		t.Errorf("TopupQuota = %d, want 50000000", after.TopupQuota)
	}
}
