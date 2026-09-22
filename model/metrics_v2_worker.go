package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var errMetricsLease = errors.New("metrics lease lost or busy")

func MigrateMetricsV2(db *gorm.DB) error {
	return db.AutoMigrate(&MetricsV2State{}, &MetricsV2Job{}, &MetricsV2Stage{}, &MetricsV2Bucket{}, &MetricsV2Snapshot{})
}

// 大日志表索引只能由独立迁移建立；自动扩容节点不能启动时争抢 DDL。
func verifyMetricsIndex(db *gorm.DB) error {
	if db.Dialector.Name() != "postgres" {
		return nil
	}
	var valid bool
	err := db.Raw(`SELECT EXISTS(SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid WHERE i.indrelid='logs'::regclass AND c.relname='idx_logs_metrics_v2_scan' AND i.indisvalid AND i.indisready AND pg_get_indexdef(i.indexrelid) LIKE '%(created_at, id)%' AND pg_get_expr(i.indpred,i.indrelid) LIKE '%metrics_version = 2%')`).Scan(&valid).Error
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("metrics v2 requires valid idx_logs_metrics_v2_scan; run reviewed concurrent index migration first")
	}
	return nil
}
func metricsDBNow(db *gorm.DB) (int64, error) {
	expr := "CAST(strftime('%s','now') AS INTEGER)"
	switch db.Dialector.Name() {
	case "postgres":
		expr = "CAST(EXTRACT(EPOCH FROM clock_timestamp()) AS BIGINT)"
	case "mysql":
		expr = "UNIX_TIMESTAMP()"
	}
	var now int64
	err := db.Raw("SELECT " + expr).Scan(&now).Error
	return now, err
}
func metricsFence(db *gorm.DB, state *MetricsV2State) error {
	now, err := metricsDBNow(db)
	if err != nil {
		return err
	}
	r := db.Model(&MetricsV2State{}).Where("id = 1 AND owner = ? AND generation = ? AND lease_until > ?", state.Owner, state.Generation, now).Updates(map[string]interface{}{"lease_until": now + 60, "heartbeat": gorm.Expr("heartbeat + 1")})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return errMetricsLease
	}
	return nil
}
func acquireMetricsLease(db *gorm.DB) (*MetricsV2State, error) {
	now, err := metricsDBNow(db)
	if err != nil {
		return nil, err
	}
	start := now - now%300 + 300
	if err = db.Clauses(clause.OnConflict{DoNothing: true}).Create(&MetricsV2State{ID: 1, CoverageStart: start, NextBucket: start, FinalizedBefore: start}).Error; err != nil {
		return nil, err
	}
	owner := helper.GetUUID()
	r := db.Model(&MetricsV2State{}).Where("id = 1 AND lease_until <= ?", now).Updates(map[string]interface{}{"owner": owner, "lease_until": now + 60, "generation": gorm.Expr("generation + 1")})
	if r.Error != nil {
		return nil, r.Error
	}
	if r.RowsAffected != 1 {
		return nil, errMetricsLease
	}
	var s MetricsV2State
	err = db.First(&s, 1).Error
	if err == nil && s.Owner != owner {
		err = errMetricsLease
	}
	return &s, err
}
func releaseMetricsLease(db *gorm.DB, s *MetricsV2State) {
	db.Model(&MetricsV2State{}).Where("id = 1 AND owner = ? AND generation = ?", s.Owner, s.Generation).Updates(map[string]interface{}{"owner": "", "lease_until": 0})
}

type metricsGroup struct {
	Model, Source string
	Channel       int
}

func scanMetricsPage(db *gorm.DB, s *MetricsV2State, job *MetricsV2Job) (bool, error) {
	ctx, cancel := context.WithTimeout(db.Statement.Context, 3*time.Second)
	defer cancel()
	var logs []Log
	err := db.WithContext(ctx).Select("id, created_at, model_name, metrics_model_name, metrics_attempts").Where(metricsPredicate).Where("created_at >= ? AND created_at < ?", job.Start, job.Start+300).
		Where("(created_at, id) > (?, ?)", job.CursorTime, job.CursorID).Order("created_at, id").Limit(100).Find(&logs).Error
	if err != nil {
		return false, err
	}
	if len(logs) == 0 {
		return true, nil
	}
	sums := map[metricsGroup]metricsSums{}
	for _, log := range logs {
		if log.MetricsModelName != "" {
			log.ModelName = log.MetricsModelName
		}
		attempts, err := validateMetricsAttempts(log.MetricsAttempts)
		if err != nil {
			return false, fmt.Errorf("invalid metrics log %d: %w", log.Id, err)
		}
		if log.ModelName == "" || len(log.ModelName) > 200 {
			return false, fmt.Errorf("invalid model identity in log %d", log.Id)
		}
		for i, a := range attempts {
			for _, channel := range []int{0, a.Channel} {
				k := metricsGroup{log.ModelName, a.Source, channel}
				v := sums[k]
				v.record(a, i == len(attempts)-1)
				sums[k] = v
			}
		}
	}
	if len(sums) > 2000 {
		return false, errors.New("metrics page cardinality exceeds budget")
	}
	last := logs[len(logs)-1]
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := metricsFence(tx, s); err != nil {
			return err
		}
		var existing []MetricsV2Stage
		if err := tx.Where("start = ?", job.Start).Limit(2001).Find(&existing).Error; err != nil {
			return err
		}
		if len(existing) > 2000 {
			return errors.New("metrics stage cardinality exceeds budget")
		}
		prior := map[metricsGroup]MetricsV2Stage{}
		for _, row := range existing {
			prior[metricsGroup{row.Model, row.Source, row.Channel}] = row
		}
		rows := make([]MetricsV2Stage, 0, len(sums))
		for k, delta := range sums {
			row, exists := prior[k]
			if exists {
				old, err := decodeMetrics(row.Payload)
				if err != nil {
					return err
				}
				delta.add(old, 1)
			}
			row.Start, row.Model, row.Source, row.Channel, row.Payload = job.Start, k.Model, k.Source, k.Channel, metricsJSON(delta)
			rows = append(rows, row)
			prior[k] = row
		}
		if len(prior) > 2000 {
			return errors.New("metrics stage cardinality exceeds budget")
		}
		if len(rows) > 0 {
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "start"}, {Name: "model"}, {Name: "source"}, {Name: "channel"}}, DoUpdates: clause.AssignmentColumns([]string{"payload"})}).CreateInBatches(rows, 50).Error; err != nil {
				return err
			}
		}
		return tx.Model(&MetricsV2Job{}).Where("start = ?", job.Start).Updates(map[string]interface{}{"cursor_time": last.CreatedAt, "cursor_id": last.Id, "scanning": true}).Error
	})
	if err == nil {
		job.CursorTime, job.CursorID, job.Scanning = last.CreatedAt, int64(last.Id), true
	}
	return false, err
}

// 完整桶在一个有预算的事务内发布；同时用 new-old 修正小时、日汇总，重跑不会累加两遍。
func publishMetricsBucket(db *gorm.DB, s *MetricsV2State, job *MetricsV2Job, now int64) error {
	ctx, cancel := context.WithTimeout(db.Statement.Context, 5*time.Second)
	defer cancel()
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := metricsFence(tx, s); err != nil {
			return err
		}
		var stage []MetricsV2Stage
		if err := tx.Where("start = ?", job.Start).Limit(2001).Find(&stage).Error; err != nil {
			return err
		}
		if len(stage) > 2000 {
			return errors.New("metrics bucket cardinality exceeds budget")
		}
		var previous []MetricsV2Bucket
		if err := tx.Where("resolution = 300 AND start = ?", job.Start).Limit(2001).Find(&previous).Error; err != nil {
			return err
		}
		if len(previous) > 2000 {
			return errors.New("previous bucket exceeds budget")
		}
		deltas := map[metricsGroup]metricsSums{}
		for _, row := range previous {
			v, err := decodeMetrics(row.Payload)
			if err != nil {
				return err
			}
			k := metricsGroup{row.Model, row.Source, row.Channel}
			d := deltas[k]
			d.add(v, -1)
			deltas[k] = d
		}
		for _, row := range stage {
			v, err := decodeMetrics(row.Payload)
			if err != nil {
				return err
			}
			k := metricsGroup{row.Model, row.Source, row.Channel}
			d := deltas[k]
			d.add(v, 1)
			deltas[k] = d
		}
		if err := tx.Where("resolution = 300 AND start = ?", job.Start).Delete(&MetricsV2Bucket{}).Error; err != nil {
			return err
		}
		rows := make([]MetricsV2Bucket, 0, len(stage))
		for _, row := range stage {
			rows = append(rows, MetricsV2Bucket{Resolution: 300, Start: job.Start, Model: row.Model, Source: row.Source, Channel: row.Channel, Payload: row.Payload})
		}
		if len(rows) > 0 {
			if err := tx.CreateInBatches(rows, 50).Error; err != nil {
				return err
			}
		}
		for _, resolution := range []int64{3600, 86400} {
			start := job.Start - job.Start%resolution
			var existing []MetricsV2Bucket
			if err := tx.Where("resolution = ? AND start = ?", resolution, start).Limit(10001).Find(&existing).Error; err != nil {
				return err
			}
			if len(existing) > 10000 {
				return errors.New("metrics rollup cardinality exceeds budget")
			}
			prior := map[metricsGroup]MetricsV2Bucket{}
			for _, row := range existing {
				prior[metricsGroup{row.Model, row.Source, row.Channel}] = row
			}
			rows := make([]MetricsV2Bucket, 0, len(deltas))
			for k, delta := range deltas {
				row, exists := prior[k]
				v := metricsSums{}
				if exists {
					var err error
					v, err = decodeMetrics(row.Payload)
					if err != nil {
						return err
					}
				}
				v.add(delta, 1)
				row.Resolution, row.Start, row.Model, row.Source, row.Channel, row.Payload = resolution, start, k.Model, k.Source, k.Channel, metricsJSON(v)
				rows = append(rows, row)
			}
			if len(rows) > 0 {
				if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "resolution"}, {Name: "start"}, {Name: "model"}, {Name: "source"}, {Name: "channel"}}, DoUpdates: clause.AssignmentColumns([]string{"payload"})}).CreateInBatches(rows, 50).Error; err != nil {
					return err
				}
			}
		}
		snapshots := []MetricsV2Snapshot{}
		for k := range deltas {
			if k.Channel == 0 {
				snapshots = append(snapshots, MetricsV2Snapshot{Model: k.Model, Source: k.Source, Dirty: true})
			}
		}
		if len(snapshots) > 0 {
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "model"}, {Name: "source"}}, DoUpdates: clause.Assignments(map[string]interface{}{"dirty": true})}).CreateInBatches(snapshots, 50).Error; err != nil {
				return err
			}
		}
		pass := job.Pass + 1
		next := job.Start + 3900
		if pass >= 2 {
			next = job.Start - job.Start%86400 + 86400 + 900
			if next < job.Start+4200 {
				next = job.Start + 4200
			}
		}
		if pass >= 3 {
			next = 1 << 62
		}
		if err := tx.Model(&MetricsV2Job{}).Where("start = ?", job.Start).Updates(map[string]interface{}{"pass": pass, "published": true, "finalized": pass >= 3, "next_run": next, "cursor_time": 0, "cursor_id": 0, "scanning": false, "error": ""}).Error; err != nil {
			return err
		}
		if err := tx.Where("start = ?", job.Start).Delete(&MetricsV2Stage{}).Error; err != nil {
			return err
		}
		if job.Start == s.NextBucket {
			if err := tx.Model(&MetricsV2State{}).Where("id = 1").Update("next_bucket", job.Start+300).Error; err != nil {
				return err
			}
			// 时间窗口移动，即使某来源本轮无请求也需更新其零数据窗口。
			if err := tx.Model(&MetricsV2Snapshot{}).Where("as_of < ? AND dirty = ?", job.Start+300, false).Update("dirty", true).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func runMetricsV2Cycle(db *gorm.DB, now int64) error {
	if err := verifyMetricsIndex(db); err != nil {
		return err
	}
	s, err := acquireMetricsLease(db)
	if err != nil {
		return err
	}
	defer releaseMetricsLease(db, s)
	if !config.ModelMetricsEnabled || !config.LogConsumeEnabled {
		return db.Model(&MetricsV2State{}).Where("id = 1 AND owner = ? AND generation = ? AND paused = ?", s.Owner, s.Generation, false).Update("paused", true).Error
	}
	if s.Paused {
		start := now - now%300 + 300
		// 暂停期间覆盖不完整，新的窗口从恢复边界重新累计，不混入旧区间。
		if err := db.Model(&MetricsV2State{}).Where("id = 1 AND owner = ? AND generation = ?", s.Owner, s.Generation).Updates(map[string]interface{}{"paused": false, "coverage_start": start}).Error; err != nil {
			return err
		}
		s.CoverageStart = start
	}
	deadline := time.Now().Add(20 * time.Second)
	for n := 0; n < 200 && time.Now().Before(deadline); n++ {
		var job MetricsV2Job
		// 轮流处理复核和新桶，避免历史积压饿死实时采集。
		if n%2 == 0 {
			err = db.Where("next_run <= ? AND finalized = ? AND start >= ?", now, false, s.FinalizedBefore).Order("next_run, start").First(&job).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		if job.Start == 0 && s.NextBucket+420 <= now {
			job = MetricsV2Job{Start: s.NextBucket, NextRun: s.NextBucket + 420}
			if err = db.Clauses(clause.OnConflict{DoNothing: true}).Create(&job).Error; err != nil {
				return err
			}
			if err = db.First(&job, "start = ?", s.NextBucket).Error; err != nil {
				return err
			}
			if job.NextRun > now {
				break
			}
		}
		if job.Start == 0 {
			break
		}
		done, err := scanMetricsPage(db, s, &job)
		if err != nil {
			_ = db.Transaction(func(tx *gorm.DB) error {
				if fenceErr := metricsFence(tx, s); fenceErr != nil {
					return fenceErr
				}
				return tx.Model(&MetricsV2Job{}).Where("start = ?", job.Start).Updates(map[string]interface{}{"next_run": now + 60, "error": err.Error()}).Error
			})
			return err
		}
		if done {
			if err = publishMetricsBucket(db, s, &job, now); err != nil {
				_ = db.Transaction(func(tx *gorm.DB) error {
					if e := metricsFence(tx, s); e != nil {
						return e
					}
					return tx.Model(&MetricsV2Job{}).Where("start = ?", job.Start).Updates(map[string]interface{}{"next_run": now + 60, "error": err.Error()}).Error
				})
				return err
			}
			if job.Start == s.NextBucket {
				s.NextBucket += 300
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err = advanceMetricsFinalized(db, s); err != nil {
		return err
	}
	if err = publishMetricsSnapshots(db, s); err != nil {
		return err
	}
	return cleanupMetricsV2(db, s, now)
}

func advanceMetricsFinalized(db *gorm.DB, s *MetricsV2State) error {
	var jobs []MetricsV2Job
	if err := db.Where("start >= ?", s.FinalizedBefore).Order("start").Limit(500).Find(&jobs).Error; err != nil {
		return err
	}
	cutoff := s.FinalizedBefore
	for _, j := range jobs {
		if j.Start != cutoff || !j.Finalized {
			break
		}
		cutoff += 300
	}
	if cutoff == s.FinalizedBefore {
		return nil
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := metricsFence(tx, s); err != nil {
			return err
		}
		return tx.Model(&MetricsV2State{}).Where("id = 1").Update("finalized_before", cutoff).Error
	})
	if err == nil {
		s.FinalizedBefore = cutoff
	}
	return err
}
func cleanupMetricsV2(db *gorm.DB, s *MetricsV2State, now int64) error {
	cutoff := now - metricsRetention
	if cutoff > s.FinalizedBefore {
		cutoff = s.FinalizedBefore
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := metricsFence(tx, s); err != nil {
			return err
		}
		var ids []int64
		if err := tx.Model(&MetricsV2Bucket{}).Where("(resolution < 86400 AND start < ?) OR (resolution = 86400 AND start < ?)", cutoff, now-90*86400).Order("start, id").Limit(500).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			// 完成任务也分批回收，保留未完成断点。
			var starts []int64
			if err := tx.Model(&MetricsV2Job{}).Where("start < ? AND finalized = ?", cutoff, true).Order("start").Limit(500).Pluck("start", &starts).Error; err != nil {
				return err
			}
			if len(starts) > 0 {
				return tx.Where("start IN ? AND finalized = ?", starts, true).Delete(&MetricsV2Job{}).Error
			}
			return nil
		}
		return tx.Where("id IN ?", ids).Delete(&MetricsV2Bucket{}).Error
	})
}

func StartMetricsV2Worker() {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.SysError(fmt.Sprint("metrics v2 worker panic: ", r))
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := runMetricsV2Cycle(LOG_DB.WithContext(ctx), time.Now().Unix()); err != nil && !errors.Is(err, errMetricsLease) {
				logger.SysError("metrics v2: " + err.Error())
			}
		}()
		time.Sleep(30 * time.Second)
	}
}
