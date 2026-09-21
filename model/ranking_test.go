package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func rankingTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupTestDB(t, &Log{}, &RankingDaily{}, &RankingState{}, &RankingJob{}, &RankingSnapshot{})
	oldPause, oldEnabled, oldConsume := config.RankingBatchPauseMS, config.RankingsEnabled, config.LogConsumeEnabled
	config.RankingBatchPauseMS = 0
	config.RankingsEnabled = true
	config.LogConsumeEnabled = true
	rankingCache.Store(nil)
	t.Cleanup(func() {
		config.RankingBatchPauseMS = oldPause
		config.RankingsEnabled = oldEnabled
		config.LogConsumeEnabled = oldConsume
		rankingCache.Store(nil)
	})
	return db
}

func TestRankingIdentityAndSnapshot(t *testing.T) {
	for _, name := range []string{"gpt-image-1", "gemini-2.5-flash-image", "arn:aws:bedrock:model/claude", "deployment-a", "qwen3-embedding"} {
		model, _ := RankingModelIdentity(name)
		require.Empty(t, model, name)
	}
	name, author := RankingModelIdentity(" Anthropic/Claude-Sonnet-4-20250514 ")
	require.Equal(t, "claude-sonnet-4-20250514", name)
	require.Equal(t, "Anthropic", author)
	ctx := WithRankingUsage(context.Background(), name, " AZURE-A ", 123)
	log := Log{}
	applyRankingUsage(ctx, &log)
	require.Equal(t, "azure-a", log.Provider)
	require.EqualValues(t, 123, *log.RankingTokens)
	require.Equal(t, name, log.RankingModelName)
	plain := Log{}
	applyRankingUsage(context.Background(), &plain)
	require.Nil(t, plain.RankingTokens)
}

func TestRankingWindowsMergeProvidersAndConserveTrend(t *testing.T) {
	end := floorRankingDay(time.Now().Unix())
	rows := []RankingDaily{{DayStart: end - rankingDay, ModelName: "gpt-4.1", Provider: "openai", Tokens: 100}, {DayStart: end - rankingDay, ModelName: "gpt-4.1", Provider: "azure", Tokens: 200}, {DayStart: end - 2*rankingDay, ModelName: "gpt-4.1", Tokens: 100}, {DayStart: end - 31*rankingDay, ModelName: "gpt-4.1", Tokens: 200}}
	for i := 0; i < 12; i++ {
		rows = append(rows, RankingDaily{DayStart: end - rankingDay, ModelName: fmt.Sprintf("qwen3-%d", i), Tokens: 1})
	}
	data := buildRankingData(rows, end-60*rankingDay, end, end)
	day := data.Leaderboards["day"]
	require.EqualValues(t, 312, day.TotalTokens)
	require.Equal(t, "gpt-4.1", day.Entries[0].Model)
	require.EqualValues(t, 300, day.Entries[0].Tokens)
	require.InDelta(t, 200, *day.Entries[0].ChangePercent, 0.0001)
	require.Len(t, data.Trend.Series, 11)
	require.Len(t, data.Trend.Dates, 30)
	var sum int64
	for _, series := range data.Trend.Series {
		for _, v := range series.Tokens {
			sum += v
		}
	}
	require.Equal(t, data.Leaderboards["month"].TotalTokens, sum)
	partial := buildRankingData(rows, end-rankingDay, end, end)
	require.False(t, partial.Leaderboards["week"].WindowComplete)
	require.False(t, partial.Leaderboards["day"].ComparisonComplete)
	require.Nil(t, partial.Leaderboards["day"].Entries[0].ChangePercent)
	require.EqualValues(t, 312, partial.Leaderboards["month"].TotalTokens, "rows before coverage must not reappear after a logging gap")
}

func TestRankingDailyIdempotenceLateRecheckAndNoRequestScan(t *testing.T) {
	db := rankingTestDB(t)
	now := time.Now().Unix()
	today := floorRankingDay(now)
	day := today - rankingDay
	require.NoError(t, db.Create(&RankingState{ID: 1, CoverageStart: day, NextDay: day}).Error)
	tokens := int64(100)
	require.NoError(t, db.Create(&[]Log{
		{CreatedAt: day + 1, Type: LogTypeConsume, RankingModelName: "gpt-4.1", Provider: "openai", RankingTokens: &tokens},
		{CreatedAt: day + 3599, Type: LogTypeConsume, RankingModelName: "gpt-4.1", Provider: "azure", RankingTokens: &tokens},
		{CreatedAt: day + 3600, Type: LogTypeError, RankingModelName: "gpt-4.1", RankingTokens: &tokens},
		{CreatedAt: day + 3601, Type: LogTypeConsume, ModelName: "image", CompletionTokens: 10},
		{CreatedAt: today, Type: LogTypeConsume, RankingModelName: "gpt-4.1", RankingTokens: &tokens},
	}).Error)
	// now 只控制自然日调度；租约期限使用真实时钟。
	runAt := today + 3600
	if now > runAt {
		runAt = now
	}
	require.NoError(t, runRankingCycle(db, runAt))
	require.NoError(t, RefreshRankingCache())
	cached := GetCachedRanking()
	require.NotNil(t, cached)
	var response struct {
		Data RankingData `json:"data"`
	}
	require.NoError(t, json.Unmarshal(cached.Payload, &response))
	require.EqualValues(t, 200, response.Data.Leaderboards["day"].TotalTokens)
	// 同日再次执行不重扫日志；读榜单也完全不依赖 DB。
	scans := 0
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("count_ranking_logs", func(tx *gorm.DB) {
		if tx.Statement.Table == "logs" {
			scans++
		}
	}))
	require.NoError(t, db.Callback().Row().Before("gorm:row").Register("count_ranking_log_rows", func(tx *gorm.DB) {
		if tx.Statement.Table == "logs" {
			scans++
		}
	}))
	require.NoError(t, runRankingCycle(db, runAt))
	require.Equal(t, 0, scans)
	for i := 0; i < 100; i++ {
		require.Same(t, cached, GetCachedRanking())
	}
	require.Equal(t, 0, scans)
	// 迟到的一条日志在下一天复核，覆盖而非重复累加。
	require.NoError(t, db.Create(&Log{CreatedAt: day + 7200, Type: LogTypeConsume, RankingModelName: "gpt-4.1", RankingTokens: &tokens}).Error)
	require.NoError(t, runRankingCycle(db, runAt+rankingDay))
	var total int64
	require.NoError(t, db.Model(&RankingDaily{}).Select("SUM(tokens)").Where("day_start = ?", day).Scan(&total).Error)
	require.EqualValues(t, 300, total)
	var job RankingJob
	require.NoError(t, db.First(&job, "day_start = ?", day).Error)
	require.True(t, job.Finalized)
	// 清理只能到已复核日期，今天/未完成日期的日志保留。
	_, err := deleteLogsWithRankingGuard(today + 10*rankingDay)
	require.NoError(t, err)
	var count int64
	require.NoError(t, db.Model(&Log{}).Where("created_at >= ?", today).Count(&count).Error)
	require.EqualValues(t, 1, count)
	require.Error(t, RequestRankingRebuild(day))
}

func TestRankingLeaseFencingAndAtomicRollback(t *testing.T) {
	db := rankingTestDB(t)
	now := time.Now().Unix()
	day := floorRankingDay(now) - rankingDay
	require.NoError(t, db.Create(&RankingState{ID: 1, CoverageStart: day, NextDay: day}).Error)
	state, err := acquireRankingLease(db, now)
	require.NoError(t, err)
	_, err = acquireRankingLease(db, now)
	require.ErrorIs(t, err, errRankingLease)
	require.NoError(t, db.Create(&RankingDaily{DayStart: day, ModelName: "gpt-4.1", Tokens: 99}).Error)
	// 模拟任务状态写入失败，已删除的旧日结果必须回滚。
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("fail_ranking_job", func(tx *gorm.DB) {
		if tx.Statement.Table == "ranking_jobs" {
			tx.AddError(errors.New("injected job failure"))
		}
	}))
	require.Error(t, aggregateRankingDay(db, state, day, false))
	var row RankingDaily
	require.NoError(t, db.First(&row).Error)
	require.EqualValues(t, 99, row.Tokens)
	require.NoError(t, db.Callback().Create().Remove("fail_ranking_job"))
	require.NoError(t, db.Model(&RankingState{}).Where("id = 1").Update("generation", state.Generation+1).Error)
	require.ErrorIs(t, aggregateRankingDay(db, state, day, false), errRankingLease)
}

func TestRankingEmptyDayAndPublishRetry(t *testing.T) {
	db := rankingTestDB(t)
	now := time.Now().Unix()
	today := floorRankingDay(now)
	day := today - rankingDay
	require.NoError(t, db.Create(&RankingState{ID: 1, CoverageStart: day, NextDay: day}).Error)
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("fail_ranking_snapshot", func(tx *gorm.DB) {
		if tx.Statement.Table == "ranking_snapshots" {
			tx.AddError(errors.New("injected publish failure"))
		}
	}))
	require.Error(t, runRankingCycle(db, today+3600))
	var state RankingState
	require.NoError(t, db.First(&state, 1).Error)
	require.Equal(t, today, state.NextDay)
	require.True(t, state.SnapshotDirty)
	require.NoError(t, db.Callback().Create().Remove("fail_ranking_snapshot"))
	require.NoError(t, runRankingCycle(db, today+3600))
	require.NoError(t, RefreshRankingCache())
	require.NotNil(t, GetCachedRanking())
	var count int64
	require.NoError(t, db.Model(&RankingJob{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func TestRankingLoggingGapInvalidatesWorker(t *testing.T) {
	db := rankingTestDB(t)
	now := time.Now().Unix()
	day := floorRankingDay(now) - rankingDay
	require.NoError(t, db.Create(&RankingState{ID: 1, CoverageStart: day, NextDay: day}).Error)
	state, err := acquireRankingLease(db, now)
	require.NoError(t, err)
	require.NoError(t, resetRankingCoverage())
	require.ErrorIs(t, rankingFence(db, state), errRankingLease)
	var updated RankingState
	require.NoError(t, db.First(&updated, 1).Error)
	require.Equal(t, floorRankingDay(now)+rankingDay, updated.CoverageStart)
	require.Equal(t, updated.CoverageStart, updated.NextDay)
}
