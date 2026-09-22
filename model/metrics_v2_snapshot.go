package model

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"
)

// 按互不重叠的日/小时/5m片段读取，30天最多读取约120行而非8640个细桶。
func metricsRange(db *gorm.DB, model, source string, start, end int64) (metricsSums, error) {
	var total metricsSums
	for start < end {
		resolution := int64(300)
		if start%86400 == 0 && end-start >= 86400 {
			resolution = 86400
		} else if start%3600 == 0 && end-start >= 3600 {
			resolution = 3600
		}
		next := start + resolution
		for next < end && next+resolution <= end {
			if resolution == 300 && next%3600 == 0 {
				break
			}
			if resolution == 3600 && next%86400 == 0 {
				break
			}
			next += resolution
		}
		var rows []MetricsV2Bucket
		if err := db.Select("payload").Where("model = ? AND source = ? AND channel = 0 AND resolution = ? AND start >= ? AND start < ?", model, source, resolution, start, next).Limit(301).Find(&rows).Error; err != nil {
			return total, err
		}
		if len(rows) > 300 {
			return total, errors.New("metrics range exceeded budget")
		}
		for _, row := range rows {
			v, err := decodeMetrics(row.Payload)
			if err != nil {
				return total, err
			}
			total.add(v, 1)
		}
		start = next
	}
	return total, nil
}

func buildMetricsSnapshot(db *gorm.DB, s *MetricsV2State, snapshot MetricsV2Snapshot) (MetricsV2Data, error) {
	data := MetricsV2Data{SchemaVersion: 2, Model: snapshot.Model, Source: snapshot.Source, AsOf: s.NextBucket, CoverageStart: s.CoverageStart, Version: snapshot.Version + 1, Periods: map[string]MetricsV2Period{}}
	// 一次按来源读取有界汇总，四档窗口共用，避免每个点一次SQL。
	var rows []MetricsV2Bucket
	if err := db.Select("resolution, start, payload").Where("model = ? AND source = ? AND channel = 0 AND start >= ? AND start < ?", snapshot.Model, snapshot.Source, s.NextBucket-31*86400, s.NextBucket).
		Where("resolution >= 3600 OR start >= ? OR (start >= ? AND start < ?) OR (start >= ? AND start < ?) OR (start >= ? AND start < ?)", s.NextBucket-86400-3600, s.NextBucket-7*86400-3600, s.NextBucket-7*86400+3600, s.NextBucket-30*86400-3600, s.NextBucket-30*86400+3600, s.CoverageStart, s.CoverageStart+3600).
		Limit(1501).Find(&rows).Error; err != nil {
		return data, err
	}
	if len(rows) > 1500 {
		return data, errors.New("source snapshot exceeds row budget")
	}
	lookup := map[[2]int64]metricsSums{}
	for _, row := range rows {
		v, err := decodeMetrics(row.Payload)
		if err != nil {
			return data, err
		}
		lookup[[2]int64{row.Resolution, row.Start}] = v
	}
	rangeSum := func(start, end int64) metricsSums {
		var sum metricsSums
		for start < end {
			resolution := int64(300)
			if start%86400 == 0 && end-start >= 86400 {
				resolution = 86400
			} else if start%3600 == 0 && end-start >= 3600 {
				resolution = 3600
			}
			sum.add(lookup[[2]int64{resolution, start}], 1)
			start += resolution
		}
		return sum
	}
	for _, window := range []struct {
		name          string
		seconds, step int64
	}{{"1h", 3600, 300}, {"24h", 86400, 3600}, {"7d", 7 * 86400, 86400}, {"30d", 30 * 86400, 86400}} {
		start := s.NextBucket - window.seconds
		period := MetricsV2Period{Start: start, End: s.NextBucket, Partial: start < s.CoverageStart, Points: []MetricsV2Point{}}
		var total metricsSums
		for pos := start; pos < s.NextBucket; {
			end := pos - pos%window.step + window.step
			if end > s.NextBucket {
				end = s.NextBucket
			}
			from := pos
			if from < s.CoverageStart {
				from = s.CoverageStart
			}
			var sum metricsSums
			if from < end {
				sum = rangeSum(from, end)
			}
			total.add(sum, 1)
			period.Points = append(period.Points, MetricsV2Point{Timestamp: pos, End: end, MetricsV2Stats: statsForMetrics(sum, end-pos)})
			pos = end
		}
		coveredStart := start
		if coveredStart < s.CoverageStart {
			coveredStart = s.CoverageStart
		}
		period.Summary = statsForMetrics(total, s.NextBucket-coveredStart)
		data.Periods[window.name] = period
	}
	return data, nil
}

func publishMetricsSnapshots(db *gorm.DB, s *MetricsV2State) error {
	var snapshots []MetricsV2Snapshot
	if err := db.Where("dirty = ?", true).Order("as_of, id").Limit(64).Find(&snapshots).Error; err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		ctx, cancel := context.WithTimeout(db.Statement.Context, 5*time.Second)
		err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := metricsFence(tx, s); err != nil {
				return err
			}
			data, err := buildMetricsSnapshot(tx, s, snapshot)
			if err != nil {
				return err
			}
			mini := data.Periods["24h"].Summary
			return tx.Model(&MetricsV2Snapshot{}).Where("id = ?", snapshot.ID).Updates(map[string]interface{}{"version": data.Version, "as_of": data.AsOf, "payload": metricsJSON(data), "mini": metricsJSON(mini), "dirty": false}).Error
		})
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}

type metricsCacheEntry struct {
	data    *MetricsV2Data
	expires time.Time
	bytes   int
}

var sourceMetricsCache = struct {
	sync.Mutex
	entries map[string]metricsCacheEntry
	bytes   int
}{entries: map[string]metricsCacheEntry{}}
var sourceMetricsFlight singleflight.Group
var sourceMetricsReads = make(chan struct{}, 4)

// HTTP 路径只允许读取预生成快照；缓存缺失绝不扫描 logs 或触发聚合。
func GetMetricsV2Snapshot(ctx context.Context, model, source string) (*MetricsV2Data, error) {
	key := MetricsIdentity(model, source)
	sourceMetricsCache.Lock()
	entry, ok := sourceMetricsCache.entries[key]
	sourceMetricsCache.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return entry.data, nil
	}
	result, err, _ := sourceMetricsFlight.Do(key, func() (interface{}, error) {
		select {
		case sourceMetricsReads <- struct{}{}:
			defer func() { <-sourceMetricsReads }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var row MetricsV2Snapshot
		query := LOG_DB.WithContext(readCtx).Select("payload, version").Where("model = ? AND source = ?", model, MetricsSourceKey(source))
		if entry.data != nil {
			query = query.Where("version <> ?", entry.data.Version)
		}
		err := query.First(&row).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			if entry.data != nil {
				sourceMetricsCache.Lock()
				entry.expires = time.Now().Add(10 * time.Second)
				sourceMetricsCache.entries[key] = entry
				sourceMetricsCache.Unlock()
				return entry.data, nil
			}
			return nil, err
		}
		data := entry.data
		bytes := entry.bytes
		if row.Payload != "" {
			var parsed MetricsV2Data
			if err = json.Unmarshal([]byte(row.Payload), &parsed); err != nil {
				return nil, err
			}
			data = &parsed
			bytes = len(row.Payload)
		}
		sourceMetricsCache.Lock()
		defer sourceMetricsCache.Unlock()
		if len(sourceMetricsCache.entries) >= 2000 || sourceMetricsCache.bytes+bytes > 64*1024*1024 {
			// 有界批次淘汰；缓存只是加速，持久化快照仍为读取依据。
			for k, v := range sourceMetricsCache.entries {
				delete(sourceMetricsCache.entries, k)
				sourceMetricsCache.bytes -= v.bytes
				if len(sourceMetricsCache.entries) < 1500 && sourceMetricsCache.bytes+bytes < 48*1024*1024 {
					break
				}
			}
		}
		if previous, ok := sourceMetricsCache.entries[key]; ok {
			sourceMetricsCache.bytes -= previous.bytes
		}
		sourceMetricsCache.entries[key] = metricsCacheEntry{data: data, expires: time.Now().Add(30 * time.Second), bytes: bytes}
		sourceMetricsCache.bytes += bytes
		return data, nil
	})
	if result == nil {
		return nil, err
	}
	return result.(*MetricsV2Data), err
}

type MetricsV2Mini struct {
	Model  string `json:"model_name"`
	Source string `json:"source_key"`
	AsOf   int64  `json:"as_of"`
	MetricsV2Stats
}

var metricsMiniCache = struct {
	sync.Mutex
	entries []MetricsV2Mini
	expires time.Time
}{}

func GetMetricsV2Mini(ctx context.Context) ([]MetricsV2Mini, error) {
	metricsMiniCache.Lock()
	defer metricsMiniCache.Unlock()
	if time.Now().Before(metricsMiniCache.expires) {
		return metricsMiniCache.entries, nil
	}
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var rows []MetricsV2Snapshot
	err := LOG_DB.WithContext(readCtx).Select("model, source, as_of, mini").Where("as_of > ?", time.Now().Unix()-35*86400).Order("id").Limit(10001).Find(&rows).Error
	if err != nil {
		if metricsMiniCache.entries != nil {
			metricsMiniCache.expires = time.Now().Add(10 * time.Second)
			return metricsMiniCache.entries, nil
		}
		return nil, err
	}
	if len(rows) > 10000 {
		return nil, errors.New("metrics catalog exceeds snapshot budget")
	}
	items := make([]MetricsV2Mini, 0, len(rows))
	for _, row := range rows {
		var stat MetricsV2Stats
		if err = json.Unmarshal([]byte(row.Mini), &stat); err != nil {
			return nil, err
		}
		items = append(items, MetricsV2Mini{Model: row.Model, Source: row.Source, AsOf: row.AsOf, MetricsV2Stats: stat})
	}
	metricsMiniCache.entries, metricsMiniCache.expires = items, time.Now().Add(30*time.Second)
	return items, nil
}

// 管理员下钻仅查有索引的小时聚合，限制范围和行数，不读取原始日志。
func GetMetricsV2Channels(ctx context.Context, model, source string) ([]ChannelMetricsSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var rows []MetricsV2Bucket
	err := LOG_DB.WithContext(ctx).Where("model = ? AND source = ? AND channel > 0 AND resolution = 3600 AND start >= ?", model, source, time.Now().Unix()-86400).Limit(2401).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) > 2400 {
		return nil, errors.New("too many channels for metrics detail")
	}
	sums := map[int]metricsSums{}
	for _, row := range rows {
		v, err := decodeMetrics(row.Payload)
		if err != nil {
			return nil, err
		}
		s := sums[row.Channel]
		s.add(v, 1)
		sums[row.Channel] = s
	}
	result := []ChannelMetricsSummary{}
	for channel, s := range sums {
		result = append(result, ChannelMetricsSummary{ChannelId: channel, SuccessRate: safeDiv(float64(s.Success), s.Success+s.Errors), AvgLatency: safeDiv(s.Duration, s.DurationCount), AvgSpeed: safeDiv(s.Speed, s.SpeedCount), TotalRequests24h: s.Attempts})
	}
	return result, nil
}
