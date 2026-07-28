package model

import (
	"testing"
)

func TestPreviewLevelRecalc(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &Log{})
	seedLevels(t)

	// 会升级的
	mkUser(t, "up1", "Lv1", 150000000) // → Lv5
	mkUser(t, "up2", "Lv1", 2500000)   // → Lv2
	mkUser(t, "up3", "Lv2", 60000000)  // → Lv4
	// 不会变的
	mkUser(t, "same1", "Lv1", 0)
	mkUser(t, "same2", "Lv5", 150000000)
	// 只升不降：等级高于累计充值应得的，不动
	mkUser(t, "keep", "Lv5", 0)

	preview, err := PreviewLevelRecalc()
	if err != nil {
		t.Fatalf("PreviewLevelRecalc failed: %v", err)
	}

	if preview.TotalUsers != 6 {
		t.Errorf("TotalUsers = %d, want 6", preview.TotalUsers)
	}
	if preview.ChangedUsers != 3 {
		t.Errorf("ChangedUsers = %d, want 3", preview.ChangedUsers)
	}

	wantTransitions := map[string]int{
		"Lv1 -> Lv5": 1,
		"Lv1 -> Lv2": 1,
		"Lv2 -> Lv4": 1,
	}
	for k, v := range wantTransitions {
		if preview.Transitions[k] != v {
			t.Errorf("Transitions[%q] = %d, want %d", k, preview.Transitions[k], v)
		}
	}
	if len(preview.Transitions) != len(wantTransitions) {
		t.Errorf("Transitions 有意外条目: %+v", preview.Transitions)
	}

	// 样本里不能出现不变的用户
	for _, s := range preview.Samples {
		if s.FromGroup == s.ToGroup {
			t.Errorf("样本含未变化用户: %+v", s)
		}
	}
}

// TestPreviewLevelRecalcIsReadOnly 预览绝不能改数据 —— 它存在的意义
// 就是让运营在改动发生前看到影响面。
func TestPreviewLevelRecalcIsReadOnly(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &Log{})
	seedLevels(t)
	u := mkUser(t, "u1", "Lv1", 150000000)

	if _, err := PreviewLevelRecalc(); err != nil {
		t.Fatalf("PreviewLevelRecalc failed: %v", err)
	}

	var after User
	if err := DB.First(&after, u.Id).Error; err != nil {
		t.Fatalf("read back failed: %v", err)
	}
	if after.Group != "Lv1" {
		t.Errorf("Group = %q, want Lv1（预览不得写库）", after.Group)
	}

	// 也不能写日志
	var logCount int64
	DB.Model(&Log{}).Count(&logCount)
	if logCount != 0 {
		t.Errorf("日志条数 = %d, want 0（预览不得写日志）", logCount)
	}
}

func TestPreviewLevelRecalcEmptyGroupConfigs(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &Log{})
	mkUser(t, "u1", "Lv1", 150000000)

	preview, err := PreviewLevelRecalc()
	if err != nil {
		t.Fatalf("分组表为空不应报错: %v", err)
	}
	if preview.ChangedUsers != 0 {
		t.Errorf("ChangedUsers = %d, want 0", preview.ChangedUsers)
	}
}
