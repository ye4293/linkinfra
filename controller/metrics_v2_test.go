package controller

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
	"github.com/stretchr/testify/require"
)

func TestSourceMetricsHTTPIsolation(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.GroupConfig{}))
	require.NoError(t, model.MigrateMetricsV2(db))
	oldV2, oldEnabled, oldConsume := config.ModelMetricsV2Enabled, config.ModelMetricsEnabled, config.LogConsumeEnabled
	config.ModelMetricsV2Enabled, config.ModelMetricsEnabled, config.LogConsumeEnabled = true, true, true
	t.Cleanup(func() {
		config.ModelMetricsV2Enabled, config.ModelMetricsEnabled, config.LogConsumeEnabled = oldV2, oldEnabled, oldConsume
	})
	for i, kind := range []int{common.ChannelTypeOpenAI, common.ChannelTypeAzure} {
		require.NoError(t, db.Create(&model.Channel{Id: i + 1, Type: kind, Status: 1, Models: "http-metrics-gpt"}).Error)
	}
	for _, source := range []string{"openai", "azure"} {
		count := int64(9)
		if source == "azure" {
			count = 3
		}
		data := model.MetricsV2Data{SchemaVersion: 2, Model: "http-metrics-gpt", Source: source, Version: 1, AsOf: time.Now().Unix(), Periods: map[string]model.MetricsV2Period{"1h": {Summary: model.MetricsV2Stats{TotalRequests: count}, Points: []model.MetricsV2Point{}}}}
		b, err := json.Marshal(data)
		require.NoError(t, err)
		require.NoError(t, db.Create(&model.MetricsV2Snapshot{Model: data.Model, Source: source, Version: 1, AsOf: data.AsOf, Payload: string(b), Mini: `{}`}).Error)
	}
	call := func(query string) (int, map[string]interface{}) {
		t.Helper()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("role", common.RoleCommonUser)
		c.Request = httptest.NewRequest("GET", "/api/model-plaza/metrics/detail?model_name=http-metrics-gpt&"+query, nil)
		GetModelMetricsDetail(c)
		var body map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		return w.Code, body
	}
	status, _ := call("period=1h")
	require.Equal(t, 409, status)
	status, _ = call("period=1h&source_key=azure&channel_id=1")
	require.Equal(t, 400, status)
	for _, query := range []string{"source_key=azure", "channel_id=2"} {
		status, body := call("period=1h&" + query)
		require.Equal(t, 200, status)
		data := body["data"].(map[string]interface{})
		require.Equal(t, "azure", data["source_key"])
		require.EqualValues(t, 3, data["current"].(map[string]interface{})["total_requests"])
		require.Nil(t, data["channels"])
	}
	status, body := call("period=1h&source_key=openai")
	require.Equal(t, 200, status)
	require.EqualValues(t, 9, body["data"].(map[string]interface{})["current"].(map[string]interface{})["total_requests"])
	status, _ = call("period=bad&source_key=azure")
	require.Equal(t, 400, status)
}
