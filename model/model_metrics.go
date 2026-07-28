package model

import (
	"encoding/json"
	"math"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ModelMetrics 模型监控预聚合表（小时粒度）
//
// 索引设计（基于查询模式）：
// 1. idx_mm_upsert (唯一): (model_name, provider, channel_id, hour_timestamp) → UPSERT 去重
// 2. idx_mm_model_channel_hour: (model_name, channel_id, hour_timestamp) → 单模型时间范围查询（detail/timeseries）
// 3. idx_mm_channel_hour: (channel_id, hour_timestamp) → 全模型聚合查询（/metrics/all, channel_id=0）
// 4. idx_mm_hour: (hour_timestamp) → 过期数据清理 DELETE
type ModelMetrics struct {
	Id               int64   `json:"id" gorm:"primaryKey;autoIncrement"`
	ModelName        string  `json:"model_name" gorm:"type:varchar(200);uniqueIndex:idx_mm_upsert,priority:1;index:idx_mm_model_channel_hour,priority:1;not null"`
	Provider         string  `json:"provider" gorm:"type:varchar(100);uniqueIndex:idx_mm_upsert,priority:2;not null;default:''"`
	ChannelId        int     `json:"channel_id" gorm:"uniqueIndex:idx_mm_upsert,priority:3;index:idx_mm_model_channel_hour,priority:2;index:idx_mm_channel_hour,priority:1;default:0"`
	HourTimestamp    int64   `json:"hour_timestamp" gorm:"uniqueIndex:idx_mm_upsert,priority:4;index:idx_mm_model_channel_hour,priority:3;index:idx_mm_channel_hour,priority:2;index:idx_mm_hour;not null"`
	TotalRequests    int64   `json:"total_requests" gorm:"default:0"`
	SuccessRequests  int64   `json:"success_requests" gorm:"default:0"`
	ErrorRequests    int64   `json:"error_requests" gorm:"default:0"`
	StreamRequests   int64   `json:"stream_requests" gorm:"default:0"`
	TotalTokens      int64   `json:"total_tokens" gorm:"default:0"`
	PromptTokens     int64   `json:"prompt_tokens" gorm:"default:0"`
	CompletionTokens int64   `json:"completion_tokens" gorm:"default:0"`
	CachedTokens     int64   `json:"cached_tokens" gorm:"default:0"`
	TotalQuota       int64   `json:"total_quota" gorm:"default:0"`
	SumDuration      float64 `json:"sum_duration" gorm:"default:0"`
	SumSpeed         float64 `json:"sum_speed" gorm:"default:0"`
	SpeedCount       int64   `json:"speed_count" gorm:"default:0"`
	SumFirstWord     float64 `json:"sum_first_word" gorm:"default:0"`
	FirstWordCount   int64   `json:"first_word_count" gorm:"default:0"`
	LatencyBuckets   string  `json:"latency_buckets" gorm:"type:text"`
	SpeedBuckets     string  `json:"speed_buckets" gorm:"type:text"`
	CreatedAt        int64   `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt        int64   `json:"updated_at" gorm:"autoUpdateTime"`
}

// HistogramBuckets 直方图桶
type HistogramBuckets struct {
	Boundaries []float64 `json:"boundaries"`
	Counts     []int64   `json:"counts"`
}

// 延迟直方图桶边界: [0, 0.5), [0.5, 1), [1, 2), [2, 3), [3, 5), [5, 10), [10, 30), [30, +∞)
var LatencyBoundaries = []float64{0.5, 1, 2, 3, 5, 10, 30}

// 速度直方图桶边界: [0, 5), [5, 10), [10, 20), [20, 50), [50, 100), [100, 200), [200, +∞)
var SpeedBoundaries = []float64{5, 10, 20, 50, 100, 200}

// NewHistogramBuckets 创建空直方图
func NewHistogramBuckets(boundaries []float64) *HistogramBuckets {
	return &HistogramBuckets{
		Boundaries: boundaries,
		Counts:     make([]int64, len(boundaries)+1),
	}
}

// MarshalHistogram 序列化直方图为 JSON
func MarshalHistogram(h *HistogramBuckets) string {
	if h == nil {
		return ""
	}
	data, err := json.Marshal(h)
	if err != nil {
		return ""
	}
	return string(data)
}

// UnmarshalHistogram 反序列化直方图
func UnmarshalHistogram(s string) *HistogramBuckets {
	if s == "" {
		return nil
	}
	var h HistogramBuckets
	if err := json.Unmarshal([]byte(s), &h); err != nil {
		return nil
	}
	return &h
}

// MergeHistograms 合并两个直方图
func MergeHistograms(a, b *HistogramBuckets) *HistogramBuckets {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	result := &HistogramBuckets{
		Boundaries: a.Boundaries,
		Counts:     make([]int64, len(a.Counts)),
	}
	for i := range result.Counts {
		ca := int64(0)
		cb := int64(0)
		if i < len(a.Counts) {
			ca = a.Counts[i]
		}
		if i < len(b.Counts) {
			cb = b.Counts[i]
		}
		result.Counts[i] = ca + cb
	}
	return result
}

// EstimatePercentile 从直方图估算百分位值
func EstimatePercentile(h *HistogramBuckets, percentile float64) float64 {
	if h == nil {
		return 0
	}
	var total int64
	for _, c := range h.Counts {
		total += c
	}
	if total == 0 {
		return 0
	}

	target := int64(math.Ceil(float64(total) * percentile))
	cumulative := int64(0)
	for i, count := range h.Counts {
		cumulative += count
		if cumulative >= target {
			lowerBound := 0.0
			if i > 0 {
				lowerBound = h.Boundaries[i-1]
			}
			upperBound := 0.0
			if i < len(h.Boundaries) {
				upperBound = h.Boundaries[i]
			} else {
				// 最后一个桶，取前一个边界的2倍作为估算上界
				if len(h.Boundaries) > 0 {
					upperBound = h.Boundaries[len(h.Boundaries)-1] * 2
				}
			}
			if count == 0 {
				return lowerBound
			}
			fraction := float64(target-(cumulative-count)) / float64(count)
			return lowerBound + fraction*(upperBound-lowerBound)
		}
	}
	if len(h.Boundaries) > 0 {
		return h.Boundaries[len(h.Boundaries)-1]
	}
	return 0
}

// aggregatedRow 聚合查询中间结果
type aggregatedRow struct {
	ModelName        string  `gorm:"column:model_name"`
	Provider         string  `gorm:"column:provider"`
	ChannelId        int     `gorm:"column:channel_id"`
	TotalRequests    int64   `gorm:"column:total_requests"`
	SuccessRequests  int64   `gorm:"column:success_requests"`
	ErrorRequests    int64   `gorm:"column:error_requests"`
	StreamRequests   int64   `gorm:"column:stream_requests"`
	TotalTokens      int64   `gorm:"column:total_tokens"`
	PromptTokens     int64   `gorm:"column:prompt_tokens"`
	CompletionTokens int64   `gorm:"column:completion_tokens"`
	CachedTokens     int64   `gorm:"column:cached_tokens"`
	TotalQuota       int64   `gorm:"column:total_quota"`
	SumDuration      float64 `gorm:"column:sum_duration"`
	SumSpeed         float64 `gorm:"column:sum_speed"`
	SpeedCount       int64   `gorm:"column:speed_count"`
	SumFirstWord     float64 `gorm:"column:sum_first_word"`
	FirstWordCount   int64   `gorm:"column:first_word_count"`
}

// AggregateLogsForHour 聚合指定小时的日志数据
func AggregateLogsForHour(hourStart, hourEnd int64) ([]aggregatedRow, error) {
	var rows []aggregatedRow
	err := LOG_DB.Model(&Log{}).
		Select(`model_name, provider, channel_id,
			COUNT(*) as total_requests,
			SUM(CASE WHEN type = ? THEN 1 ELSE 0 END) as success_requests,
			SUM(CASE WHEN type = ? THEN 1 ELSE 0 END) as error_requests,
			SUM(CASE WHEN is_stream THEN 1 ELSE 0 END) as stream_requests,
			COALESCE(SUM(prompt_tokens + completion_tokens), 0) as total_tokens,
			COALESCE(SUM(prompt_tokens), 0) as prompt_tokens,
			COALESCE(SUM(completion_tokens), 0) as completion_tokens,
			COALESCE(SUM(cached_tokens), 0) as cached_tokens,
			COALESCE(SUM(quota), 0) as total_quota,
			COALESCE(SUM(duration), 0) as sum_duration,
			COALESCE(SUM(CASE WHEN speed > 0 THEN speed ELSE 0 END), 0) as sum_speed,
			SUM(CASE WHEN speed > 0 THEN 1 ELSE 0 END) as speed_count,
			COALESCE(SUM(CASE WHEN first_word_latency > 0 THEN first_word_latency ELSE 0 END), 0) as sum_first_word,
			SUM(CASE WHEN first_word_latency > 0 THEN 1 ELSE 0 END) as first_word_count`,
			LogTypeConsume, LogTypeError).
		Where("created_at >= ? AND created_at < ? AND type IN (?, ?)",
			hourStart, hourEnd, LogTypeConsume, LogTypeError).
		Group("model_name, provider, channel_id").
		Scan(&rows).Error
	return rows, err
}

// addToHistogram 在直方图对应桶中加 1（供 histogram 累加器使用）
func addToHistogram(h *HistogramBuckets, value float64) {
	for i, boundary := range h.Boundaries {
		if value < boundary {
			h.Counts[i]++
			return
		}
	}
	h.Counts[len(h.Boundaries)]++
}

// UpsertModelMetrics 批量 UPSERT 模型指标
func UpsertModelMetrics(metrics []ModelMetrics) error {
	if len(metrics) == 0 {
		return nil
	}
	return LOG_DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "model_name"},
			{Name: "provider"},
			{Name: "channel_id"},
			{Name: "hour_timestamp"},
		},
		DoUpdates: clause.AssignmentColumns([]string{
			"total_requests", "success_requests", "error_requests", "stream_requests",
			"total_tokens", "prompt_tokens", "completion_tokens", "cached_tokens", "total_quota",
			"sum_duration", "sum_speed", "speed_count", "sum_first_word", "first_word_count",
			"latency_buckets", "speed_buckets", "updated_at",
		}),
	}).CreateInBatches(metrics, 100).Error
}

// GetModelMetricsRange 查询指定时间范围的预聚合数据
func GetModelMetricsRange(modelName string, channelId int, startHour, endHour int64) ([]ModelMetrics, error) {
	var metrics []ModelMetrics
	tx := LOG_DB.Where("hour_timestamp >= ? AND hour_timestamp < ?", startHour, endHour)
	if modelName != "" {
		tx = tx.Where("model_name = ?", modelName)
	}
	tx = tx.Where("channel_id = ?", channelId)
	err := tx.Order("hour_timestamp ASC").Find(&metrics).Error
	return metrics, err
}

// GetModelMetricsRangeAllChannels 查询指定时间范围所有 channel 的数据（管理员用）
func GetModelMetricsRangeAllChannels(modelName string, startHour, endHour int64) ([]ModelMetrics, error) {
	var metrics []ModelMetrics
	tx := LOG_DB.Where("hour_timestamp >= ? AND hour_timestamp < ? AND channel_id > 0", startHour, endHour)
	if modelName != "" {
		tx = tx.Where("model_name = ?", modelName)
	}
	err := tx.Order("hour_timestamp ASC").Find(&metrics).Error
	return metrics, err
}

// GetMaxHourTimestamp 获取最大小时时间戳
func GetMaxHourTimestamp() (int64, error) {
	var maxHour int64
	err := LOG_DB.Model(&ModelMetrics{}).Select("COALESCE(MAX(hour_timestamp), 0)").Scan(&maxHour).Error
	return maxHour, err
}

// GetMinLogTimestamp 获取最早日志时间戳
func GetMinLogTimestamp() (int64, error) {
	var minTime int64
	err := LOG_DB.Model(&Log{}).Select("COALESCE(MIN(created_at), 0)").Scan(&minTime).Error
	return minTime, err
}

// DeleteOldMetrics 清理过期的预聚合数据
func DeleteOldMetrics(beforeTimestamp int64) (int64, error) {
	result := LOG_DB.Where("hour_timestamp < ?", beforeTimestamp).Delete(&ModelMetrics{})
	return result.RowsAffected, result.Error
}

// GetRecentLogsForModel 获取模型最近N分钟的日志（用于1h实时查询）
func GetRecentLogsForModel(modelName string, startTime, endTime int64) ([]Log, error) {
	var logs []Log
	err := LOG_DB.Where("model_name = ? AND created_at >= ? AND created_at < ? AND type IN (?, ?)",
		modelName, startTime, endTime, LogTypeConsume, LogTypeError).
		Find(&logs).Error
	return logs, err
}

// FloorHour 将 Unix 时间戳下取整到小时
func FloorHour(ts int64) int64 {
	return ts - (ts % 3600)
}

// MigrateModelMetrics 在 LOG_DB 上执行 ModelMetrics 表迁移
func MigrateModelMetrics(db *gorm.DB) error {
	return db.AutoMigrate(&ModelMetrics{})
}

// 辅助：安全除法
func safeDiv(a float64, b int64) float64 {
	if b == 0 {
		return 0
	}
	return a / float64(b)
}

// 辅助：安全除法 float
func safeDivF(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// 辅助：获取当前 UTC 小时起始时间戳
func CurrentHourStart() int64 {
	return time.Now().UTC().Truncate(time.Hour).Unix()
}
