package model

import (
	"testing"
)

// TestInitGroupConfigsDeterministic 验证 InitGroupConfigs 的 sort_order 是确定的。
// 原实现用 for range map 分配 sort_order，Go map 迭代顺序随机，
// 导致每次全新部署的等级排序都不同。P3 的等级判定要依赖 sort_order。
func TestInitGroupConfigsDeterministic(t *testing.T) {
	// 连续初始化两次独立的库，sort_order 分配必须完全一致
	first := runInitGroupConfigs(t)
	second := runInitGroupConfigs(t)

	if len(first) == 0 {
		t.Fatal("InitGroupConfigs produced no rows")
	}
	for key, order := range first {
		if second[key] != order {
			t.Errorf("group %q sort_order not deterministic: %d vs %d",
				key, order, second[key])
		}
	}
}

// runInitGroupConfigs 在一个全新的库上跑一次 InitGroupConfigs，返回 key -> sort_order。
func runInitGroupConfigs(t *testing.T) map[string]int {
	t.Helper()
	db := setupTestDB(t, &GroupConfig{})
	if err := InitGroupConfigs(db); err != nil {
		t.Fatalf("InitGroupConfigs failed: %v", err)
	}
	var configs []GroupConfig
	if err := db.Find(&configs).Error; err != nil {
		t.Fatalf("query failed: %v", err)
	}
	result := make(map[string]int, len(configs))
	for _, c := range configs {
		result[c.GroupKey] = c.SortOrder
	}
	return result
}

// TestInitGroupConfigsDefaults 验证默认门槛与返现比例。
// commission_rate 必须默认为 0（返现全局关闭，安全默认）；
// upgrade_threshold 必须沿用原 controller/stripeCharge.go 的硬编码值。
func TestInitGroupConfigsDefaults(t *testing.T) {
	db := setupTestDB(t, &GroupConfig{})
	if err := InitGroupConfigs(db); err != nil {
		t.Fatalf("InitGroupConfigs failed: %v", err)
	}

	want := map[string]int64{
		"Lv1": 0,
		"Lv2": 2500000,   // $5
		"Lv3": 25000000,  // $50
		"Lv4": 50000000,  // $100
		"Lv5": 125000000, // $250
		"Lv6": 250000000, // $500
	}

	for key, wantThreshold := range want {
		var c GroupConfig
		if err := db.Where("group_key = ?", key).First(&c).Error; err != nil {
			t.Errorf("group %q not created: %v", key, err)
			continue
		}
		if c.UpgradeThreshold != wantThreshold {
			t.Errorf("group %q UpgradeThreshold = %d, want %d",
				key, c.UpgradeThreshold, wantThreshold)
		}
		if c.CommissionRate != 0 {
			t.Errorf("group %q CommissionRate = %v, want 0 (返现默认关闭)",
				key, c.CommissionRate)
		}
	}
}

// TestGetGroupConfigsByThresholdDesc 验证按门槛降序查询，
// 门槛并列时按 sort_order 升序——P3 的 RecalcUserLevel 依赖这个顺序。
func TestGetGroupConfigsByThresholdDesc(t *testing.T) {
	db := setupTestDB(t, &GroupConfig{})

	rows := []GroupConfig{
		{GroupKey: "A", DisplayName: "A", Discount: 1, UpgradeThreshold: 100, SortOrder: 3},
		{GroupKey: "B", DisplayName: "B", Discount: 1, UpgradeThreshold: 500, SortOrder: 1},
		{GroupKey: "C", DisplayName: "C", Discount: 1, UpgradeThreshold: 100, SortOrder: 2},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("create %s failed: %v", rows[i].GroupKey, err)
		}
	}

	got, err := GetGroupConfigsByThresholdDesc()
	if err != nil {
		t.Fatalf("GetGroupConfigsByThresholdDesc failed: %v", err)
	}

	// 期望：B(500) 最前；A 与 C 门槛并列 100，按 sort_order 升序 → C(2) 先于 A(3)
	wantOrder := []string{"B", "C", "A"}
	if len(got) != len(wantOrder) {
		t.Fatalf("got %d rows, want %d", len(got), len(wantOrder))
	}
	for i, key := range wantOrder {
		if got[i].GroupKey != key {
			t.Errorf("position %d = %q, want %q", i, got[i].GroupKey, key)
		}
	}
}
