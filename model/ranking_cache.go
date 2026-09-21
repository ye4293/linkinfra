package model

import (
	"encoding/json"
	"errors"
	"sync/atomic"

	"gorm.io/gorm"
)

type CachedRanking struct {
	Payload []byte
	ETag    string
}

var rankingCache atomic.Pointer[CachedRanking]

// 固定一分钟从持久快照同步一次；HTTP 请求永远不触发 SQL 或缓存重建。
func RefreshRankingCache() error {
	var snapshot RankingSnapshot
	if err := LOG_DB.First(&snapshot, 1).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	if cached := rankingCache.Load(); cached != nil && cached.ETag == `"`+snapshot.Version+`"` {
		return nil
	}
	if !json.Valid([]byte(snapshot.Payload)) {
		return errors.New("invalid ranking snapshot")
	}
	rankingCache.Store(&CachedRanking{Payload: []byte(`{"success":true,"data":` + snapshot.Payload + `}`), ETag: `"` + snapshot.Version + `"`})
	return nil
}

func GetCachedRanking() *CachedRanking { return rankingCache.Load() }
