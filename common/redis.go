package common

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/songquanpeng/one-api/common/logger"
)

var RDB *redis.Client
var RedisEnabled = true

// errRedisUnavailable Redis 未启用或未初始化时的统一错误。
//
// RedisEnabled 的默认值是 true（见上一行），只有 InitRedisClient() 发现没配
// REDIS_CONN_STRING 时才置为 false。这意味着任何在 InitRedisClient() 之前、
// 或没走完整启动流程的场景（单元测试、一次性 CLI 任务）里调用 RedisSet /
// RedisGet / RedisDel / RedisDecrease，RDB 都还是 nil —— 原实现直接
// RDB.Set(...) 会空指针 panic。
//
// 同文件的 RedisLockAcquire / RedisLockRelease / RedisIncrMod 本来就写的是
// `!RedisEnabled || RDB == nil` 双条件守卫，只有这四个裸函数漏了。
var errRedisUnavailable = errors.New("redis is not available")

// InitRedisClient This function is called after init()
func InitRedisClient() (err error) {
	if os.Getenv("REDIS_CONN_STRING") == "" {
		RedisEnabled = false
		logger.SysLog("REDIS_CONN_STRING not set, Redis is not enabled")
		return nil
	}
	if os.Getenv("SYNC_FREQUENCY") == "" {
		RedisEnabled = false
		logger.SysLog("SYNC_FREQUENCY not set, Redis is disabled")
		return nil
	}
	logger.SysLog("Redis is enabled")
	opt, err := redis.ParseURL(os.Getenv("REDIS_CONN_STRING"))
	if err != nil {
		logger.FatalLog("failed to parse Redis connection string: " + err.Error())
	}
	RDB = redis.NewClient(opt)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = RDB.Ping(ctx).Result()
	if err != nil {
		logger.FatalLog("Redis ping test failed: " + err.Error())
	}
	return err
}

func ParseRedisOption() *redis.Options {
	opt, err := redis.ParseURL(os.Getenv("REDIS_CONN_STRING"))
	if err != nil {
		logger.FatalLog("failed to parse Redis connection string: " + err.Error())
	}
	return opt
}

func RedisSet(key string, value string, expiration time.Duration) error {
	if !RedisEnabled || RDB == nil {
		return errRedisUnavailable
	}
	ctx := context.Background()
	return RDB.Set(ctx, key, value, expiration).Err()
}

func RedisGet(key string) (string, error) {
	if !RedisEnabled || RDB == nil {
		return "", errRedisUnavailable
	}
	ctx := context.Background()
	return RDB.Get(ctx, key).Result()
}

func RedisDel(key string) error {
	if !RedisEnabled || RDB == nil {
		return errRedisUnavailable
	}
	ctx := context.Background()
	return RDB.Del(ctx, key).Err()
}

func RedisDecrease(key string, value int64) error {
	if !RedisEnabled || RDB == nil {
		return errRedisUnavailable
	}
	ctx := context.Background()
	return RDB.DecrBy(ctx, key, value).Err()
}

// RedisLockAcquire tries to acquire a distributed lock using SET NX.
// Returns a non-empty token if the lock was acquired, empty string if not.
// Pass the returned token to RedisLockRelease to safely release.
func RedisLockAcquire(key string, ttl time.Duration) string {
	if !RedisEnabled || RDB == nil {
		return "local"
	}
	ctx := context.Background()
	token := fmt.Sprintf("%d:%d", time.Now().UnixNano(), os.Getpid())
	ok, err := RDB.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		logger.SysError("redis lock acquire error: " + err.Error())
		return ""
	}
	if !ok {
		return ""
	}
	return token
}

// RedisLockRelease releases a distributed lock only if the token matches (compare-and-delete).
func RedisLockRelease(key string, token string) {
	if !RedisEnabled || RDB == nil || token == "" || token == "local" {
		return
	}
	ctx := context.Background()
	const luaScript = `if redis.call("get",KEYS[1]) == ARGV[1] then return redis.call("del",KEYS[1]) else return 0 end`
	_ = RDB.Eval(ctx, luaScript, []string{key}, token).Err()
}

// RedisIncrMod atomically increments a counter and returns (counter-1) % n.
// Used for distributed round-robin selection without DB writes.
// The counter is kept alive with the given TTL on each access.
// Returns an error if Redis is unavailable; callers should fall back to in-process logic.
func RedisIncrMod(key string, n int, ttl time.Duration) (int, error) {
	if !RedisEnabled || RDB == nil {
		return 0, fmt.Errorf("redis not available")
	}
	if n <= 0 {
		return 0, fmt.Errorf("n must be positive")
	}
	ctx := context.Background()
	pipe := RDB.Pipeline()
	incrCmd := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	// Incr returns 1-based counter; subtract 1 for 0-based index
	return int((incrCmd.Val() - 1) % int64(n)), nil
}
