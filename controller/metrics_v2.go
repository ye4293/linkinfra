package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
)

func GetMetricsV2Status(c *gin.Context) {
	if !config.ModelMetricsV2Enabled {
		c.JSON(200, gin.H{"success": true, "enabled": false})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	var state model.MetricsV2State
	if err := model.LOG_DB.WithContext(ctx).First(&state, 1).Error; err != nil {
		c.JSON(503, gin.H{"success": false, "status": "preparing"})
		return
	}
	var jobs []model.MetricsV2Job
	if err := model.LOG_DB.WithContext(ctx).Where("next_run <= ? AND finalized = ?", time.Now().Unix(), false).Order("next_run").Limit(20).Find(&jobs).Error; err != nil {
		c.JSON(503, gin.H{"success": false})
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(200, gin.H{"success": true, "enabled": true, "data": gin.H{"coverage_start": state.CoverageStart, "as_of": state.NextBucket, "finalized_before": state.FinalizedBefore, "paused": state.Paused, "lag_seconds": time.Now().Unix() - state.NextBucket, "pending_sample": jobs}})
}
func RebuildSourceMetrics(c *gin.Context) {
	var input struct {
		Start int64 `json:"bucket_start"`
	}
	if !config.ModelMetricsV2Enabled || c.ShouldBindJSON(&input) != nil {
		c.JSON(400, gin.H{"success": false, "message": "invalid rebuild request"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	if err := model.RebuildMetricsV2Bucket(model.LOG_DB.WithContext(ctx), input.Start); err != nil {
		c.JSON(409, gin.H{"success": false, "message": "bucket unavailable, finalized, or worker busy"})
		return
	}
	c.JSON(200, gin.H{"success": true, "message": "rebuild queued"})
}

func sourceMetricsEnabled(c *gin.Context) bool {
	if config.ModelMetricsEnabled && config.LogConsumeEnabled {
		return true
	}
	c.JSON(http.StatusOK, gin.H{"success": false, "status": "disabled", "data": nil})
	return false
}
func getSourceMetricsMini(c *gin.Context) {
	if !sourceMetricsEnabled(c) {
		return
	}
	items, err := model.GetMetricsV2Mini(c.Request.Context())
	if err != nil {
		c.JSON(503, gin.H{"success": false, "status": "preparing", "data": nil})
		return
	}
	c.Header("Cache-Control", "public, max-age=30, stale-while-revalidate=60")
	c.JSON(200, gin.H{"success": true, "schema_version": 2, "data": items})
}
func sourceMetricsSelection(c *gin.Context) (string, string, string, *ModelPlazaItem, bool) {
	name := c.Query("model_name")
	period := c.DefaultQuery("period", "24h")
	source := model.MetricsSourceKey(c.Query("source_key"))
	if name == "" || len(name) > 200 || len(source) > 200 {
		c.JSON(400, gin.H{"success": false, "message": "invalid model or source"})
		return "", "", "", nil, false
	}
	if period != "1h" && period != "24h" && period != "7d" && period != "30d" {
		c.JSON(400, gin.H{"success": false, "message": "invalid period"})
		return "", "", "", nil, false
	}
	var pricing *ModelPlazaItem
	if raw, supplied := c.GetQuery("channel_id"); supplied {
		id, err := strconv.Atoi(raw)
		if err != nil || id <= 0 {
			c.JSON(400, gin.H{"success": false, "message": "invalid channel_id"})
			return "", "", "", nil, false
		}
		pricing = getModelPricing(name, id)
		if pricing == nil && source == "" {
			c.JSON(404, gin.H{"success": false, "message": "channel model not found"})
			return "", "", "", nil, false
		}
		if pricing != nil {
			if source != "" && source != pricing.SourceKey {
				c.JSON(400, gin.H{"success": false, "message": "channel/source mismatch"})
				return "", "", "", nil, false
			}
			source = pricing.SourceKey
		}
	}
	if pricing == nil {
		for key, info := range deduplicateModelCatalog(getModelInfoFromChannels()) {
			if key.ModelName != name || (source != "" && source != model.MetricsSourceKey(info.Provider)) {
				continue
			}
			if pricing != nil && source == "" {
				c.JSON(409, gin.H{"success": false, "status": "source_required", "message": "source_key is required for a model with multiple sources"})
				return "", "", "", nil, false
			}
			pricing = getModelPricing(name, key.ChannelID)
		}
		if pricing != nil {
			source = pricing.SourceKey
		}
	}
	if source == "" {
		c.JSON(404, gin.H{"success": false, "message": "model source not found"})
		return "", "", "", nil, false
	}
	return name, source, period, pricing, true
}
func sourceMetricsData(c *gin.Context) (*model.MetricsV2Data, string, *ModelPlazaItem, bool) {
	if !sourceMetricsEnabled(c) {
		return nil, "", nil, false
	}
	name, source, period, pricing, ok := sourceMetricsSelection(c)
	if !ok {
		return nil, "", nil, false
	}
	data, err := model.GetMetricsV2Snapshot(c.Request.Context(), name, source)
	if err != nil {
		c.JSON(503, gin.H{"success": false, "status": "unavailable", "message": "monitoring temporarily unavailable"})
		return nil, "", nil, false
	}
	if data == nil {
		provider := source
		if pricing != nil {
			provider = pricing.Provider
		}
		c.JSON(200, gin.H{"success": true, "schema_version": 2, "status": "preparing", "data": gin.H{"model_name": name, "source_key": source, "provider": provider, "pricing": pricing, "current": nil, "period_24h": nil, "points": []model.MetricsV2Point{}}})
		return nil, "", nil, false
	}
	return data, period, pricing, true
}
func getSourceMetricsDetail(c *gin.Context) {
	data, period, pricing, ok := sourceMetricsData(c)
	if !ok {
		return
	}
	selected := data.Periods[period]
	provider := data.Source
	if pricing != nil {
		provider = pricing.Provider
	}
	// 管理员响应不能进入共享缓存。
	c.Header("Cache-Control", "private, no-store")
	var channels []model.ChannelMetricsSummary
	if isRequestFromAdmin(c) {
		var err error
		channels, err = model.GetMetricsV2Channels(c.Request.Context(), data.Model, data.Source)
		if err != nil {
			channels = []model.ChannelMetricsSummary{}
		}
	}
	c.JSON(200, gin.H{"success": true, "schema_version": 2, "data": gin.H{"model_name": data.Model, "source_key": data.Source, "provider": provider, "period": period, "current": selected.Summary, "period_24h": data.Periods["24h"].Summary, "pricing": pricing, "channels": channels, "as_of": data.AsOf, "partial": selected.Partial, "stale": time.Now().Unix()-data.AsOf > 900, "window_start": selected.Start, "window_end": selected.End}})
}
func getSourceMetricsSeries(c *gin.Context) {
	data, period, _, ok := sourceMetricsData(c)
	if !ok {
		return
	}
	etag := fmt.Sprintf(`"mv2-%x"`, sha256.Sum256([]byte(strconv.FormatInt(data.Version, 10)+model.MetricsIdentity(data.Model, data.Source)+period)))
	c.Header("ETag", etag)
	if c.GetHeader("If-None-Match") == etag {
		c.Status(http.StatusNotModified)
		return
	}
	c.Header("Cache-Control", "public, max-age=30, stale-while-revalidate=60")
	p := data.Periods[period]
	c.JSON(200, gin.H{"success": true, "schema_version": 2, "data": gin.H{"model_name": data.Model, "source_key": data.Source, "period": period, "points": p.Points, "summary": p.Summary, "as_of": data.AsOf, "partial": p.Partial, "stale": time.Now().Unix()-data.AsOf > 900, "window_start": p.Start, "window_end": p.End}})
}
