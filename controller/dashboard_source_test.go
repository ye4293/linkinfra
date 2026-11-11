package controller

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
	"github.com/stretchr/testify/require"
)

// 总览页的来源消耗：渠道类型的展示名与 /api/channel/types 一致，标签和已删除渠道保持原样。
func TestNameDashboardSources(t *testing.T) {
	stats := model.Dashboard24hStats{SourceStats: []model.DashboardSourceStats{
		{Kind: model.DashboardSourceChannelType, ChannelType: common.ChannelTypeVertexAI, QuotaSum: 325},
		{Kind: model.DashboardSourceTag, Tag: "vertex-claude", Name: "vertex-claude", QuotaSum: 250},
		{Kind: model.DashboardSourceDeleted, QuotaSum: 40},
		{Kind: model.DashboardSourceChannelType, ChannelType: common.ChannelTypeAPI2D, QuotaSum: 10},
		{Kind: model.DashboardSourceChannelType, ChannelType: 9001, QuotaSum: 5},
	}}
	nameDashboardSources(stats.SourceStats)

	names := make([]string, 0, len(stats.SourceStats))
	for _, source := range stats.SourceStats {
		names = append(names, source.Name)
	}
	require.Equal(t, []string{"Vertex AI", "vertex-claude", "", "API2D", "Channel type 9001"}, names)

	// 前端依赖的字段名。
	payload, err := json.Marshal(stats.SourceStats[:3])
	require.NoError(t, err)
	require.JSONEq(t, `[
		{"kind":"channel_type","channel_type":48,"name":"Vertex AI","quota_sum":325},
		{"kind":"tag","tag":"vertex-claude","name":"vertex-claude","quota_sum":250},
		{"kind":"deleted","name":"","quota_sum":40}
	]`, string(payload))
}

// 管理员总览接口返回的来源消耗带展示名：接口确实做了补名，而不只是辅助函数可用。
// 总览结果在 model 包里按进程缓存 10 分钟，从这个包清不掉；本包只有这一个测试会计算管理员总览，
// 以后再加同类测试要用同一份数据，否则会读到这里留下的缓存。
func TestAdminDashboardStatsNamesSources(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.TopUp{}))
	require.NoError(t, db.Create(&[]model.Channel{
		{Id: 1, Type: common.ChannelTypeVertexAI},
		{Id: 2, Type: common.ChannelTypeVertexAI, Config: `{"billing_tag":"vertex-claude"}`},
	}).Error)
	now := time.Now().UTC().Unix()
	require.NoError(t, db.Create(&[]model.Log{
		{UserId: 1, Username: "alice", CreatedAt: now - 60, Type: model.LogTypeConsume, ModelName: "gemini", Quota: 300, ChannelId: 1},
		{UserId: 1, Username: "alice", CreatedAt: now - 60, Type: model.LogTypeConsume, ModelName: "claude", Quota: 200, ChannelId: 2},
	}).Error)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/dashboard/stats", nil)
	GetAdminDashboardStats(c)

	var response struct {
		Success bool `json:"success"`
		Data    struct {
			SourceStats json.RawMessage `json:"source_stats"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response), w.Body.String())
	require.True(t, response.Success, w.Body.String())
	require.JSONEq(t, `[
		{"kind":"channel_type","channel_type":48,"name":"Vertex AI","quota_sum":300},
		{"kind":"tag","tag":"vertex-claude","name":"vertex-claude","quota_sum":200}
	]`, string(response.Data.SourceStats))
}
