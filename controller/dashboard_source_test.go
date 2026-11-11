package controller

import (
	"encoding/json"
	"testing"

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
