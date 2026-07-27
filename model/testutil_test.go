package model

import (
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// setupTestDB 建立一个隔离的 in-memory sqlite 库并替换全局 DB / LOG_DB。
// 测试结束后自动还原，供 model 包内所有需要 DB 的测试复用。
func setupTestDB(t *testing.T, models ...interface{}) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to get underlying sql.DB: %v", err)
	}
	// ":memory:" 下每个连接是一个独立的库。限制为单连接，
	// 否则同一个测试里的两次查询可能落在不同的空库上。
	sqlDB.SetMaxOpenConns(1)

	if len(models) > 0 {
		if err := db.AutoMigrate(models...); err != nil {
			t.Fatalf("failed to migrate test models: %v", err)
		}
	}

	origDB, origLogDB := DB, LOG_DB
	DB, LOG_DB = db, db
	t.Cleanup(func() {
		DB, LOG_DB = origDB, origLogDB
		_ = sqlDB.Close()
	})

	return db
}

// TestSetupTestDB 自检：基座能建表、能写读、单连接可见性正确。
func TestSetupTestDB(t *testing.T) {
	db := setupTestDB(t, &GroupConfig{})

	if err := db.Create(&GroupConfig{GroupKey: "Lv1", DisplayName: "Lv1", Discount: 1.0}).Error; err != nil {
		t.Fatalf("create failed: %v", err)
	}

	var got GroupConfig
	if err := DB.Where("group_key = ?", "Lv1").First(&got).Error; err != nil {
		t.Fatalf("read back through global DB failed: %v", err)
	}
	if got.DisplayName != "Lv1" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Lv1")
	}
}
