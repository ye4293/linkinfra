package model

import (
	"testing"

	"github.com/songquanpeng/one-api/common"
)

// TestLikeOp 只有 PG 需要 ILIKE。
//
// MySQL 默认排序规则 utf8mb4_general_ci 与 sqlite 的 LIKE 对 ASCII 本就
// 大小写不敏感；PG 的 LIKE 严格区分大小写，必须用 ILIKE，否则运营在 PG 上
// 搜 "GPT-4" 什么都搜不到，而同样的关键词在 MySQL 上能搜到 —— 迁库后才
// 暴露且不报错的行为回退。
func TestLikeOp(t *testing.T) {
	orig := common.UsingPostgreSQL
	t.Cleanup(func() { common.UsingPostgreSQL = orig })

	common.UsingPostgreSQL = false
	if got := likeOp(); got != "LIKE" {
		t.Errorf("非 PG 下 likeOp() = %q, want LIKE", got)
	}

	common.UsingPostgreSQL = true
	if got := likeOp(); got != "ILIKE" {
		t.Errorf("PG 下 likeOp() = %q, want ILIKE", got)
	}
}

// TestFindEnabledModelsByGroup 覆盖去重与排序。
//
// 原实现是 SELECT DISTINCT model ... ORDER BY priority DESC，在 PG 上报
// "for SELECT DISTINCT, ORDER BY expressions must appear in select list"，
// 而 MySQL/sqlite 宽容通过 —— 只在 PG 上才炸。改为 GROUP BY model +
// ORDER BY MAX(priority) 后三库都合法。
func TestFindEnabledModelsByGroup(t *testing.T) {
	setupTestDB(t, &Ability{})

	p := func(v int64) *int64 { return &v }
	rows := []Ability{
		// 同一个 model 在两个 channel 上，priority 不同 —— 必须去重成一条
		{Group: "Lv1", Model: "gpt-4", ChannelId: 1, Enabled: true, Priority: p(10)},
		{Group: "Lv1", Model: "gpt-4", ChannelId: 2, Enabled: true, Priority: p(50)},
		{Group: "Lv1", Model: "claude", ChannelId: 3, Enabled: true, Priority: p(30)},
		// 未启用的不应出现
		{Group: "Lv1", Model: "disabled-model", ChannelId: 4, Enabled: false, Priority: p(99)},
		// 别的分组不应串进来
		{Group: "Lv2", Model: "other-group-model", ChannelId: 5, Enabled: true, Priority: p(99)},
	}
	for i := range rows {
		if err := DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("create ability failed: %v", err)
		}
	}

	models, err := FindEnabledModelsByGroup("Lv1")
	if err != nil {
		t.Fatalf("FindEnabledModelsByGroup failed: %v", err)
	}

	// 去重后应只有 gpt-4 与 claude；按各自最高 priority 降序：
	// gpt-4 的 MAX(priority)=50 > claude 的 30
	want := []string{"gpt-4", "claude"}
	if len(models) != len(want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
	for i, m := range want {
		if models[i] != m {
			t.Errorf("位置 %d = %q, want %q（应按 MAX(priority) 降序）", i, models[i], m)
		}
	}
}
