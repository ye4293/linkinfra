package model

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/songquanpeng/one-api/common/logger"
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

// DashboardSourceStats.Kind 的取值。
const (
	DashboardSourceTag         = "tag"          // 打了计费标签的渠道，按标签归组
	DashboardSourceChannelType = "channel_type" // 没有标签的渠道，按渠道类型归组
	DashboardSourceDeleted     = "deleted"      // 渠道已删除，合并为一项
)

// DashboardSourceStats 是一个来源在统计窗口内的消耗。
// Name 是展示名：标签归组为标签本身，渠道类型的名称由 controller 按类型选项补上。
type DashboardSourceStats struct {
	Kind        string `json:"kind"`
	Tag         string `json:"tag,omitempty"`
	ChannelType int    `json:"channel_type,omitempty"`
	Name        string `json:"name"`
	QuotaSum    int64  `json:"quota_sum"`
}

type Dashboard24hStats struct {
	StartTimestamp int64                  `json:"start_timestamp"`
	EndTimestamp   int64                  `json:"end_timestamp"`
	CachedUntil    int64                  `json:"cached_until"`
	Hourly         []DashboardHourlyStats `json:"hourly"`
	ModelStats     []ModelQuotaStats      `json:"model_stats"`
	UserStats      []DashboardUserStats   `json:"user_stats,omitempty"`
	SourceStats    []DashboardSourceStats `json:"source_stats,omitempty"`
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
	result.SourceStats = append([]DashboardSourceStats(nil), data.SourceStats...)
	return &result
}

// dashboardSourceStats 按来源汇总时间范围内的消耗，按消耗降序返回，只包含有消耗的来源。
// 日志只记录 channel_id，且日志库可以独立于主库，所以先按渠道聚合，再在内存里归组。
// 归组依据是渠道当前的配置，不是请求发生时的快照。
func dashboardSourceStats(startTimestamp, endTimestamp int64) ([]DashboardSourceStats, error) {
	var rows []struct {
		ChannelId int
		QuotaSum  int64
	}
	err := LOG_DB.Model(&Log{}).
		Select("channel_id, COALESCE(SUM(quota), 0) AS quota_sum").
		Where("channel_id > 0 AND created_at >= ? AND created_at <= ?", startTimestamp, endTimestamp).
		Group("channel_id").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	var channels []*Channel
	if err := DB.Select("id, type, config").Find(&channels).Error; err != nil {
		return nil, err
	}
	channelsByID := make(map[int]*Channel, len(channels))
	for _, channel := range channels {
		channelsByID[channel.Id] = channel
	}

	type sourceKey struct {
		kind        string
		tag         string
		channelType int
	}
	sums := map[sourceKey]int64{}
	for _, row := range rows {
		key := sourceKey{kind: DashboardSourceDeleted}
		if channel, ok := channelsByID[row.ChannelId]; ok {
			if tag := ChannelBillingTag(channel); tag != "" {
				key = sourceKey{kind: DashboardSourceTag, tag: tag}
			} else {
				key = sourceKey{kind: DashboardSourceChannelType, channelType: channel.Type}
			}
		}
		sums[key] += row.QuotaSum
	}

	stats := make([]DashboardSourceStats, 0, len(sums))
	for key, quota := range sums {
		if quota <= 0 {
			continue
		}
		item := DashboardSourceStats{Kind: key.kind, Tag: key.tag, ChannelType: key.channelType, QuotaSum: quota}
		if key.kind == DashboardSourceTag {
			item.Name = key.tag
		}
		stats = append(stats, item)
	}
	sort.Slice(stats, func(i, j int) bool {
		a, b := stats[i], stats[j]
		if a.QuotaSum != b.QuotaSum {
			return a.QuotaSum > b.QuotaSum
		}
		// 消耗相同时顺序固定，不随 map 遍历变化。
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Tag != b.Tag {
			return a.Tag < b.Tag
		}
		return a.ChannelType < b.ChannelType
	})
	return stats, nil
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
		// 来源消耗是附加数据：它的查询失败时只记日志、本次不返回来源，
		// 不让总览原有的小时、模型、用户和充值统计跟着失败。
		if sourceStats, err := dashboardSourceStats(startTimestamp, endTimestamp); err != nil {
			logger.SysError("dashboard source stats failed: " + err.Error())
		} else {
			data.SourceStats = sourceStats
		}
	}

	expiresAt := now.Add(dashboardStatsTTL)
	dashboardStatsCache.Store(userId, dashboardStatsCacheEntry{
		data:      cloneDashboard24hStats(data),
		expiresAt: expiresAt,
	})
	return data, nil
}
