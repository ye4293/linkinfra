package model

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestDefaultGroupSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []GroupConfig
		want string
	}{
		{"empty", nil, "Start"},
		{"threshold", []GroupConfig{{GroupKey: "high", UpgradeThreshold: 100, SortOrder: 0}, {GroupKey: "low", UpgradeThreshold: 1, SortOrder: 5}}, "low"},
		{"sort_order", []GroupConfig{{GroupKey: "later", SortOrder: 5}, {GroupKey: "earlier", SortOrder: 1}}, "earlier"},
		{"id", []GroupConfig{{ID: 20, GroupKey: "later"}, {ID: 10, GroupKey: "earlier"}}, "earlier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupTestDB(t, &GroupConfig{})
			for i := range tc.rows {
				if err := db.Create(&tc.rows[i]).Error; err != nil {
					t.Fatal(err)
				}
			}
			if got := GetDefaultGroup(); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUserDefaultGroupInTransaction(t *testing.T) {
	for _, entry := range []string{"Lv1", "Start", ""} {
		t.Run("entry_"+entry, func(t *testing.T) {
			db := setupTestDB(t, &User{})
			// 空名称场景故意不建配置表，验证查询错误也能回退。
			if entry != "" {
				if err := db.AutoMigrate(&GroupConfig{}); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var created User
			err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				// 未提交的配置必须对钩子可见，单连接也不能死锁。
				if entry != "" {
					if err := tx.Create(&GroupConfig{GroupKey: entry}).Error; err != nil {
						return err
					}
				}
				created = User{Username: "default", AffCode: "default", AccessToken: "default"}
				return tx.Create(&created).Error
			})
			if err != nil {
				t.Fatal(err)
			}
			want := entry
			if want == "" {
				want = "Start"
			}
			var stored User
			if err := db.First(&stored, created.Id).Error; err != nil {
				t.Fatal(err)
			}
			if created.Group != want || stored.Group != want {
				t.Fatalf("created=%q stored=%q want=%q", created.Group, stored.Group, want)
			}
			explicit := User{Username: "explicit", Group: "custom", AffCode: "explicit", AccessToken: "explicit"}
			if err := db.Create(&explicit).Error; err != nil {
				t.Fatal(err)
			}
			if explicit.Group != "custom" {
				t.Fatalf("explicit group overwritten: %q", explicit.Group)
			}
		})
	}
}
