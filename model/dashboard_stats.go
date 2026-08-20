package model

import (
	"fmt"
	"sync"
	"time"
)

const dashboardStatsTTL = 10 * time.Minute

type DashboardHourlyStats struct {
	Hour        string `json:"hour"`
	Timestamp   int64  `json:"timestamp"`
	Consumption int64  `json:"consumption"`
	Tokens      int64  `json:"tokens"`
	Times       int64  `json:"times"`
}

type DashboardUserStats struct {
	UserId   int    `json:"user_id"`
	Username string `json:"username"`
	QuotaSum int64  `json:"quota_sum"`
}

type Dashboard24hStats struct {
	StartTimestamp int64                  `json:"start_timestamp"`
	EndTimestamp   int64                  `json:"end_timestamp"`
	CachedUntil    int64                  `json:"cached_until"`
	Hourly         []DashboardHourlyStats `json:"hourly"`
	ModelStats     []ModelQuotaStats      `json:"model_stats"`
	UserStats      []DashboardUserStats   `json:"user_stats,omitempty"`
	RechargeAmount float64                `json:"recharge_amount,omitempty"`
}

type dashboardStatsCacheEntry struct {
	data      *Dashboard24hStats
	expiresAt time.Time
}

var (
	dashboardStatsCache sync.Map
	dashboardStatsLocks sync.Map
)

func dashboardStatsLock(userId int) *sync.Mutex {
	lock, _ := dashboardStatsLocks.LoadOrStore(userId, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func cloneDashboard24hStats(data *Dashboard24hStats) *Dashboard24hStats {
	result := *data
	result.Hourly = append([]DashboardHourlyStats(nil), data.Hourly...)
	result.ModelStats = append([]ModelQuotaStats(nil), data.ModelStats...)
	result.UserStats = append([]DashboardUserStats(nil), data.UserStats...)
	return &result
}

func GetDashboard24hStats(userId int) (*Dashboard24hStats, error) {
	if cached, ok := dashboardStatsCache.Load(userId); ok {
		entry := cached.(dashboardStatsCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			return cloneDashboard24hStats(entry.data), nil
		}
	}

	lock := dashboardStatsLock(userId)
	lock.Lock()
	defer lock.Unlock()

	if cached, ok := dashboardStatsCache.Load(userId); ok {
		entry := cached.(dashboardStatsCacheEntry)
		if time.Now().Before(entry.expiresAt) {
			return cloneDashboard24hStats(entry.data), nil
		}
	}

	now := time.Now().UTC()
	endTimestamp := now.Unix()
	startHour := now.Truncate(time.Hour).Add(-23 * time.Hour)
	startTimestamp := startHour.Unix()

	data := &Dashboard24hStats{
		StartTimestamp: startTimestamp,
		EndTimestamp:   endTimestamp,
		CachedUntil:    now.Add(dashboardStatsTTL).Unix(),
		Hourly:         make([]DashboardHourlyStats, 24),
	}
	indexes := make(map[int64]int, 24)
	for i := 0; i < 24; i++ {
		bucket := startHour.Add(time.Duration(i) * time.Hour)
		data.Hourly[i] = DashboardHourlyStats{
			Hour:      fmt.Sprintf("%02d", bucket.Hour()),
			Timestamp: bucket.Unix(),
		}
		indexes[bucket.Unix()] = i
	}
	type hourlyRow struct {
		Bucket      int64
		Consumption int64
		Tokens      int64
		Times       int64
	}
	var hourlyRows []hourlyRow
	hourlyQuery := LOG_DB.Model(&Log{}).Select(`
		(created_at - created_at % 3600) AS bucket,
		COALESCE(SUM(quota), 0) AS consumption,
		COALESCE(SUM(prompt_tokens + completion_tokens), 0) AS tokens,
		COUNT(*) AS times`)
	if userId > 0 {
		hourlyQuery = hourlyQuery.Where("user_id = ?", userId)
	}
	hourlyQuery = hourlyQuery.Where("created_at >= ? AND created_at <= ?", startTimestamp, endTimestamp)
	if err := hourlyQuery.Group(hourBucketExpr).Scan(&hourlyRows).Error; err != nil {
		return nil, err
	}
	for _, row := range hourlyRows {
		if index, ok := indexes[row.Bucket]; ok {
			data.Hourly[index].Consumption = row.Consumption
			data.Hourly[index].Tokens = row.Tokens
			data.Hourly[index].Times = row.Times
		}
	}

	modelQuery := LOG_DB.Model(&Log{}).
		Select("model_name, COALESCE(SUM(quota), 0) AS quota_sum").
		Where("model_name <> ''")
	if userId > 0 {
		modelQuery = modelQuery.Where("user_id = ?", userId)
	}
	modelQuery = modelQuery.Where("created_at >= ? AND created_at <= ?", startTimestamp, endTimestamp)
	if err := modelQuery.Group("model_name").Order("quota_sum DESC").Limit(5).Scan(&data.ModelStats).Error; err != nil {
		return nil, err
	}

	if userId == 0 {
		userQuery := LOG_DB.Model(&Log{}).
			Select("user_id, username, COALESCE(SUM(quota), 0) AS quota_sum")
		userQuery = userQuery.Where("created_at >= ? AND created_at <= ?", startTimestamp, endTimestamp)
		if err := userQuery.Group("user_id, username").Order("quota_sum DESC").Limit(5).Scan(&data.UserStats).Error; err != nil {
			return nil, err
		}
		if err := DB.Model(&TopUp{}).
			Select("COALESCE(SUM(money), 0)").
			Where("status = ? AND complete_time >= ? AND complete_time <= ? AND (other = ? OR other IS NULL) AND (currency = ? OR currency = ? OR currency IS NULL)", "success", startTimestamp, endTimestamp, "", "", "USD").
			Scan(&data.RechargeAmount).Error; err != nil {
			return nil, err
		}
	}

	expiresAt := now.Add(dashboardStatsTTL)
	dashboardStatsCache.Store(userId, dashboardStatsCacheEntry{
		data:      cloneDashboard24hStats(data),
		expiresAt: expiresAt,
	})
	return data, nil
}
