package service

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/songquanpeng/one-api/common"
)

const responsesStateTTL = 7 * 24 * time.Hour

type responsesStateOrigin struct {
	Provider       string `json:"provider"`
	ChannelID      int    `json:"channel_id"`
	KeyIndex       int    `json:"key_index"`
	KeyFingerprint string `json:"key_fingerprint,omitempty"`
}

type responsesStateStore interface {
	Lookup(context.Context, []string) (map[string]responsesStateOrigin, error)
	Remember(context.Context, map[string]responsesStateOrigin) error
}

type redisResponsesStateStore struct{}

func (redisResponsesStateStore) Lookup(ctx context.Context, keys []string) (map[string]responsesStateOrigin, error) {
	if common.RDB == nil {
		return nil, errors.New("Responses state cache unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	values, err := common.RDB.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, errors.New("Responses state cache unavailable")
	}
	result := make(map[string]responsesStateOrigin)
	pipe := common.RDB.Pipeline()
	for i, value := range values {
		if value == nil {
			continue
		}
		raw, ok := value.(string)
		var origin responsesStateOrigin
		if !ok || json.Unmarshal([]byte(raw), &origin) != nil || origin.ChannelID <= 0 || origin.KeyIndex < 0 {
			return nil, errors.New("Invalid Responses state cache entry")
		}
		result[keys[i]] = origin
		pipe.Expire(ctx, keys[i], responsesStateTTL)
	}
	if len(result) > 0 {
		if _, err := pipe.Exec(ctx); err != nil {
			return nil, errors.New("Responses state cache unavailable")
		}
	}
	return result, nil
}

func (redisResponsesStateStore) Remember(ctx context.Context, entries map[string]responsesStateOrigin) error {
	if common.RDB == nil {
		return errors.New("Responses state cache unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	pipe := common.RDB.Pipeline()
	for key, origin := range entries {
		data, _ := json.Marshal(origin)
		// 相同状态已有来源时不覆盖，防止历史重放改变来源。
		pipe.SetNX(ctx, key, string(data), responsesStateTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return errors.New("Failed to record Responses state origin")
	}
	return nil
}

type memoryResponsesStateEntry struct {
	origin   responsesStateOrigin
	expires  time.Time
	position *list.Element
}
type memoryResponsesStateStore struct {
	mu      sync.Mutex
	entries map[string]memoryResponsesStateEntry
	limit   int
	now     func() time.Time
	order   *list.List
}

func newMemoryResponsesStateStore(limit int) *memoryResponsesStateStore {
	return &memoryResponsesStateStore{entries: make(map[string]memoryResponsesStateEntry), limit: limit, now: time.Now, order: list.New()}
}

func (s *memoryResponsesStateStore) Lookup(_ context.Context, keys []string) (map[string]responsesStateOrigin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]responsesStateOrigin)
	for _, key := range keys {
		entry, ok := s.entries[key]
		if ok && entry.expires.After(s.now()) {
			result[key] = entry.origin
			entry.expires = s.now().Add(responsesStateTTL)
			s.entries[key] = entry
			s.order.MoveToBack(entry.position)
		} else {
			if ok {
				s.order.Remove(entry.position)
			}
			delete(s.entries, key)
		}
	}
	return result, nil
}

func (s *memoryResponsesStateStore) Remember(_ context.Context, entries map[string]responsesStateOrigin) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, origin := range entries {
		if old, ok := s.entries[key]; ok {
			if old.expires.After(s.now()) {
				continue
			}
			s.order.Remove(old.position)
			delete(s.entries, key)
		}
		if len(s.entries) >= s.limit {
			oldest := s.order.Front()
			delete(s.entries, oldest.Value.(string))
			s.order.Remove(oldest)
		}
		s.entries[key] = memoryResponsesStateEntry{origin, s.now().Add(responsesStateTTL), s.order.PushBack(key)}
	}
	return nil
}

var localResponsesStateStore = newMemoryResponsesStateStore(100000)

func currentResponsesStateStore() responsesStateStore {
	if common.RedisEnabled {
		return redisResponsesStateStore{}
	}
	return localResponsesStateStore
}
