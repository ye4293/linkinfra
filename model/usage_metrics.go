package model

import (
	"sync"
	"time"
)

const usageMetricsTTL = 10 * time.Second

// UsageMetrics contains only the live counters needed by usage summary cards.
// It intentionally excludes model rankings and other expensive dashboard data.
type UsageMetrics struct {
	RPM         int64 `json:"rpm"`
	TPM         int64 `json:"tpm"`
	TodaySpend  int64 `json:"today_spend"`
	CachedUntil int64 `json:"cached_until"`
}

type usageMetricsCacheEntry struct {
	data      UsageMetrics
	expiresAt time.Time
}

var (
	usageMetricsCache sync.Map
	usageMetricsLocks sync.Map
)

func usageMetricsLock(userId int) *sync.Mutex {
	lock, _ := usageMetricsLocks.LoadOrStore(userId, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func GetUsageMetrics(userId int) (*UsageMetrics, error) {
	if cached, ok := usageMetricsCache.Load(userId); ok {
		entry := cached.(usageMetricsCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			result := entry.data
			return &result, nil
		}
	}

	lock := usageMetricsLock(userId)
	lock.Lock()
	defer lock.Unlock()

	if cached, ok := usageMetricsCache.Load(userId); ok {
		entry := cached.(usageMetricsCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			result := entry.data
			return &result, nil
		}
	}

	now := time.Now()
	currentTime := now.Unix()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()

	result := UsageMetrics{}
	minuteQuery := LOG_DB.Model(&Log{}).Select(`
		COUNT(*) AS rpm,
		COALESCE(SUM(prompt_tokens + completion_tokens), 0) AS tpm`)
	if userId > 0 {
		minuteQuery = minuteQuery.Where("user_id = ?", userId)
	}
	minuteQuery = minuteQuery.Where("created_at >= ? AND created_at <= ?", currentTime-60, currentTime)
	if err := minuteQuery.Row().Scan(&result.RPM, &result.TPM); err != nil {
		return nil, err
	}

	todayQuery := LOG_DB.Model(&Log{}).Select("COALESCE(SUM(quota), 0)")
	if userId > 0 {
		todayQuery = todayQuery.Where("user_id = ?", userId)
	}
	todayQuery = todayQuery.Where("created_at >= ? AND created_at <= ?", startOfDay, currentTime)
	if err := todayQuery.Scan(&result.TodaySpend).Error; err != nil {
		return nil, err
	}

	expiresAt := time.Now().Add(usageMetricsTTL)
	result.CachedUntil = expiresAt.Unix()
	usageMetricsCache.Store(userId, usageMetricsCacheEntry{data: result, expiresAt: expiresAt})
	return &result, nil
}
