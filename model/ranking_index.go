package model

import (
	"fmt"

	"gorm.io/gorm"
)

const rankingLogIndex = "idx_logs_ranking_daily"

// 统计 SQL 使用固定日志类型字面量，让 PostgreSQL 的 generic prepared plan
// 也能证明查询满足部分索引谓词；所有时间边界仍使用绑定参数。
var rankingLogPredicate = fmt.Sprintf("type = %d AND ranking_tokens IS NOT NULL", LogTypeConsume)

func rankingLogIndexSQL(dialect string) string {
	switch dialect {
	case "postgres":
		return "CREATE INDEX IF NOT EXISTS " + rankingLogIndex + " ON logs (created_at) INCLUDE (ranking_model_name, provider, ranking_tokens) WHERE " + rankingLogPredicate
	case "sqlite":
		return "CREATE INDEX IF NOT EXISTS " + rankingLogIndex + " ON logs (created_at, ranking_model_name, provider, ranking_tokens) WHERE " + rankingLogPredicate
	default:
		// MySQL 无部分索引/INCLUDE，使用较窄复合索引控制写入与空间成本。
		return "CREATE INDEX " + rankingLogIndex + " ON logs (type, created_at)"
	}
}

func EnsureRankingLogIndex(db *gorm.DB) error {
	if db.Migrator().HasIndex(&Log{}, rankingLogIndex) {
		return nil
	}
	return db.Exec(rankingLogIndexSQL(db.Dialector.Name())).Error
}
