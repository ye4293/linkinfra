package model

import (
	"context"
	"github.com/songquanpeng/one-api/common/config"
	"time"

	"gorm.io/gorm"
)

func deleteLogsWithMetricsGuard(target int64) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db := LOG_DB.WithContext(ctx)
	var existing MetricsV2State
	if err := db.Where("id = 1").Find(&existing).Error; err != nil {
		return 0, err
	}
	if existing.ID == 0 {
		return deleteLogsWithoutMetricsGuard(target)
	}
	s, err := acquireMetricsLease(db)
	if err != nil {
		return 0, err
	}
	defer releaseMetricsLease(LOG_DB, s)
	if target > s.FinalizedBefore {
		target = s.FinalizedBefore
	}
	// 租约在5秒有界删除期间仍有效，rankings 继续执行其自身水位检查。
	if err = metricsFence(db, s); err != nil {
		return 0, err
	}
	var ranking RankingState
	if db.Migrator().HasTable(&RankingState{}) {
		if err := db.Select("id").Where("id = 1").Find(&ranking).Error; err != nil {
			return 0, err
		}
	}
	if config.RankingsEnabled || ranking.ID != 0 {
		return deleteLogsWithoutMetricsGuard(target)
	}
	// 没有排名任务时也使用带明确时间条件的小批删除，不依赖时间/ID单调假设。
	var count int64
	for batch := 0; batch < 20; batch++ {
		var deleted int64
		err := db.Transaction(func(tx *gorm.DB) error {
			if err := metricsFence(tx, s); err != nil {
				return err
			}
			var ids []int64
			if err := tx.Model(&Log{}).Where("created_at < ?", target).Order("created_at, id").Limit(500).Pluck("id", &ids).Error; err != nil {
				return err
			}
			if len(ids) == 0 {
				return nil
			}
			r := tx.Where("id IN ? AND created_at < ?", ids, target).Delete(&Log{})
			deleted = r.RowsAffected
			return r.Error
		})
		if err != nil {
			return count, err
		}
		count += deleted
		if deleted < 500 {
			break
		}
	}
	return count, nil
}

// 重建只允许原始日志仍完整且未封存的最近桶；管理入口不直接扫描日志。
func RebuildMetricsV2Bucket(db *gorm.DB, start int64) error {
	s, err := acquireMetricsLease(db)
	if err != nil {
		return err
	}
	defer releaseMetricsLease(db, s)
	if start%300 != 0 || start < s.CoverageStart || start < s.FinalizedBefore || start >= s.NextBucket {
		return gorm.ErrRecordNotFound
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := metricsFence(tx, s); err != nil {
			return err
		}
		if err := tx.Where("start = ?", start).Delete(&MetricsV2Stage{}).Error; err != nil {
			return err
		}
		return tx.Model(&MetricsV2Job{}).Where("start = ?", start).Updates(map[string]interface{}{"cursor_time": 0, "cursor_id": 0, "scanning": false, "next_run": 0, "pass": 0, "finalized": false}).Error
	})
}
