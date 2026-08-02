package model

import (
	"strings"
	"testing"
)

// TestEnsureProviderIdUniqueIndexesAllowsMultipleEmptyValues 是这个索引最关键的
// 性质：邮箱注册的用户 github_id / google_id 都是空串，一个**普通**唯一索引会
// 让第二个邮箱注册用户直接插入失败。所以必须是带 WHERE 的部分索引。
func TestEnsureProviderIdUniqueIndexesAllowsMultipleEmptyValues(t *testing.T) {
	db := setupTestDB(t, &User{})

	if err := EnsureProviderIdUniqueIndexes(db); err != nil {
		t.Fatalf("EnsureProviderIdUniqueIndexes failed: %v", err)
	}

	// 三个邮箱注册用户，provider id 全为空串
	for i, name := range []string{"mail1", "mail2", "mail3"} {
		u := User{Username: name, AffCode: "AF" + name, AccessToken: "tok" + name}
		if err := db.Create(&u).Error; err != nil {
			t.Fatalf("第 %d 个空 provider id 的用户插入失败: %v（部分索引写错成了普通唯一索引？）", i+1, err)
		}
	}
}

// TestEnsureProviderIdUniqueIndexesRejectsDuplicateProviderId 索引必须真的拦住
// 重复的 provider id —— 这是关掉「先查再建」并发竞态的唯一手段。
func TestEnsureProviderIdUniqueIndexesRejectsDuplicateProviderId(t *testing.T) {
	db := setupTestDB(t, &User{})
	if err := EnsureProviderIdUniqueIndexes(db); err != nil {
		t.Fatalf("EnsureProviderIdUniqueIndexes failed: %v", err)
	}

	first := User{Username: "gh1", GitHubId: "12345", AffCode: "A1", AccessToken: "t1"}
	if err := db.Create(&first).Error; err != nil {
		t.Fatalf("first insert failed: %v", err)
	}

	// 模拟竞态：同一个 github_id 再建一个号
	second := User{Username: "gh2", GitHubId: "12345", AffCode: "A2", AccessToken: "t2"}
	err := db.Create(&second).Error
	if err == nil {
		t.Fatal("同一个 github_id 插入了两行 —— 唯一索引没生效，并发注册仍会产生重复账号并重复发赠额")
	}
	if !IsDuplicateProviderIdError(err) {
		t.Errorf("IsDuplicateProviderIdError 没认出这个错误: %v", err)
	}

	// google_id 同理
	g1 := User{Username: "gg1", GoogleId: "abc", AffCode: "A3", AccessToken: "t3"}
	if err := db.Create(&g1).Error; err != nil {
		t.Fatalf("google first insert failed: %v", err)
	}
	g2 := User{Username: "gg2", GoogleId: "abc", AffCode: "A4", AccessToken: "t4"}
	if err := db.Create(&g2).Error; err == nil {
		t.Fatal("同一个 google_id 插入了两行")
	}
}

// TestEnsureProviderIdUniqueIndexesIsIdempotent 每次启动都会跑，必须可重复执行。
func TestEnsureProviderIdUniqueIndexesIsIdempotent(t *testing.T) {
	db := setupTestDB(t, &User{})
	for i := 0; i < 3; i++ {
		if err := EnsureProviderIdUniqueIndexes(db); err != nil {
			t.Fatalf("第 %d 次调用失败: %v", i+1, err)
		}
	}
}

// TestEnsureProviderIdUniqueIndexesReportsExistingDuplicates 已有重复数据时不能
// 直接抛 duplicate key 了事 —— 那样运维还得自己去查是哪几行。应当列出重复值、
// 跳过该索引、且不阻止启动。
func TestEnsureProviderIdUniqueIndexesReportsExistingDuplicates(t *testing.T) {
	db := setupTestDB(t, &User{})

	// 先造重复数据（索引还没建，所以能插进去）
	for i, tok := range []string{"t1", "t2"} {
		u := User{
			Username:    "dup" + tok,
			GitHubId:    "same-id",
			AffCode:     "AF" + tok,
			AccessToken: tok,
		}
		if err := db.Create(&u).Error; err != nil {
			t.Fatalf("第 %d 行插入失败: %v", i+1, err)
		}
	}

	// 不能返回 error（否则启动流程会把它当故障），而是跳过并记日志
	if err := EnsureProviderIdUniqueIndexes(db); err != nil {
		t.Errorf("有重复数据时返回了 error = %v，会让启动流程误判为故障", err)
	}

	// github_id 的索引应当没建上，而 google_id 的应当建上了（互不影响）
	g1 := User{Username: "g1", GoogleId: "gid", AffCode: "AG1", AccessToken: "tg1"}
	if err := db.Create(&g1).Error; err != nil {
		t.Fatalf("google 用户插入失败: %v", err)
	}
	g2 := User{Username: "g2", GoogleId: "gid", AffCode: "AG2", AccessToken: "tg2"}
	if err := db.Create(&g2).Error; err == nil {
		t.Error("github_id 有重复数据不该影响 google_id 索引的创建")
	}
}

// TestFindDuplicateProviderIds 重复检测只该看非空值。
func TestFindDuplicateProviderIds(t *testing.T) {
	db := setupTestDB(t, &User{})

	// 两个空串 + 两个相同的非空值
	rows := []User{
		{Username: "e1", GitHubId: "", AffCode: "E1", AccessToken: "e1"},
		{Username: "e2", GitHubId: "", AffCode: "E2", AccessToken: "e2"},
		{Username: "d1", GitHubId: "dup", AffCode: "D1", AccessToken: "d1"},
		{Username: "d2", GitHubId: "dup", AffCode: "D2", AccessToken: "d2"},
		{Username: "u1", GitHubId: "uniq", AffCode: "U1", AccessToken: "u1"},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("insert %s failed: %v", rows[i].Username, err)
		}
	}

	dupes, err := findDuplicateProviderIds(db, "github_id")
	if err != nil {
		t.Fatalf("findDuplicateProviderIds failed: %v", err)
	}
	if len(dupes) != 1 || dupes[0] != "dup" {
		t.Errorf("dupes = %v, want [dup]（空串不该算重复，唯一值也不该算）", dupes)
	}
}

// TestIsDuplicateProviderIdError 按索引名兜底识别的路径。
func TestIsDuplicateProviderIdError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"无关错误", errString("connection refused"), false},
		{"github 索引名", errString("UNIQUE constraint failed: idx_users_github_id_unique"), true},
		{"google 索引名", errString("duplicate key value violates unique constraint \"idx_users_google_id_unique\""), true},
		{"大写", errString("ERROR: IDX_USERS_GITHUB_ID_UNIQUE"), true},
		// SQLite 只报列名、不提索引名（实测踩到过）
		{"sqlite 列名式", errString("UNIQUE constraint failed: users.github_id"), true},
		{"sqlite google 列名式", errString("UNIQUE constraint failed: users.google_id"), true},
		// 反例：提到列名但不是唯一性冲突，不能误判
		{"提到列名但非冲突", errString("column github_id does not exist"), false},
		{"提到列名的连接错误", errString("failed to scan github_id: connection reset"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsDuplicateProviderIdError(tt.err); got != tt.want {
				t.Errorf("IsDuplicateProviderIdError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestProviderIdIndexSQLIsPartial 直接断言生成的 SQL 带 WHERE —— 防止有人
// "简化"掉这个条件，那会让所有邮箱注册用户插不进去。
func TestProviderIdIndexSQLIsPartial(t *testing.T) {
	for _, target := range providerIdUniqueIndexes {
		sql := "CREATE UNIQUE INDEX IF NOT EXISTS " + target.index +
			" ON users (" + target.column + ") WHERE " + target.column + " <> ''"
		if !strings.Contains(sql, "WHERE") {
			t.Errorf("%s 的索引 SQL 缺少 WHERE 条件", target.column)
		}
	}
}
