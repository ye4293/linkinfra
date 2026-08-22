package model

import (
	"sync"
	"testing"
	"time"
)

func resetUsageMetricsCache() {
	usageMetricsCache = sync.Map{}
	usageMetricsLocks = sync.Map{}
}

func TestGetUsageMetricsScopesAndCaches(t *testing.T) {
	db := setupTestDB(t, &Log{})
	resetUsageMetricsCache()
	t.Cleanup(resetUsageMetricsCache)

	now := time.Now().Unix()
	logs := []Log{
		{UserId: 1, CreatedAt: now - 120, PromptTokens: 50, CompletionTokens: 60, Quota: 200},
		{UserId: 1, CreatedAt: now - 30, PromptTokens: 10, CompletionTokens: 20, Quota: 100},
		{UserId: 2, CreatedAt: now - 30, PromptTokens: 30, CompletionTokens: 40, Quota: 900},
	}
	if err := db.Create(&logs).Error; err != nil {
		t.Fatalf("create logs: %v", err)
	}

	userMetrics, err := GetUsageMetrics(1)
	if err != nil {
		t.Fatalf("GetUsageMetrics user: %v", err)
	}
	if userMetrics.RPM != 1 || userMetrics.TPM != 30 || userMetrics.TodaySpend != 300 {
		t.Fatalf("unexpected user metrics: %+v", userMetrics)
	}

	adminMetrics, err := GetUsageMetrics(0)
	if err != nil {
		t.Fatalf("GetUsageMetrics admin: %v", err)
	}
	if adminMetrics.RPM != 2 || adminMetrics.TPM != 100 || adminMetrics.TodaySpend != 1200 {
		t.Fatalf("unexpected admin metrics: %+v", adminMetrics)
	}

	if err := db.Create(&Log{UserId: 1, CreatedAt: now, PromptTokens: 999, Quota: 999}).Error; err != nil {
		t.Fatalf("create cached log: %v", err)
	}
	cached, err := GetUsageMetrics(1)
	if err != nil {
		t.Fatalf("GetUsageMetrics cached: %v", err)
	}
	if cached.RPM != userMetrics.RPM || cached.TodaySpend != userMetrics.TodaySpend {
		t.Fatalf("cache was not reused: before=%+v after=%+v", userMetrics, cached)
	}
}

func TestLogHasUserIdIdIndex(t *testing.T) {
	db := setupTestDB(t, &Log{})
	if !db.Migrator().HasIndex(&Log{}, "idx_logs_user_id_id") {
		t.Fatal("idx_logs_user_id_id was not created")
	}
}
