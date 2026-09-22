package model

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

const metricsStep int64 = 300
const metricsRetention int64 = 35 * 86400
const metricsPredicate = "type IN (2, 5) AND metrics_version = 2"

type MetricsV2State struct {
	ID              int `gorm:"primaryKey;autoIncrement:false"`
	CoverageStart   int64
	NextBucket      int64
	FinalizedBefore int64
	Owner           string `gorm:"type:varchar(64)"`
	LeaseUntil      int64
	Generation      int64
	Heartbeat       int64
	Paused          bool
}

func (MetricsV2State) TableName() string { return "metrics_v2_state" }

type MetricsV2Job struct {
	Start      int64 `gorm:"primaryKey;autoIncrement:false"`
	CursorTime int64
	CursorID   int64
	Pass       int
	NextRun    int64 `gorm:"index:idx_mv2_job_due"`
	Scanning   bool
	Published  bool
	Finalized  bool
	Error      string `gorm:"type:text"`
}

func (MetricsV2Job) TableName() string { return "metrics_v2_jobs" }

type MetricsV2Bucket struct {
	ID         int64  `gorm:"primaryKey"`
	Resolution int64  `gorm:"uniqueIndex:idx_mv2_bucket,priority:1"`
	Start      int64  `gorm:"uniqueIndex:idx_mv2_bucket,priority:2;index:idx_mv2_retention,priority:1;index:idx_mv2_source,priority:4"`
	Model      string `gorm:"type:varchar(200);uniqueIndex:idx_mv2_bucket,priority:3;index:idx_mv2_source,priority:1"`
	Source     string `gorm:"type:varchar(200);uniqueIndex:idx_mv2_bucket,priority:4;index:idx_mv2_source,priority:2"`
	Channel    int    `gorm:"uniqueIndex:idx_mv2_bucket,priority:5;index:idx_mv2_source,priority:3"`
	Payload    string `gorm:"type:text"`
}

func (MetricsV2Bucket) TableName() string { return "metrics_v2_buckets" }

// 中间结果使用独立表；页结果与游标同事务保存，完成前不对读者可见。
type MetricsV2Stage struct {
	ID      int64  `gorm:"primaryKey"`
	Start   int64  `gorm:"uniqueIndex:idx_mv2_stage,priority:1"`
	Model   string `gorm:"type:varchar(200);uniqueIndex:idx_mv2_stage,priority:2"`
	Source  string `gorm:"type:varchar(200);uniqueIndex:idx_mv2_stage,priority:3"`
	Channel int    `gorm:"uniqueIndex:idx_mv2_stage,priority:4"`
	Payload string `gorm:"type:text"`
}

func (MetricsV2Stage) TableName() string { return "metrics_v2_stage" }

type MetricsV2Snapshot struct {
	ID      int64  `gorm:"primaryKey"`
	Model   string `gorm:"type:varchar(200);uniqueIndex:idx_mv2_snapshot,priority:1"`
	Source  string `gorm:"type:varchar(200);uniqueIndex:idx_mv2_snapshot,priority:2"`
	Version int64
	AsOf    int64
	Payload string `gorm:"type:text"`
	Mini    string `gorm:"type:text"`
	Dirty   bool   `gorm:"index:idx_mv2_snapshot_dirty"`
}

func (MetricsV2Snapshot) TableName() string { return "metrics_v2_snapshots" }

// 固定边界版本；末桶返回下界并标 capped，不虚构一个上限。
var metricsBounds = []float64{.05, .1, .2, .5, 1, 2, 3, 5, 10, 15, 30, 45, 60, 90, 120, 180, 300, 600, 900, 1800}

type metricsSums struct {
	Attempts      int64     `json:"attempts"`
	Success       int64     `json:"success"`
	Errors        int64     `json:"errors"`
	Cancelled     int64     `json:"cancelled"`
	FinalRequests int64     `json:"final_requests"`
	Duration      float64   `json:"duration"`
	DurationCount int64     `json:"duration_count"`
	Speed         float64   `json:"speed"`
	SpeedCount    int64     `json:"speed_count"`
	TTFT          float64   `json:"ttft"`
	TTFTCount     int64     `json:"ttft_count"`
	Prompt        int64     `json:"prompt"`
	Completion    int64     `json:"completion"`
	UsageCount    int64     `json:"usage_count"`
	Histogram     [21]int64 `json:"histogram"`
}

func (s *metricsSums) add(other metricsSums, sign int64) {
	s.Attempts += sign * other.Attempts
	s.Success += sign * other.Success
	s.Errors += sign * other.Errors
	s.Cancelled += sign * other.Cancelled
	s.FinalRequests += sign * other.FinalRequests
	s.Duration += float64(sign) * other.Duration
	s.DurationCount += sign * other.DurationCount
	s.Speed += float64(sign) * other.Speed
	s.SpeedCount += sign * other.SpeedCount
	s.TTFT += float64(sign) * other.TTFT
	s.TTFTCount += sign * other.TTFTCount
	s.Prompt += sign * other.Prompt
	s.Completion += sign * other.Completion
	s.UsageCount += sign * other.UsageCount
	for i := range s.Histogram {
		s.Histogram[i] += sign * other.Histogram[i]
	}
}
func (s *metricsSums) record(a MetricsAttempt, final bool) {
	s.Attempts++
	if final {
		s.FinalRequests++
	}
	switch a.Outcome {
	case "success":
		s.Success++
	case "cancelled":
		s.Cancelled++
	default:
		s.Errors++
	}
	if a.UsageKnown {
		s.Prompt += a.Prompt
		s.Completion += a.Completion
		s.UsageCount++
	}
	if a.Outcome != "success" {
		return
	}
	if a.Duration > 0 && !math.IsInf(a.Duration, 0) && !math.IsNaN(a.Duration) {
		s.Duration += a.Duration
		s.DurationCount++
		i := 0
		for i < len(metricsBounds) && a.Duration >= metricsBounds[i] {
			i++
		}
		s.Histogram[i]++
		if a.UsageKnown && a.Completion > 0 {
			s.Speed += float64(a.Completion) / a.Duration
			s.SpeedCount++
		}
	}
	if a.TTFT > 0 && a.TTFT <= a.Duration {
		s.TTFT += a.TTFT
		s.TTFTCount++
	}
}
func metricsJSON(v interface{}) string { b, _ := json.Marshal(v); return string(b) }
func decodeMetrics(raw string) (metricsSums, error) {
	var s metricsSums
	err := json.Unmarshal([]byte(raw), &s)
	return s, err
}
func metricRatio(n float64, d int64) *float64 {
	if d <= 0 {
		return nil
	}
	v := n / float64(d)
	return &v
}
func metricPercentile(s metricsSums, p float64) (*float64, bool) {
	if s.DurationCount == 0 {
		return nil, false
	}
	target := int64(math.Ceil(float64(s.DurationCount) * p))
	var cumulative int64
	for i, n := range s.Histogram {
		cumulative += n
		if cumulative >= target {
			lo := 0.0
			if i > 0 {
				lo = metricsBounds[i-1]
			}
			if i == len(metricsBounds) {
				return &lo, true
			}
			v := lo + (metricsBounds[i]-lo)*float64(target-(cumulative-n))/float64(n)
			return &v, false
		}
	}
	return nil, false
}

type MetricsV2Stats struct {
	TotalRequests    int64    `json:"total_requests"`
	FinalRequests    int64    `json:"final_requests"`
	Cancelled        int64    `json:"cancelled"`
	SuccessRate      *float64 `json:"success_rate"`
	AvgLatency       *float64 `json:"avg_latency"`
	AvgSpeed         *float64 `json:"avg_speed"`
	AvgFirstWord     *float64 `json:"avg_first_word"`
	P50              *float64 `json:"p50_latency"`
	P95              *float64 `json:"p95_latency"`
	P99              *float64 `json:"p99_latency"`
	PercentileCapped bool     `json:"percentile_capped"`
	Prompt           int64    `json:"prompt_tokens"`
	Completion       int64    `json:"completion_tokens"`
	TotalTokens      int64    `json:"total_tokens"`
	UsageSamples     int64    `json:"usage_samples"`
	LatencySamples   int64    `json:"latency_samples"`
	RPM              float64  `json:"rpm"`
	TPM              float64  `json:"tpm"`
	Status           string   `json:"status"`
}

func statsForMetrics(s metricsSums, seconds int64) MetricsV2Stats {
	stat := MetricsV2Stats{TotalRequests: s.Attempts, FinalRequests: s.FinalRequests, Cancelled: s.Cancelled, SuccessRate: metricRatio(float64(s.Success), s.Success+s.Errors), AvgLatency: metricRatio(s.Duration, s.DurationCount), AvgSpeed: metricRatio(s.Speed, s.SpeedCount), AvgFirstWord: metricRatio(s.TTFT, s.TTFTCount), Prompt: s.Prompt, Completion: s.Completion, TotalTokens: s.Prompt + s.Completion, UsageSamples: s.UsageCount, LatencySamples: s.DurationCount, Status: "no_data"}
	stat.P50, _ = metricPercentile(s, .5)
	stat.P95, _ = metricPercentile(s, .95)
	stat.P99, stat.PercentileCapped = metricPercentile(s, .99)
	if seconds > 0 {
		stat.RPM = float64(s.Attempts) * 60 / float64(seconds)
		stat.TPM = float64(stat.TotalTokens) * 60 / float64(seconds)
	}
	if s.Attempts > 0 {
		stat.Status = "insufficient_data"
	}
	if s.Success+s.Errors >= 20 {
		stat.Status = "down"
		if *stat.SuccessRate >= .95 {
			stat.Status = "healthy"
		} else if *stat.SuccessRate >= .8 {
			stat.Status = "degraded"
		}
	}
	return stat
}

type MetricsV2Point struct {
	Timestamp int64 `json:"timestamp"`
	End       int64 `json:"end"`
	MetricsV2Stats
}
type MetricsV2Period struct {
	Start   int64            `json:"window_start"`
	End     int64            `json:"window_end"`
	Partial bool             `json:"partial"`
	Summary MetricsV2Stats   `json:"summary"`
	Points  []MetricsV2Point `json:"points"`
}
type MetricsV2Data struct {
	SchemaVersion int                        `json:"schema_version"`
	Model         string                     `json:"model_name"`
	Source        string                     `json:"source_key"`
	AsOf          int64                      `json:"as_of"`
	CoverageStart int64                      `json:"coverage_start"`
	Version       int64                      `json:"version"`
	Periods       map[string]MetricsV2Period `json:"periods"`
}

func MetricsSourceKey(provider string) string { return strings.ToLower(strings.TrimSpace(provider)) }
func MetricsIdentity(model, source string) string {
	return metricsJSON([]string{model, MetricsSourceKey(source)})
}
func validateMetricsAttempts(raw string) ([]MetricsAttempt, error) {
	if len(raw) > 32768 {
		return nil, fmt.Errorf("metrics payload exceeds limit")
	}
	var a []MetricsAttempt
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return nil, err
	}
	if len(a) == 0 || len(a) > 64 {
		return nil, fmt.Errorf("invalid attempt count")
	}
	for _, v := range a {
		if v.Source == "" || len(v.Source) > 200 || v.Channel <= 0 || v.Duration < 0 || math.IsInf(v.Duration, 0) || math.IsNaN(v.Duration) || v.Prompt < 0 || v.Completion < 0 {
			return nil, fmt.Errorf("invalid attempt")
		}
		if v.Outcome != "success" && v.Outcome != "error" && v.Outcome != "cancelled" {
			return nil, fmt.Errorf("invalid outcome")
		}
	}
	return a, nil
}
