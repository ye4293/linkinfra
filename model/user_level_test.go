package model

import (
	"testing"
)

// seedLevels 建立 Lv1~Lv5 的门槛配置，sort_order 与等级顺序一致。
func seedLevels(t *testing.T) {
	t.Helper()
	rows := []GroupConfig{
		{GroupKey: "Lv1", DisplayName: "Lv1", Discount: 1.00, SortOrder: 0, UpgradeThreshold: 0},
		{GroupKey: "Lv2", DisplayName: "Lv2", Discount: 0.95, SortOrder: 1, UpgradeThreshold: 2500000},
		{GroupKey: "Lv3", DisplayName: "Lv3", Discount: 0.90, SortOrder: 2, UpgradeThreshold: 25000000},
		{GroupKey: "Lv4", DisplayName: "Lv4", Discount: 0.85, SortOrder: 3, UpgradeThreshold: 50000000},
		{GroupKey: "Lv5", DisplayName: "Lv5", Discount: 0.80, SortOrder: 4, UpgradeThreshold: 125000000},
	}
	for i := range rows {
		if err := DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed %s failed: %v", rows[i].GroupKey, err)
		}
	}
}

// mkUser 造一个指定分组与累计充值的用户。
func mkUser(t *testing.T, name, group string, topupQuota int64) *User {
	t.Helper()
	u := &User{
		Username: name, Group: group, TopupQuota: topupQuota,
		AffCode: name, AccessToken: "t-" + name,
	}
	if err := DB.Create(u).Error; err != nil {
		t.Fatalf("create user %s failed: %v", name, err)
	}
	return u
}

func TestRecalcUserLevel(t *testing.T) {
	tests := []struct {
		name        string
		group       string
		topupQuota  int64
		wantGroup   string
		wantChanged bool
	}{
		{"零充值留在Lv1", "Lv1", 0, "Lv1", false},
		{"刚好够Lv2门槛", "Lv1", 2500000, "Lv2", true},
		{"差1个quota不够Lv2", "Lv1", 2499999, "Lv1", false},
		// Bug 3 的核心回归：一次充 $300 必须从 Lv1 直达 Lv5，
		// 而不是因为「超过下一级门槛」而卡在原地
		{"一次充300美元跨级直达Lv5", "Lv1", 150000000, "Lv5", true},
		{"跨级到Lv4", "Lv1", 60000000, "Lv4", true},
		{"已是Lv5不再变", "Lv5", 150000000, "Lv5", false},
		// 只升不降：手工调高过等级的用户不能被打回去
		{"只升不降", "Lv5", 0, "Lv5", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupTestDB(t, &User{}, &GroupConfig{}, &Log{})
			seedLevels(t)
			u := mkUser(t, "u1", tt.group, tt.topupQuota)

			changed, err := RecalcUserLevel(u.Id)
			if err != nil {
				t.Fatalf("RecalcUserLevel failed: %v", err)
			}
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
			}

			var after User
			if err := DB.First(&after, u.Id).Error; err != nil {
				t.Fatalf("read back failed: %v", err)
			}
			if after.Group != tt.wantGroup {
				t.Errorf("Group = %q, want %q", after.Group, tt.wantGroup)
			}
		})
	}
}

// TestRecalcUserLevelThresholdTie 门槛并列时取 sort_order 较小者（较低等级）。
// 这保证运营新增分组忘记设门槛（默认 0）时，用户落到 Lv1 而不是被拉到新分组。
func TestRecalcUserLevelThresholdTie(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &Log{})

	rows := []GroupConfig{
		{GroupKey: "Lv1", DisplayName: "Lv1", Discount: 1, SortOrder: 0, UpgradeThreshold: 0},
		// 运营新加的分组，忘了设门槛
		{GroupKey: "VIP", DisplayName: "VIP", Discount: 0.5, SortOrder: 9, UpgradeThreshold: 0},
	}
	for i := range rows {
		if err := DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed failed: %v", err)
		}
	}

	u := mkUser(t, "u1", "Lv1", 0)
	if _, err := RecalcUserLevel(u.Id); err != nil {
		t.Fatalf("RecalcUserLevel failed: %v", err)
	}

	var after User
	if err := DB.First(&after, u.Id).Error; err != nil {
		t.Fatalf("read back failed: %v", err)
	}
	if after.Group != "Lv1" {
		t.Errorf("Group = %q, want Lv1（并列时不能被拉到 VIP）", after.Group)
	}
}

// TestRecalcUserLevelEdgeCases 分组表为空、用户不存在、当前分组不在表中。
func TestRecalcUserLevelEdgeCases(t *testing.T) {
	t.Run("分组表为空时不报错也不改动", func(t *testing.T) {
		setupTestDB(t, &User{}, &GroupConfig{}, &Log{})
		u := mkUser(t, "u1", "Lv3", 999999999)

		changed, err := RecalcUserLevel(u.Id)
		if err != nil {
			t.Fatalf("空分组表不应报错，got: %v", err)
		}
		if changed {
			t.Error("changed = true，空分组表不该改动等级")
		}

		var after User
		_ = DB.First(&after, u.Id).Error
		if after.Group != "Lv3" {
			t.Errorf("Group = %q, want Lv3（不该被改）", after.Group)
		}
	})

	t.Run("用户不存在返回错误", func(t *testing.T) {
		setupTestDB(t, &User{}, &GroupConfig{}, &Log{})
		seedLevels(t)
		if _, err := RecalcUserLevel(999999); err == nil {
			t.Error("用户不存在时应返回错误")
		}
	})

	t.Run("非法userId直接返回", func(t *testing.T) {
		setupTestDB(t, &User{}, &GroupConfig{}, &Log{})
		if changed, err := RecalcUserLevel(0); err != nil || changed {
			t.Errorf("userId=0 应返回 (false, nil)，got (%v, %v)", changed, err)
		}
	})

	t.Run("当前分组不在表中视为门槛0允许升级", func(t *testing.T) {
		setupTestDB(t, &User{}, &GroupConfig{}, &Log{})
		seedLevels(t)
		// 手工改过 DB 留下的野分组
		u := mkUser(t, "u1", "LegacyGroup", 60000000)

		changed, err := RecalcUserLevel(u.Id)
		if err != nil {
			t.Fatalf("RecalcUserLevel failed: %v", err)
		}
		if !changed {
			t.Error("changed = false，野分组应被当作门槛 0 从而允许升级")
		}

		var after User
		_ = DB.First(&after, u.Id).Error
		if after.Group != "Lv4" {
			t.Errorf("Group = %q, want Lv4", after.Group)
		}
	})
}
