package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"time"
)

type CachedRanking struct {
	Payload []byte
	ETag    string
}

var rankingCache atomic.Pointer[CachedRanking]

// 固定一分钟从持久快照同步一次；HTTP 请求永远不触发 SQL 或缓存重建。
func RefreshRankingCache() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var snapshot RankingSnapshot
	query := LOG_DB.WithContext(ctx).Where("id = 1")
	if cached := rankingCache.Load(); cached != nil {
		query = query.Where("version <> ?", strings.Trim(cached.ETag, `"`))
	}
	result := query.Limit(1).Find(&snapshot)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return nil
	}
	if !json.Valid([]byte(snapshot.Payload)) {
		return errors.New("invalid ranking snapshot")
	}
	rankingCache.Store(&CachedRanking{Payload: []byte(`{"success":true,"data":` + snapshot.Payload + `}`), ETag: `"` + snapshot.Version + `"`})
	return nil
}

func GetCachedRanking() *CachedRanking { return rankingCache.Load() }
