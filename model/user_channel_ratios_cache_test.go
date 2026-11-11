package model

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-redis/redis/v8"
	"github.com/songquanpeng/one-api/common"
	"github.com/stretchr/testify/require"
)

// fakeRedis 是只支持 GET / SET / DEL 的内存 RESP 服务，测试不依赖外部 Redis。
type fakeRedis struct {
	mu   sync.Mutex
	data map[string]string
}

func (f *fakeRedis) get(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.data[key]
	return value, ok
}

func (f *fakeRedis) set(key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[key] = value
}

func (f *fakeRedis) handle(args []string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch command := strings.ToUpper(args[0]); {
	case command == "GET" && len(args) == 2:
		if value, ok := f.data[args[1]]; ok {
			return fmt.Sprintf("$%d\r\n%s\r\n", len(value), value)
		}
		return "$-1\r\n"
	case command == "SET" && len(args) >= 3:
		// 过期时间等附加参数不影响这里的断言，忽略
		f.data[args[1]] = args[2]
		return "+OK\r\n"
	case command == "DEL" && len(args) >= 2:
		deleted := 0
		for _, key := range args[1:] {
			if _, ok := f.data[key]; ok {
				delete(f.data, key)
				deleted++
			}
		}
		return fmt.Sprintf(":%d\r\n", deleted)
	}
	return "-ERR unsupported command\r\n"
}

func (f *fakeRedis) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		// 请求是 RESP 数组：*<参数个数>，随后每个参数是 $<长度> 加内容
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		count, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
		if err != nil || count < 1 {
			return
		}
		args := make([]string, 0, count)
		for i := 0; i < count; i++ {
			if line, err = reader.ReadString('\n'); err != nil {
				return
			}
			size, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "$")))
			if err != nil || size < 0 {
				return
			}
			buf := make([]byte, size+2)
			if _, err = io.ReadFull(reader, buf); err != nil {
				return
			}
			args = append(args, string(buf[:size]))
		}
		if _, err = io.WriteString(conn, f.handle(args)); err != nil {
			return
		}
	}
}

// useFakeRedis 让本测试里的 Redis 读写落到内存服务上，结束后恢复全局状态。
func useFakeRedis(t *testing.T) *fakeRedis {
	t.Helper()
	fake := &fakeRedis{data: map[string]string{}}
	client := redis.NewClient(&redis.Options{Dialer: func(context.Context, string, string) (net.Conn, error) {
		clientConn, serverConn := net.Pipe()
		go fake.serve(serverConn)
		return clientConn, nil
	}})
	oldEnabled, oldClient := common.RedisEnabled, common.RDB
	common.RedisEnabled, common.RDB = true, client
	t.Cleanup(func() {
		common.RedisEnabled, common.RDB = oldEnabled, oldClient
		_ = client.Close()
	})
	return fake
}

// Redis 开启时的用户折扣缓存：不读旧版本实例留下的缓存（旧 key，不含标签折扣），
// 自己写入的缓存保留标签折扣；清除缓存时新旧两个 key 都删除。
func TestUserChannelRatiosCacheIgnoresLegacyPayload(t *testing.T) {
	db := setupTestDB(t, &User{})
	fake := useFakeRedis(t)
	require.NoError(t, db.Create(&User{Id: 1, Username: "tagged", ChannelRatios: `{"48":0.9,"tag:vertex-claude":0.85}`}).Error)
	require.NoError(t, db.Create(&User{Id: 2, Username: "tag-only", AffCode: "tag-only", AccessToken: "tag-only", ChannelRatios: `{"tag:vertex-claude":0.5}`}).Error)
	// 旧版本实例写缓存时丢掉标签项：用户 1 只剩类型折扣，只配了标签折扣的用户 2 被写成空串。
	fake.set("user_channel_ratios:1", `{"48":0.9}`)
	fake.set("user_channel_ratios:2", "")

	for id, want := range map[int]float64{1: 0.85, 2: 0.5} {
		ratio, matched := ResolveUserChannelRatio(id, 48, "vertex-claude")
		require.Equal(t, want, ratio, "user %d", id)
		require.Equal(t, "vertex-claude", matched, "user %d", id)
	}

	// 新写入的缓存带标签折扣；之后直接命中缓存，不再读库。
	cached, ok := fake.get("user_channel_ratios:v2:1")
	require.True(t, ok)
	require.JSONEq(t, `{"48":0.9,"tag:vertex-claude":0.85}`, cached)
	require.NoError(t, db.Model(&User{}).Where("id = ?", 1).Update("channel_ratios", "").Error)
	ratio, matched := ResolveUserChannelRatio(1, 48, "vertex-claude")
	require.Equal(t, 0.85, ratio)
	require.Equal(t, "vertex-claude", matched)

	// 清除缓存：新旧两个 key 都删除，下一次读取回源读库。
	InvalidateUserChannelRatiosCache(1)
	_, ok = fake.get("user_channel_ratios:v2:1")
	require.False(t, ok)
	_, ok = fake.get("user_channel_ratios:1")
	require.False(t, ok)
	ratio, matched = ResolveUserChannelRatio(1, 48, "vertex-claude")
	require.Equal(t, 1.0, ratio)
	require.Empty(t, matched)

	// 没有任何折扣的用户写入空串占位，避免反复读库；命中空串按没有折扣处理。
	cached, ok = fake.get("user_channel_ratios:v2:1")
	require.True(t, ok)
	require.Empty(t, cached)
	ratio, _ = ResolveUserChannelRatio(1, 48, "vertex-claude")
	require.Equal(t, 1.0, ratio)
}
