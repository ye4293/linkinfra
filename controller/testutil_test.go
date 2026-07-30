package controller

import (
	"testing"

	"github.com/songquanpeng/one-api/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// setupTestDB 建立一个隔离的 in-memory sqlite 库并替换 model 包的全局
// DB / LOG_DB，测试结束后自动还原。仿 model/testutil_test.go —— 那份
// 在 model 包内，_test.go 不跨包，所以 controller 包要自己有一份。
func setupTestDB(t *testing.T) *gorm.DB {
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
	// ":memory:" 下每个连接是一个独立的库，必须限制为单连接，
	// 否则同一个测试里的两次查询可能落在不同的空库上。
	sqlDB.SetMaxOpenConns(1)

	// Log 表是必需的：model.Insert 在 QuotaForNewUser > 0 时会 RecordLog。
	if err := db.AutoMigrate(&model.User{}, &model.Log{}); err != nil {
		t.Fatalf("failed to migrate test models: %v", err)
	}

	origDB, origLogDB := model.DB, model.LOG_DB
	model.DB, model.LOG_DB = db, db
	t.Cleanup(func() {
		model.DB, model.LOG_DB = origDB, origLogDB
		_ = sqlDB.Close()
	})

	return db
}
