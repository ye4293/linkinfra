package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var errRankingLease = errors.New("ranking worker lease lost or busy")

// 管理员关闭消费日志前持久化覆盖断点。更新 generation 会让在途 worker 停止发布。
func resetRankingCoverage() error {
	start := floorRankingDay(time.Now().Unix()) + rankingDay
	return LOG_DB.Model(&RankingState{}).Where("id = 1").Updates(map[string]interface{}{
		"coverage_start": start, "next_day": start, "generation": gorm.Expr("generation + 1"), "lease_until": 0, "lease_owner": "",
	}).Error
}

func acquireRankingLease(db *gorm.DB, now int64) (*RankingState, error) {
	// 首次启用只统计下一个完整自然日，避免将上线前的空字段误当零用量。
	start := floorRankingDay(now) + rankingDay
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&RankingState{ID: 1, CoverageStart: start, NextDay: start}).Error; err != nil {
		return nil, err
	}
	owner := helper.GetUUID()
	leaseNow := time.Now().Unix()
	r := db.Model(&RankingState{}).Where("id = ? AND lease_until <= ?", 1, leaseNow).Updates(map[string]interface{}{"lease_owner": owner, "lease_until": leaseNow + 600, "generation": gorm.Expr("generation + 1")})
	if r.Error != nil {
		return nil, r.Error
	}
	if r.RowsAffected != 1 {
		return nil, errRankingLease
	}
	var state RankingState
	if err := db.First(&state, 1).Error; err != nil {
		return nil, err
	}
	if state.LeaseOwner != owner {
		return nil, errRankingLease
	}
	return &state, nil
}

func rankingFence(db *gorm.DB, state *RankingState) error {
	now := time.Now().Unix()
	r := db.Model(&RankingState{}).Where("id = 1 AND lease_owner = ? AND generation = ? AND lease_until > ?", state.LeaseOwner, state.Generation, now).Updates(map[string]interface{}{"lease_until": now + 600, "heartbeat": gorm.Expr("heartbeat + 1")})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return errRankingLease
	}
	return nil
}

func releaseRankingLease(db *gorm.DB, state *RankingState) {
	db.Model(&RankingState{}).Where("id = 1 AND lease_owner = ? AND generation = ?", state.LeaseOwner, state.Generation).Updates(map[string]interface{}{"lease_owner": "", "lease_until": 0})
}

// aggregateRankingDay 每小时分组一次，只读排名必需列；数据库按时间索引范围访问。
// 一天的所有结果发布成功前不推进游标，重试覆盖当天结果而不是再次累加。
func aggregateRankingDay(db *gorm.DB, state *RankingState, day int64, finalized bool) error {
	type key struct{ model, provider string }
	sums := map[key]*RankingDaily{}
	var excluded int64
	for hour := day; hour < day+rankingDay; hour += 3600 {
		if err := rankingFence(db, state); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		var rows []struct {
			ModelName string
			Provider  string
			Tokens    int64
			Requests  int64
		}
		err := db.WithContext(ctx).Model(&Log{}).
			Select("ranking_model_name AS model_name, provider, SUM(ranking_tokens) AS tokens, COUNT(*) AS requests").
			Where("created_at >= ? AND created_at < ? AND type = ? AND ranking_tokens IS NOT NULL", hour, hour+3600, LogTypeConsume).
			Group("ranking_model_name, provider").Scan(&rows).Error
		cancel()
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.ModelName == "" {
				excluded += row.Requests
				continue
			}
			k := key{row.ModelName, row.Provider}
			if sums[k] == nil {
				sums[k] = &RankingDaily{DayStart: day, ModelName: row.ModelName, Provider: row.Provider}
			}
			sums[k].Tokens += row.Tokens
			sums[k].Requests += row.Requests
		}
		// 固定后台预算，避免 24 个区间连续抢占数据库连接。
		if config.RankingBatchPauseMS > 0 {
			time.Sleep(time.Duration(config.RankingBatchPauseMS) * time.Millisecond)
		}
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := rankingFence(tx, state); err != nil {
			return err
		}
		if err := tx.Where("day_start = ?", day).Delete(&RankingDaily{}).Error; err != nil {
			return err
		}
		rows := make([]RankingDaily, 0, len(sums))
		for _, row := range sums {
			rows = append(rows, *row)
		}
		if len(rows) > 0 {
			if err := tx.CreateInBatches(rows, 100).Error; err != nil {
				return err
			}
		}
		job := RankingJob{DayStart: day, CompletedAt: time.Now().Unix(), Finalized: finalized, ExcludedRequests: excluded}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "day_start"}}, DoUpdates: clause.AssignmentColumns([]string{"completed_at", "finalized", "excluded_requests"})}).Create(&job).Error; err != nil {
			return err
		}
		if day == state.NextDay {
			if err := tx.Model(&RankingState{}).Where("id = 1").Update("next_day", day+rankingDay).Error; err != nil {
				return err
			}
		}
		return tx.Model(&RankingState{}).Where("id = 1").Update("snapshot_dirty", true).Error
	})
}

func publishRankingSnapshot(db *gorm.DB, state *RankingState) error {
	if state.NextDay <= state.CoverageStart {
		return nil
	}
	var rows []RankingDaily
	if err := db.Where("day_start >= ? AND day_start < ?", state.NextDay-60*rankingDay, state.NextDay).Find(&rows).Error; err != nil {
		return err
	}
	data := buildRankingData(rows, state.CoverageStart, state.NextDay, time.Now().Unix())
	data.Version = helper.GetUUID()
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := rankingFence(tx, state); err != nil {
			return err
		}
		if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&RankingSnapshot{ID: 1, Payload: string(payload), Version: data.Version}).Error; err != nil {
			return err
		}
		return tx.Model(&RankingState{}).Where("id = 1").Update("snapshot_dirty", false).Error
	})
}

func runRankingCycle(db *gorm.DB, now int64) error {
	state, err := acquireRankingLease(db, now)
	if err != nil {
		return err
	}
	defer releaseRankingLease(db, state)
	today := floorRankingDay(now)
	if !config.LogConsumeEnabled {
		// 禁用日志意味着统计缺口。恢复后重新建立完整覆盖区间，旧快照仍可阅读。
		return db.Model(&RankingState{}).Where("id = 1 AND lease_owner = ? AND generation = ?", state.LeaseOwner, state.Generation).
			Updates(map[string]interface{}{"coverage_start": today + rankingDay, "next_day": today + rankingDay}).Error
	}
	if now < today+15*60 {
		return nil
	}
	changed := false
	// 每轮最多补三天；完成的空日也有 job，不用 MAX(日汇总日期) 猜进度。
	for n := 0; state.NextDay < today && n < 3; n++ {
		day := state.NextDay
		if err := aggregateRankingDay(db, state, day, day < today-rankingDay); err != nil {
			return err
		}
		state.NextDay += rankingDay
		changed = true
	}
	// 已发布的最新一天在下一天复核一次，随后允许清理源日志。
	var jobs []RankingJob
	if err := db.Where("day_start >= ? AND day_start < ? AND finalized = ?", state.CoverageStart, today-rankingDay, false).Order("day_start").Limit(3).Find(&jobs).Error; err != nil {
		return err
	}
	for _, job := range jobs {
		if err := aggregateRankingDay(db, state, job.DayStart, true); err != nil {
			return err
		}
		changed = true
	}
	// 发布失败时下轮即使没有新日也要重试，不能留下永久过期快照。
	var snapshot RankingSnapshot
	err = db.First(&snapshot, 1).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	var prior RankingData
	if snapshot.Payload != "" {
		_ = json.Unmarshal([]byte(snapshot.Payload), &prior)
	}
	if changed || state.SnapshotDirty || prior.DataThrough != rankingDate(state.NextDay-rankingDay) {
		if err := publishRankingSnapshot(db, state); err != nil {
			return err
		}
	}
	if !changed {
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := rankingFence(tx, state); err != nil {
			return err
		}
		cutoff := state.NextDay - 90*rankingDay
		if err := tx.Where("day_start < ?", cutoff).Delete(&RankingDaily{}).Error; err != nil {
			return err
		}
		return tx.Where("day_start < ? AND finalized = ?", cutoff, true).Delete(&RankingJob{}).Error
	})
}

// StartRankingWorker 每个节点只加载一条快照；日汇总由 SQL 租约选出的单节点执行。
func StartRankingWorker() {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.SysError(fmt.Sprintf("rankings: %v", r))
				}
			}()
			if config.RankingsEnabled {
				if err := runRankingCycle(LOG_DB, time.Now().Unix()); err != nil && !errors.Is(err, errRankingLease) {
					logger.SysError("rankings: " + err.Error())
				}
				if err := RefreshRankingCache(); err != nil {
					logger.SysError("ranking snapshot: " + err.Error())
				}
			}
		}()
		time.Sleep(time.Minute)
	}
}

// RequestRankingRebuild 只标记明确日期，实际重算仍由受租约保护的 worker 完成。
func RequestRankingRebuild(day int64) error {
	state, err := acquireRankingLease(LOG_DB, time.Now().Unix())
	if err != nil {
		return err
	}
	defer releaseRankingLease(LOG_DB, state)
	if day%rankingDay != 0 || day < state.CoverageStart || day >= state.NextDay || day < state.LogPurgedBefore || day < time.Now().Unix()-89*rankingDay {
		return errors.New("date outside retained ranking logs")
	}
	return LOG_DB.Model(&RankingJob{}).Where("day_start = ?", day).Update("finalized", false).Error
}

func deleteLogsWithRankingGuard(target int64) (int64, error) {
	state, err := acquireRankingLease(LOG_DB, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	defer releaseRankingLease(LOG_DB, state)
	cutoff := state.NextDay
	var pending RankingJob
	err = LOG_DB.Where("day_start >= ? AND finalized = ?", state.CoverageStart, false).Order("day_start").First(&pending).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, err
	}
	if err == nil && pending.DayStart < cutoff {
		cutoff = pending.DayStart
	}
	if target < cutoff {
		cutoff = target
	}
	var count int64
	err = LOG_DB.Transaction(func(tx *gorm.DB) error {
		if err := rankingFence(tx, state); err != nil {
			return err
		}
		r := tx.Where("created_at < ?", cutoff).Delete(&Log{})
		if r.Error != nil {
			return r.Error
		}
		count = r.RowsAffected
		if cutoff > state.LogPurgedBefore {
			return tx.Model(&RankingState{}).Where("id = 1").Update("log_purged_before", cutoff).Error
		}
		return nil
	})
	return count, err
}
