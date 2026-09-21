package model

import (
	"sort"
	"time"
)

const rankingDay int64 = 86400

type RankingDaily struct {
	DayStart  int64  `gorm:"primaryKey;autoIncrement:false"`
	ModelName string `gorm:"primaryKey;type:varchar(180)"`
	Provider  string `gorm:"primaryKey;type:varchar(200)"`
	Tokens    int64
	Requests  int64
}

func (RankingDaily) TableName() string { return "ranking_daily" }

// 单行状态既保存覆盖起点，又为聚合/发布/清理提供带 fencing 的数据库租约。
type RankingState struct {
	ID              int `gorm:"primaryKey;autoIncrement:false"`
	CoverageStart   int64
	NextDay         int64
	LeaseOwner      string `gorm:"type:varchar(64)"`
	LeaseUntil      int64
	SnapshotDirty   bool
	LogPurgedBefore int64
	Generation      int64
	Heartbeat       int64
}

type RankingJob struct {
	DayStart         int64 `gorm:"primaryKey;autoIncrement:false"`
	CompletedAt      int64
	Finalized        bool
	ExcludedRequests int64
}

type RankingSnapshot struct {
	ID      int    `gorm:"primaryKey;autoIncrement:false"`
	Payload string `gorm:"type:text"`
	Version string `gorm:"type:varchar(64)"`
}

type RankingEntry struct {
	Model         string   `json:"model"`
	Author        string   `json:"author"`
	Tokens        int64    `json:"tokens"`
	Share         float64  `json:"share"`
	ChangePercent *float64 `json:"change_percent"`
}

type RankingBoard struct {
	Entries            []RankingEntry `json:"entries"`
	TotalTokens        int64          `json:"total_tokens"`
	ComparisonComplete bool           `json:"comparison_complete"`
	WindowComplete     bool           `json:"window_complete"`
}

type RankingSeries struct {
	Model  string  `json:"model"`
	Tokens []int64 `json:"tokens"`
}

type RankingTrend struct {
	Dates  []string        `json:"dates"`
	Series []RankingSeries `json:"series"`
}

type RankingData struct {
	Version       string                  `json:"version"`
	GeneratedAt   int64                   `json:"generated_at"`
	DataThrough   string                  `json:"data_through"`
	CoverageStart string                  `json:"coverage_start"`
	Timezone      string                  `json:"timezone"`
	Leaderboards  map[string]RankingBoard `json:"leaderboards"`
	Trend         RankingTrend            `json:"trend"`
}

func rankingDate(ts int64) string    { return time.Unix(ts, 0).UTC().Format("2006-01-02") }
func floorRankingDay(ts int64) int64 { return ts - ts%rankingDay }

// buildRankingData 只接收日汇总，固定窗口和排名并列时的顺序使快照可重复计算。
func buildRankingData(rows []RankingDaily, coverage, end, now int64) RankingData {
	data := RankingData{GeneratedAt: now, DataThrough: rankingDate(end - rankingDay), CoverageStart: rankingDate(coverage), Timezone: "UTC", Leaderboards: map[string]RankingBoard{}}
	for label, days := range map[string]int64{"day": 1, "week": 7, "month": 30} {
		start := end - days*rankingDay
		current, previous := map[string]int64{}, map[string]int64{}
		board := RankingBoard{Entries: []RankingEntry{}, ComparisonComplete: coverage <= start-days*rankingDay, WindowComplete: coverage <= start}
		for _, row := range rows {
			if row.DayStart < coverage {
				continue
			}
			if row.DayStart >= start && row.DayStart < end {
				current[row.ModelName] += row.Tokens
			} else if row.DayStart >= start-days*rankingDay && row.DayStart < start {
				previous[row.ModelName] += row.Tokens
			}
		}
		for _, tokens := range current {
			board.TotalTokens += tokens
		}
		for name, tokens := range current {
			if tokens <= 0 {
				continue
			}
			_, author := RankingModelIdentity(name)
			entry := RankingEntry{Model: name, Author: author, Tokens: tokens, Share: float64(tokens) / float64(board.TotalTokens)}
			if board.ComparisonComplete && previous[name] > 0 {
				change := (float64(tokens)/float64(previous[name]) - 1) * 100
				entry.ChangePercent = &change
			}
			board.Entries = append(board.Entries, entry)
		}
		sort.Slice(board.Entries, func(i, j int) bool {
			if board.Entries[i].Tokens == board.Entries[j].Tokens {
				return board.Entries[i].Model < board.Entries[j].Model
			}
			return board.Entries[i].Tokens > board.Entries[j].Tokens
		})
		data.Leaderboards[label] = board
	}
	start := end - 30*rankingDay
	if coverage > start {
		start = coverage
	}
	data.Trend = RankingTrend{Dates: []string{}, Series: []RankingSeries{}}
	for day := start; day < end; day += rankingDay {
		data.Trend.Dates = append(data.Trend.Dates, rankingDate(day))
	}
	indexes := map[string]int{}
	for _, entry := range data.Leaderboards["month"].Entries {
		if len(indexes) == 10 {
			break
		}
		indexes[entry.Model] = len(data.Trend.Series)
		data.Trend.Series = append(data.Trend.Series, RankingSeries{entry.Model, make([]int64, len(data.Trend.Dates))})
	}
	others := len(data.Trend.Series)
	data.Trend.Series = append(data.Trend.Series, RankingSeries{"Others", make([]int64, len(data.Trend.Dates))})
	for _, row := range rows {
		if row.DayStart < start || row.DayStart >= end {
			continue
		}
		i, ok := indexes[row.ModelName]
		if !ok {
			i = others
		}
		data.Trend.Series[i].Tokens[(row.DayStart-start)/rankingDay] += row.Tokens
	}
	return data
}
