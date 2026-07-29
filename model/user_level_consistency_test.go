package model

import (
	"testing"
)

// TestPreviewMatchesRecalc 预览的判定必须与真实重算完全一致。
//
// PreviewLevelRecalc 存在的唯一意义，是让运营在改动发生前看到准确的影响面。
// 它和 RecalcUserLevel 是两份独立实现的判定逻辑 —— 一旦漂移，预览就会给出
// 错误的结论，而这种错误极难被发现（运营照着预览做了决策，实际结果不同）。
//
// 这条测试用同一批数据分别跑两者，逐用户比对结论。
func TestPreviewMatchesRecalc(t *testing.T) {
	// 覆盖各种边界：跨级、刚好卡门槛、只升不降、野分组、零充值
	scenarios := []struct {
		name  string
		group string
		topup int64
	}{
		{"零充值", "Lv1", 0},
		{"刚好Lv2门槛", "Lv1", 2500000},
		{"差1不够Lv2", "Lv1", 2499999},
		{"跨级到Lv5", "Lv1", 150000000},
		{"跨级到Lv4", "Lv1", 60000000},
		{"已是Lv5", "Lv5", 150000000},
		{"只升不降", "Lv5", 0},
		{"野分组", "LegacyGroup", 60000000},
		{"野分组零充值", "LegacyGroup", 0},
		{"中间等级再升", "Lv2", 60000000},
		{"中间等级不动", "Lv3", 26000000},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			// —— 第一遍：跑 PreviewLevelRecalc，记录它的预测 ——
			setupTestDB(t, &User{}, &GroupConfig{}, &Log{})
			seedLevels(t)
			u := mkUser(t, "u1", sc.group, sc.topup)

			preview, err := PreviewLevelRecalc()
			if err != nil {
				t.Fatalf("PreviewLevelRecalc failed: %v", err)
			}
			predictedChange := false
			predictedTo := sc.group
			for _, s := range preview.Samples {
				if s.UserId == u.Id {
					predictedChange = true
					predictedTo = s.ToGroup
				}
			}

			// —— 第二遍：同样的数据跑真实重算 ——
			actualChanged, err := RecalcUserLevel(u.Id)
			if err != nil {
				t.Fatalf("RecalcUserLevel failed: %v", err)
			}
			var after User
			if err := DB.First(&after, u.Id).Error; err != nil {
				t.Fatalf("read back failed: %v", err)
			}

			// —— 比对 ——
			if predictedChange != actualChanged {
				t.Errorf("预览说 changed=%v，实际 changed=%v —— 两份判定逻辑已漂移",
					predictedChange, actualChanged)
			}
			if predictedTo != after.Group {
				t.Errorf("预览说会变成 %q，实际变成 %q —— 两份判定逻辑已漂移",
					predictedTo, after.Group)
			}
			// ChangedUsers 计数也要对得上
			wantCount := 0
			if actualChanged {
				wantCount = 1
			}
			if preview.ChangedUsers != wantCount {
				t.Errorf("preview.ChangedUsers = %d, want %d", preview.ChangedUsers, wantCount)
			}
		})
	}
}

// TestPreviewMatchesRecalcMultiUser 多用户混合场景下的一致性。
func TestPreviewMatchesRecalcMultiUser(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &Log{})
	seedLevels(t)

	users := []struct {
		name  string
		group string
		topup int64
	}{
		{"a", "Lv1", 150000000},
		{"b", "Lv1", 2500000},
		{"c", "Lv2", 60000000},
		{"d", "Lv1", 0},
		{"e", "Lv5", 150000000},
		{"f", "LegacyGroup", 60000000},
	}
	ids := map[string]int{}
	for _, x := range users {
		u := mkUser(t, x.name, x.group, x.topup)
		ids[x.name] = u.Id
	}

	preview, err := PreviewLevelRecalc()
	if err != nil {
		t.Fatalf("PreviewLevelRecalc failed: %v", err)
	}
	predicted := map[int]string{}
	for _, s := range preview.Samples {
		predicted[s.UserId] = s.ToGroup
	}

	actualChanged := 0
	for _, x := range users {
		id := ids[x.name]
		changed, err := RecalcUserLevel(id)
		if err != nil {
			t.Fatalf("RecalcUserLevel(%s) failed: %v", x.name, err)
		}
		var after User
		_ = DB.First(&after, id).Error

		if changed {
			actualChanged++
			if predicted[id] != after.Group {
				t.Errorf("用户 %s：预览说 %q，实际 %q", x.name, predicted[id], after.Group)
			}
		} else if _, inPreview := predicted[id]; inPreview {
			t.Errorf("用户 %s：预览说会变成 %q，实际没变（仍是 %q）",
				x.name, predicted[id], after.Group)
		}
	}

	if preview.ChangedUsers != actualChanged {
		t.Errorf("preview.ChangedUsers = %d, 实际变更 %d 个", preview.ChangedUsers, actualChanged)
	}
	if preview.TotalUsers != len(users) {
		t.Errorf("preview.TotalUsers = %d, want %d", preview.TotalUsers, len(users))
	}
}
