package model

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func metricsTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupTestDB(t, &Log{})
	require.NoError(t, MigrateMetricsV2(db))
	oldV2, oldEnabled, oldConsume := config.ModelMetricsV2Enabled, config.ModelMetricsEnabled, config.LogConsumeEnabled
	config.ModelMetricsV2Enabled, config.ModelMetricsEnabled, config.LogConsumeEnabled = true, true, true
	sourceMetricsCache.Lock()
	sourceMetricsCache.entries = map[string]metricsCacheEntry{}
	sourceMetricsCache.bytes = 0
	sourceMetricsCache.Unlock()
	metricsMiniCache.Lock()
	metricsMiniCache.entries = nil
	metricsMiniCache.expires = time.Time{}
	metricsMiniCache.Unlock()
	t.Cleanup(func() {
		config.ModelMetricsV2Enabled, config.ModelMetricsEnabled, config.LogConsumeEnabled = oldV2, oldEnabled, oldConsume
	})
	return db
}
func TestMetricsCaptureSourceRetriesAndAsyncLog(t *testing.T) {
	metricsTestDB(t)
	for _, early := range []bool{true, false} {
		t.Run(map[bool]string{true: "log-before-return", false: "log-after-return"}[early], func(t *testing.T) {
			ctx, finish := BeginMetricsCapture(context.Background())
			RecordMetricsAttempt(ctx, " Azure ", 1, time.Now().Add(-time.Second), "error")
			log := &Log{Type: LogTypeConsume, ChannelId: 2, PromptTokens: 100, CompletionTokens: 20}
			writes := 0
			write := func(log *Log) { writes++; require.Equal(t, 2, log.MetricsVersion) }
			if early {
				writeMetricsLog(ctx, log, write)
				require.Zero(t, writes)
			}
			RecordMetricsAttempt(ctx, "openai", 2, time.Now().Add(-2*time.Second), "success")
			finish()
			if !early {
				writeMetricsLog(ctx, log, write)
			}
			require.Equal(t, 1, writes)
			attempts, err := validateMetricsAttempts(log.MetricsAttempts)
			require.NoError(t, err)
			require.Len(t, attempts, 2)
			require.Equal(t, "azure", attempts[0].Source)
			require.Equal(t, "error", attempts[0].Outcome)
			require.False(t, attempts[0].UsageKnown)
			require.Equal(t, "openai", attempts[1].Source)
			require.EqualValues(t, 20, attempts[1].Completion)
			duplicate := &Log{Type: LogTypeError, ChannelId: 2}
			writeMetricsLog(ctx, duplicate, func(*Log) {})
			require.Zero(t, duplicate.MetricsVersion)
		})
	}
}
func TestMetricsCaptureChargedFailureNotSuccess(t *testing.T) {
	metricsTestDB(t)
	ctx, finish := BeginMetricsCapture(context.Background())
	log := &Log{Type: LogTypeConsume, ChannelId: 3}
	writeMetricsLog(ctx, log, func(*Log) {})
	RecordMetricsAttempt(ctx, "xai", 3, time.Now().Add(-time.Second), "error")
	finish()
	a, err := validateMetricsAttempts(log.MetricsAttempts)
	require.NoError(t, err)
	require.Equal(t, "error", a[0].Outcome)
}
func prepareMetricsJob(t *testing.T, db *gorm.DB, start int64) (*MetricsV2State, *MetricsV2Job) {
	t.Helper()
	s, err := acquireMetricsLease(db)
	require.NoError(t, err)
	s.CoverageStart, s.NextBucket, s.FinalizedBefore = start, start, start
	require.NoError(t, db.Model(&MetricsV2State{}).Where("id = 1").Updates(map[string]interface{}{"coverage_start": start, "next_bucket": start, "finalized_before": start}).Error)
	j := &MetricsV2Job{Start: start, NextRun: start + 420}
	require.NoError(t, db.Create(j).Error)
	return s, j
}
func completeMetricsJob(t *testing.T, db *gorm.DB, s *MetricsV2State, j *MetricsV2Job) {
	t.Helper()
	for n := 0; n < 10; n++ {
		done, err := scanMetricsPage(db, s, j)
		require.NoError(t, err)
		if done {
			require.NoError(t, publishMetricsBucket(db, s, j, time.Now().Unix()))
			return
		}
	}
	t.Fatal("job did not complete")
}
func TestMetricsAggregationReplayRollupsAndSources(t *testing.T) {
	db := metricsTestDB(t)
	start := time.Now().Unix()/300*300 - 600
	s, j := prepareMetricsJob(t, db, start)
	attempts := []MetricsAttempt{{Source: "azure", Channel: 1, Outcome: "error", Duration: 1}, {Source: "openai", Channel: 2, Outcome: "success", Duration: 120, Prompt: 10, Completion: 20, UsageKnown: true}}
	require.NoError(t, db.Create(&Log{CreatedAt: start + 10, Type: 2, ModelName: "gpt-6-astra", MetricsVersion: 2, MetricsAttempts: metricsJSON(attempts)}).Error)
	completeMetricsJob(t, db, s, j)
	s.NextBucket = start + 300
	azure, err := metricsRange(db, "gpt-6-astra", "azure", start, start+300)
	require.NoError(t, err)
	require.EqualValues(t, 1, azure.Errors)
	require.Zero(t, azure.FinalRequests)
	openai, err := metricsRange(db, "gpt-6-astra", "openai", start, start+300)
	require.NoError(t, err)
	require.EqualValues(t, 1, openai.Success)
	require.EqualValues(t, 1, openai.FinalRequests)
	p99, _ := metricPercentile(openai, .99)
	require.Greater(t, *p99, 60.0)
	// 较低 ID 的迟到记录仍可在重跑固定区间时找到，不依赖最大 ID 水位。
	require.NoError(t, db.Create(&Log{CreatedAt: start + 5, Type: 2, ModelName: "gpt-6-astra", MetricsVersion: 2, MetricsAttempts: metricsJSON([]MetricsAttempt{{Source: "azure", Channel: 3, Outcome: "success", Duration: 2}})}).Error)
	require.NoError(t, db.First(j, "start = ?", start).Error)
	completeMetricsJob(t, db, s, j)
	azure, err = metricsRange(db, "gpt-6-astra", "azure", start, start+300)
	require.NoError(t, err)
	require.EqualValues(t, 2, azure.Attempts)
	var hourly MetricsV2Bucket
	require.NoError(t, db.Where("resolution = 3600 AND model = ? AND source = ? AND channel = 0", "gpt-6-astra", "azure").First(&hourly).Error)
	rollup, err := decodeMetrics(hourly.Payload)
	require.NoError(t, err)
	require.Equal(t, azure, rollup)
	require.NoError(t, publishMetricsSnapshots(db, s))
	data, err := GetMetricsV2Snapshot(context.Background(), "gpt-6-astra", "azure")
	require.NoError(t, err)
	require.NotNil(t, data)
	require.EqualValues(t, 2, data.Periods["1h"].Summary.TotalRequests)
	require.InDelta(t, .5, *data.Periods["1h"].Summary.SuccessRate, 1e-9)
	require.Len(t, data.Periods["1h"].Points, 12)
	require.True(t, data.Periods["30d"].Partial)
	require.Nil(t, data.Periods["1h"].Points[0].SuccessRate)
}
func TestMetricsPageRollbackAndLeaseFencing(t *testing.T) {
	db := metricsTestDB(t)
	start := time.Now().Unix()/300*300 - 600
	s, j := prepareMetricsJob(t, db, start)
	require.NoError(t, db.Create(&Log{CreatedAt: start + 1, Type: 2, ModelName: "gpt", MetricsVersion: 2, MetricsAttempts: metricsJSON([]MetricsAttempt{{Source: "openai", Channel: 1, Outcome: "success", Duration: 1}})}).Error)
	_, err := acquireMetricsLease(db)
	require.ErrorIs(t, err, errMetricsLease)
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("fail_cursor", func(tx *gorm.DB) {
		if tx.Statement.Table == "metrics_v2_jobs" {
			tx.AddError(errors.New("injected cursor failure"))
		}
	}))
	_, err = scanMetricsPage(db, s, j)
	require.Error(t, err)
	var count int64
	require.NoError(t, db.Model(&MetricsV2Stage{}).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, db.Callback().Update().Remove("fail_cursor"))
	_, err = scanMetricsPage(db, s, j)
	require.NoError(t, err)
	require.NoError(t, db.Model(&MetricsV2State{}).Where("id = 1").Update("lease_until", 0).Error)
	next, err := acquireMetricsLease(db)
	require.NoError(t, err)
	require.NotEqual(t, s.Generation, next.Generation)
	require.ErrorIs(t, publishMetricsBucket(db, s, j, time.Now().Unix()), errMetricsLease)
	require.NoError(t, db.First(j, "start = ?", start).Error)
	completeMetricsJob(t, db, next, j)
}
func TestMetricsSnapshotsNeverReadLogs(t *testing.T) {
	db := metricsTestDB(t)
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("no_logs", func(tx *gorm.DB) { require.NotEqual(t, "logs", tx.Statement.Table) }))
	data := MetricsV2Data{SchemaVersion: 2, Model: "m", Source: "azure", Version: 1, Periods: map[string]MetricsV2Period{}}
	require.NoError(t, db.Create(&MetricsV2Snapshot{Model: "m", Source: "azure", Version: 1, AsOf: time.Now().Unix(), Payload: metricsJSON(data), Mini: metricsJSON(statsForMetrics(metricsSums{}, 3600))}).Error)
	got, err := GetMetricsV2Snapshot(context.Background(), "m", "azure")
	require.NoError(t, err)
	require.Equal(t, "azure", got.Source)
	missing, err := GetMetricsV2Snapshot(context.Background(), "m", "openai")
	require.NoError(t, err)
	require.Nil(t, missing)
	mini, err := GetMetricsV2Mini(context.Background())
	require.NoError(t, err)
	require.Len(t, mini, 1)
	encoded, err := json.Marshal(mini)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"success_rate":null`)
}

func TestMetricsSnapshotExactMixedResolutionWindows(t *testing.T) {
	db := metricsTestDB(t)
	end := time.Now().Unix() / 300 * 300
	start := end - 30*86400
	rows := []MetricsV2Bucket{}
	hourly := map[int64]metricsSums{}
	daily := map[int64]metricsSums{}
	for ts := start; ts < end; ts += 300 {
		v := metricsSums{}
		v.record(MetricsAttempt{Source: "azure", Channel: 1, Outcome: "success", Duration: 120}, true)
		rows = append(rows, MetricsV2Bucket{Resolution: 300, Start: ts, Model: "window", Source: "azure", Payload: metricsJSON(v)})
		h := ts - ts%3600
		sum := hourly[h]
		sum.add(v, 1)
		hourly[h] = sum
		d := ts - ts%86400
		sum = daily[d]
		sum.add(v, 1)
		daily[d] = sum
	}
	for ts, v := range hourly {
		rows = append(rows, MetricsV2Bucket{Resolution: 3600, Start: ts, Model: "window", Source: "azure", Payload: metricsJSON(v)})
	}
	for ts, v := range daily {
		rows = append(rows, MetricsV2Bucket{Resolution: 86400, Start: ts, Model: "window", Source: "azure", Payload: metricsJSON(v)})
	}
	require.NoError(t, db.CreateInBatches(rows, 50).Error)
	queries := 0
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("count_snapshot", func(tx *gorm.DB) {
		if tx.Statement.Table == "metrics_v2_buckets" {
			queries++
		}
	}))
	data, err := buildMetricsSnapshot(db, &MetricsV2State{CoverageStart: start, NextBucket: end}, MetricsV2Snapshot{Model: "window", Source: "azure"})
	require.NoError(t, err)
	for period, count := range map[string]int64{"1h": 12, "24h": 288, "7d": 2016, "30d": 8640} {
		p := data.Periods[period]
		require.Equal(t, count, p.Summary.TotalRequests)
		require.False(t, p.Partial)
		require.InDelta(t, 120, *p.Summary.AvgLatency, 1e-9)
	}
	require.Equal(t, 1, queries)
}

func TestMetricsCleanupHonorsPausedWatermark(t *testing.T) {
	db := metricsTestDB(t)
	require.NoError(t, db.AutoMigrate(&RankingState{}, &RankingJob{}, &RankingDaily{}))
	old := config.RankingsEnabled
	config.RankingsEnabled = false
	t.Cleanup(func() { config.RankingsEnabled = old })
	now := time.Now().Unix() / 300 * 300
	cutoff := now - 600
	require.NoError(t, db.Create(&MetricsV2State{ID: 1, CoverageStart: cutoff - 300, NextBucket: now, FinalizedBefore: cutoff, Paused: true}).Error)
	require.NoError(t, db.Create(&Log{CreatedAt: cutoff - 1}).Error)
	require.NoError(t, db.Create(&Log{CreatedAt: cutoff + 1}).Error)
	count, err := DeleteOldLog(now)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	var remaining []Log
	require.NoError(t, db.Find(&remaining).Error)
	require.Len(t, remaining, 1)
	require.Equal(t, cutoff+1, remaining[0].CreatedAt)
}

func TestMetricsHistogramOverflowAndCancellation(t *testing.T) {
	s := metricsSums{}
	s.record(MetricsAttempt{Outcome: "success", Duration: 3600}, true)
	s.record(MetricsAttempt{Outcome: "cancelled", Duration: 1}, false)
	stats := statsForMetrics(s, 300)
	require.InDelta(t, 1, *stats.SuccessRate, 1e-9)
	require.True(t, stats.PercentileCapped)
	require.Equal(t, 1800.0, *stats.P99)
	require.EqualValues(t, 1, stats.LatencySamples)
	require.Nil(t, stats.AvgFirstWord)
	require.Nil(t, stats.AvgSpeed)
}

func TestMetricsCycleCoversBoundaryAndEmptyBuckets(t *testing.T) {
	db := metricsTestDB(t)
	now := time.Now().Unix() / 300 * 300
	start := now - 600
	require.NoError(t, db.Create(&MetricsV2State{ID: 1, CoverageStart: start, NextBucket: start, FinalizedBefore: start}).Error)
	require.NoError(t, db.Create(&Log{CreatedAt: start + 299, Type: 2, ModelName: "edge", MetricsVersion: 2, MetricsAttempts: metricsJSON([]MetricsAttempt{{Source: "openai", Channel: 1, Outcome: "success", Duration: 1}})}).Error)
	require.NoError(t, runMetricsV2Cycle(db, now+120))
	var s MetricsV2State
	require.NoError(t, db.First(&s, 1).Error)
	require.Equal(t, now, s.NextBucket)
	var jobs []MetricsV2Job
	require.NoError(t, db.Order("start").Find(&jobs).Error)
	require.Len(t, jobs, 2)
	require.True(t, jobs[1].Published)
	data, err := GetMetricsV2Snapshot(context.Background(), "edge", "openai")
	require.NoError(t, err)
	require.EqualValues(t, 1, data.Periods["1h"].Summary.TotalRequests)
}

func TestMetricsCaptureConcurrentFinishAndBilling(t *testing.T) {
	metricsTestDB(t)
	for n := 0; n < 100; n++ {
		ctx, finish := BeginMetricsCapture(context.Background(), "requested-model")
		RecordMetricsAttempt(ctx, "openai", 1, time.Now().Add(-time.Second), "success")
		log := &Log{ChannelId: 1, Type: LogTypeConsume, ModelName: "mapped-deployment"}
		var wg sync.WaitGroup
		wg.Add(2)
		var writes int
		go func() { defer wg.Done(); writeMetricsLog(ctx, log, func(*Log) { writes++ }) }()
		go func() { defer wg.Done(); finish() }()
		wg.Wait()
		require.Equal(t, 1, writes)
		require.Equal(t, "requested-model", log.MetricsModelName)
	}
}

func TestMetricsCaptureOverflowIsExplicit(t *testing.T) {
	metricsTestDB(t)
	ctx, finish := BeginMetricsCapture(context.Background())
	for i := 0; i < 65; i++ {
		RecordMetricsAttempt(ctx, "azure", 1, time.Now(), "error")
	}
	finish()
	log := &Log{ChannelId: 1, ModelName: "m", Type: LogTypeError}
	writeMetricsLog(ctx, log, func(*Log) {})
	require.Equal(t, 2, log.MetricsVersion)
	_, err := validateMetricsAttempts(log.MetricsAttempts)
	require.Error(t, err)
}
