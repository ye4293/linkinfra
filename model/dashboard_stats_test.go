package model

import (
	"sync"
	"testing"
	"time"
)

func resetDashboardStatsCache() {
	dashboardStatsCache = sync.Map{}
	dashboardStatsLocks = sync.Map{}
}

func TestDashboard24hStatsScopesUserData(t *testing.T) {
	setupTestDB(t, &Log{}, &TopUp{})
	resetDashboardStatsCache()
	t.Cleanup(resetDashboardStatsCache)

	now := time.Now().UTC()
	logs := []Log{
		{UserId: 1, Username: "alice", CreatedAt: now.Add(-25 * time.Hour).Unix(), ModelName: "old-model", Quota: 5000},
		{UserId: 1, Username: "alice", CreatedAt: now.Add(-time.Hour).Unix(), ModelName: "model-a", Quota: 100, PromptTokens: 10, CompletionTokens: 20},
		{UserId: 2, Username: "bob", CreatedAt: now.Add(-time.Hour).Unix(), ModelName: "model-b", Quota: 900, PromptTokens: 30, CompletionTokens: 40},
	}
	if err := DB.Create(&logs).Error; err != nil {
		t.Fatalf("create logs: %v", err)
	}

	stats, err := GetDashboard24hStats(1)
	if err != nil {
		t.Fatalf("GetDashboard24hStats: %v", err)
	}
	if len(stats.Hourly) != 24 {
		t.Fatalf("hourly slots = %d, want 24", len(stats.Hourly))
	}
	if len(stats.ModelStats) != 1 || stats.ModelStats[0].ModelName != "model-a" || stats.ModelStats[0].QuotaSum != 100 {
		t.Fatalf("unexpected user model stats: %+v", stats.ModelStats)
	}
	if len(stats.UserStats) != 0 || stats.RechargeAmount != 0 {
		t.Fatalf("user response leaked admin data: users=%+v recharge=%v", stats.UserStats, stats.RechargeAmount)
	}

	var consumption, tokens, times int64
	for _, item := range stats.Hourly {
		consumption += item.Consumption
		tokens += item.Tokens
		times += item.Times
	}
	if consumption != 100 || tokens != 30 || times != 1 {
		t.Fatalf("unexpected hourly totals: consumption=%d tokens=%d times=%d", consumption, tokens, times)
	}
}

func TestDashboard24hAdminStatsExcludeManualAndNonUSDTopups(t *testing.T) {
	setupTestDB(t, &Log{}, &TopUp{})
	resetDashboardStatsCache()
	t.Cleanup(resetDashboardStatsCache)

	now := time.Now().UTC().Unix()
	logs := []Log{
		{UserId: 1, Username: "alice", CreatedAt: now - 60, ModelName: "model-a", Quota: 100},
		{UserId: 2, Username: "bob", CreatedAt: now - 60, ModelName: "model-b", Quota: 900},
	}
	topups := []TopUp{
		{UserId: 1, TradeNo: "usd", Money: 10, Currency: "USD", CompleteTime: now - 60, Status: "success"},
		{UserId: 1, TradeNo: "legacy", Money: 20, Currency: "", CompleteTime: now - 60, Status: "success"},
		{UserId: 1, TradeNo: "cny", Money: 30, Currency: "CNY", CompleteTime: now - 60, Status: "success"},
		{UserId: 1, TradeNo: "manual", Money: 40, Currency: "USD", CompleteTime: now - 60, Status: "success", Other: `{"source":"manual_complete"}`},
		{UserId: 1, TradeNo: "pending", Money: 50, Currency: "USD", CompleteTime: now - 60, Status: "pending"},
	}
	if err := DB.Create(&logs).Error; err != nil {
		t.Fatalf("create logs: %v", err)
	}
	if err := DB.Create(&topups).Error; err != nil {
		t.Fatalf("create topups: %v", err)
	}

	stats, err := GetDashboard24hStats(0)
	if err != nil {
		t.Fatalf("GetDashboard24hStats: %v", err)
	}
	if stats.RechargeAmount != 30 {
		t.Fatalf("recharge amount = %v, want 30", stats.RechargeAmount)
	}
	if len(stats.UserStats) != 2 || stats.UserStats[0].Username != "bob" || stats.UserStats[0].QuotaSum != 900 {
		t.Fatalf("unexpected admin user stats: %+v", stats.UserStats)
	}
	if len(stats.ModelStats) != 2 || stats.ModelStats[0].ModelName != "model-b" {
		t.Fatalf("unexpected admin model stats: %+v", stats.ModelStats)
	}
}
