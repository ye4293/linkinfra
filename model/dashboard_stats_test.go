package model

import (
	"sync"
	"testing"
	"time"

	"github.com/songquanpeng/one-api/common"
	"github.com/stretchr/testify/require"
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

// 来源消耗：打了计费标签的渠道按标签归组（跨渠道类型合并），其余按渠道类型归组，
// 已删除的渠道合并为一项；只对管理员返回。
func TestDashboard24hAdminSourceStatsGroupByBillingTagOrChannelType(t *testing.T) {
	setupTestDB(t, &Log{}, &TopUp{}, &Channel{})
	resetDashboardStatsCache()
	t.Cleanup(resetDashboardStatsCache)

	channels := []Channel{
		{Id: 1, Type: common.ChannelTypeVertexAI, Config: `{"billing_tag":"vertex-gemini"}`},
		{Id: 2, Type: common.ChannelTypeVertexAI, Config: `{"billing_tag":" Vertex-Claude "}`},
		{Id: 3, Type: common.ChannelTypeAnthropic, Config: `{"billing_tag":"vertex-claude","provider":"anthropic"}`},
		{Id: 4, Type: common.ChannelTypeVertexAI},
		{Id: 5, Type: common.ChannelTypeVertexAI, Config: `{"provider":"vertex"}`},
		{Id: 6, Type: common.ChannelTypeOpenAI},
		{Id: 7, Type: common.ChannelTypeAzure},
	}
	require.NoError(t, DB.Create(&channels).Error)

	now := time.Now().UTC().Unix()
	logs := []Log{
		{UserId: 1, Username: "alice", CreatedAt: now - 60, Type: LogTypeConsume, ModelName: "gemini", Quota: 100, ChannelId: 1},
		{UserId: 1, Username: "alice", CreatedAt: now - 60, Type: LogTypeConsume, ModelName: "claude", Quota: 200, ChannelId: 2},
		{UserId: 2, Username: "bob", CreatedAt: now - 60, Type: LogTypeConsume, ModelName: "claude", Quota: 50, ChannelId: 3},
		{UserId: 2, Username: "bob", CreatedAt: now - 60, Type: LogTypeConsume, ModelName: "gemini", Quota: 300, ChannelId: 4},
		{UserId: 2, Username: "bob", CreatedAt: now - 60, Type: LogTypeConsume, ModelName: "gemini", Quota: 25, ChannelId: 5},
		{UserId: 1, Username: "alice", CreatedAt: now - 60, Type: LogTypeConsume, ModelName: "gpt", Quota: 10, ChannelId: 6},
		// 渠道已删除
		{UserId: 1, Username: "alice", CreatedAt: now - 60, Type: LogTypeConsume, ModelName: "gpt", Quota: 40, ChannelId: 99},
		// 没有消耗的来源不出现
		{UserId: 1, Username: "alice", CreatedAt: now - 60, Type: LogTypeError, ModelName: "gpt", Quota: 0, ChannelId: 7},
		// 没有渠道的日志不计入
		{UserId: 1, Username: "alice", CreatedAt: now - 60, Type: LogTypeConsume, ModelName: "gpt", Quota: 999},
		// 超出 24 小时窗口
		{UserId: 1, Username: "alice", CreatedAt: now - 25*3600, Type: LogTypeConsume, ModelName: "gemini", Quota: 5000, ChannelId: 1},
	}
	require.NoError(t, DB.Create(&logs).Error)

	stats, err := GetDashboard24hStats(0)
	require.NoError(t, err)
	require.Equal(t, []DashboardSourceStats{
		{Kind: DashboardSourceChannelType, ChannelType: common.ChannelTypeVertexAI, QuotaSum: 325},
		{Kind: DashboardSourceTag, Tag: "vertex-claude", Name: "vertex-claude", QuotaSum: 250},
		{Kind: DashboardSourceTag, Tag: "vertex-gemini", Name: "vertex-gemini", QuotaSum: 100},
		{Kind: DashboardSourceDeleted, QuotaSum: 40},
		{Kind: DashboardSourceChannelType, ChannelType: common.ChannelTypeOpenAI, QuotaSum: 10},
	}, stats.SourceStats)

	// 调用方（controller 会补展示名）修改返回值不能污染缓存。
	stats.SourceStats[0].Name = "mutated"
	cached, err := GetDashboard24hStats(0)
	require.NoError(t, err)
	require.Len(t, cached.SourceStats, 5)
	require.Empty(t, cached.SourceStats[0].Name)

	userStats, err := GetDashboard24hStats(1)
	require.NoError(t, err)
	require.Empty(t, userStats.SourceStats, "source stats are admin-only")
}

// 来源消耗是附加数据：它的查询失败时，总览原有的统计照常返回，只是没有来源。
func TestDashboard24hAdminStatsSurviveSourceStatsFailure(t *testing.T) {
	// 不建 channels 表：按渠道归组时读取渠道会报错。
	setupTestDB(t, &Log{}, &TopUp{})
	resetDashboardStatsCache()
	t.Cleanup(resetDashboardStatsCache)

	now := time.Now().UTC().Unix()
	require.NoError(t, DB.Create(&Log{UserId: 1, Username: "alice", CreatedAt: now - 60, Type: LogTypeConsume, ModelName: "model-a", Quota: 100, ChannelId: 1}).Error)
	_, err := dashboardSourceStats(now-3600, now)
	require.Error(t, err)

	stats, err := GetDashboard24hStats(0)
	require.NoError(t, err)
	require.Empty(t, stats.SourceStats)
	require.Len(t, stats.ModelStats, 1)
	require.EqualValues(t, 100, stats.ModelStats[0].QuotaSum)
	require.Len(t, stats.UserStats, 1)
}
