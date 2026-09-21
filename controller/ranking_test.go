package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRankingPublicSnapshotAndETagWithoutDatabase(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	defer sqlDB.Close()
	require.NoError(t, db.AutoMigrate(&model.RankingSnapshot{}))
	require.NoError(t, db.Create(&model.RankingSnapshot{ID: 1, Version: "ranking-test", Payload: `{"data_through":"2026-09-20"}`}).Error)
	oldDB, oldEnabled, oldConsume := model.LOG_DB, config.RankingsEnabled, config.LogConsumeEnabled
	t.Cleanup(func() {
		model.LOG_DB = oldDB
		config.RankingsEnabled = oldEnabled
		config.LogConsumeEnabled = oldConsume
	})
	model.LOG_DB = db
	require.NoError(t, model.RefreshRankingCache())
	// handler 若访问数据库将 panic；缓存路径必须能独立提供结果。
	model.LOG_DB = nil
	config.RankingsEnabled = true
	config.LogConsumeEnabled = true
	router := gin.New()
	router.GET("/api/rankings", GetRankings)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/rankings", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "2026-09-20")
	require.Equal(t, `"ranking-test"`, w.Header().Get("ETag"))
	req := httptest.NewRequest(http.MethodGet, "/api/rankings", nil)
	req.Header.Set("If-None-Match", w.Header().Get("ETag"))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusNotModified, w.Code)
	require.Empty(t, w.Body.String())
	config.LogConsumeEnabled = false
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/rankings", nil))
	require.Contains(t, w.Body.String(), "paused")
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
}
