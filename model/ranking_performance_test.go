package model

import (
	"strings"
	"testing"
	"time"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRankingIdleCycleDoesNotWriteOrReadPayload(t *testing.T) {
	db := rankingTestDB(t)
	today := floorRankingDay(time.Now().Unix())
	require.NoError(t, db.Create(&RankingState{ID: 1, CoverageStart: today - 2*rankingDay, NextDay: today}).Error)
	writes := 0
	track := func(tx *gorm.DB) { writes++ }
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("idle_create", track))
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("idle_update", track))
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register("idle_delete", track))
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("idle_query", func(tx *gorm.DB) {
		require.NotEqual(t, "ranking_snapshots", tx.Statement.Table)
		require.NotEqual(t, "logs", tx.Statement.Table)
	}))
	for i := 0; i < 3; i++ {
		require.NoError(t, runRankingCycle(db, today+3600))
	}
	require.Zero(t, writes)
}

func TestRankingCacheFiltersUnchangedPayloadInSQL(t *testing.T) {
	db := rankingTestDB(t)
	require.NoError(t, db.Create(&RankingSnapshot{ID: 1, Version: "one", Payload: `{"version":"one"}`}).Error)
	require.NoError(t, RefreshRankingCache())
	original := GetCachedRanking()
	var fetched int64
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("snapshot_rows", func(tx *gorm.DB) {
		if tx.Statement.Table == "ranking_snapshots" {
			fetched += tx.RowsAffected
		}
	}))
	require.NoError(t, RefreshRankingCache())
	require.Zero(t, fetched)
	require.Same(t, original, GetCachedRanking())
	require.NoError(t, db.Model(&RankingSnapshot{}).Where("id = 1").Updates(map[string]interface{}{"version": "two", "payload": `{"version":"two"}`}).Error)
	require.NoError(t, RefreshRankingCache())
	require.EqualValues(t, 1, fetched)
	require.Equal(t, `"two"`, GetCachedRanking().ETag)
}

func TestRankingCleanupIsBoundedAndProtectsDisabledBacklog(t *testing.T) {
	db := rankingTestDB(t)
	today := floorRankingDay(time.Now().Unix())
	old := today - 3*rankingDay
	require.NoError(t, db.Create(&RankingState{ID: 1, CoverageStart: old, NextDay: today - rankingDay}).Error)
	rows := make([]Log, 12001)
	for i := range rows {
		rows[i] = Log{CreatedAt: old, Type: LogTypeConsume}
	}
	require.NoError(t, db.Select("created_at", "type").CreateInBatches(rows, 100).Error)
	require.NoError(t, db.Create(&Log{CreatedAt: today - rankingDay + 1, Type: LogTypeConsume}).Error)
	config.RankingsEnabled = false
	deletes := 0
	require.NoError(t, db.Callback().Delete().After("gorm:delete").Register("bounded_delete", func(tx *gorm.DB) {
		if tx.Statement.Table == "logs" {
			require.LessOrEqual(t, tx.RowsAffected, int64(500))
			deletes++
		}
	}))
	n, err := DeleteOldLog(today + rankingDay)
	require.NoError(t, err)
	require.Positive(t, n)
	require.LessOrEqual(t, n, int64(10000))
	require.LessOrEqual(t, deletes, 20)
	var remaining int64
	require.NoError(t, db.Model(&Log{}).Where("created_at >= ?", today-rankingDay).Count(&remaining).Error)
	require.EqualValues(t, 1, remaining)
	var state RankingState
	require.NoError(t, db.First(&state, 1).Error)
	require.Equal(t, today-rankingDay, state.LogPurgedBefore)
}

func TestRankingIndexAndPreparedPredicate(t *testing.T) {
	db := rankingTestDB(t)
	require.NoError(t, EnsureRankingLogIndex(db))
	require.NoError(t, EnsureRankingLogIndex(db))
	// 有代表性的统计信息才能验证优化器选择；空表计划可能优先选旧 type 索引。
	tokens := int64(10)
	rows := make([]Log, 2000)
	for i := range rows {
		rows[i] = Log{CreatedAt: int64(i * 100), Type: LogTypeConsume, RankingModelName: "gpt-4.1", RankingTokens: &tokens}
	}
	require.NoError(t, db.Select("created_at", "type", "ranking_model_name", "ranking_tokens").CreateInBatches(rows, 100).Error)
	require.NoError(t, db.Exec("ANALYZE").Error)
	query := db.Session(&gorm.Session{DryRun: true}).Model(&Log{}).Select("ranking_model_name, provider, SUM(ranking_tokens), COUNT(*)").Where("created_at >= ? AND created_at < ?", 100, 200).Where(rankingLogPredicate).Group("ranking_model_name, provider").Find(&[]Log{})
	require.Contains(t, query.Statement.SQL.String(), "type = 2 AND ranking_tokens IS NOT NULL")
	require.Len(t, query.Statement.Vars, 2, "only timestamps are bound; generic PG plans must prove partial-index eligibility")
	var plan []struct{ Detail string }
	require.NoError(t, db.Raw("EXPLAIN QUERY PLAN "+query.Statement.SQL.String(), query.Statement.Vars...).Scan(&plan).Error)
	var details []string
	for _, row := range plan {
		details = append(details, row.Detail)
	}
	require.True(t, db.Migrator().HasIndex(&Log{}, rankingLogIndex))
	// SQLite 可选择原来的窄时间索引；PG 部分覆盖索引的实际命中另用 PG 引擎验证。
	require.Contains(t, strings.Join(details, "\n"), "USING INDEX")
	require.Contains(t, strings.Join(details, "\n"), "created_at>?")
	require.Contains(t, rankingLogIndexSQL("postgres"), "INCLUDE (ranking_model_name, provider, ranking_tokens)")
}
