// Package redistest 提供一个只支持 GET / SET / DEL 的内存 RESP 服务，
// 测试用它替换全局 Redis 客户端，不依赖外部 Redis。只供各包的 _test.go 使用。
package redistest

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
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/songquanpeng/one-api/common"
)

// Fake 是内存里的键值表，另外记下每个 key 写入时带的过期时间。
type Fake struct {
	mu   sync.Mutex
	data map[string]string
	ttl  map[string]time.Duration
}

// Use 让本测试里的 Redis 读写落到一个新的 Fake 上，测试结束后恢复全局状态。
func Use(t *testing.T) *Fake {
	t.Helper()
	fake := &Fake{data: map[string]string{}, ttl: map[string]time.Duration{}}
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

// Get 返回 key 当前的值。
func (f *Fake) Get(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.data[key]
	return value, ok
}

// Set 直接写入一个不带过期时间的值，用来模拟其他实例写入的内容。
func (f *Fake) Set(key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[key] = value
	delete(f.ttl, key)
}

// TTL 返回 key 最近一次写入时带的过期时间；没有带过期时间或 key 不存在时为 0。
func (f *Fake) TTL(key string) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ttl[key]
}

func (f *Fake) handle(args []string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch command := strings.ToUpper(args[0]); {
	case command == "GET" && len(args) == 2:
		if value, ok := f.data[args[1]]; ok {
			return fmt.Sprintf("$%d\r\n%s\r\n", len(value), value)
		}
		return "$-1\r\n"
	case command == "SET" && len(args) >= 3:
		f.data[args[1]] = args[2]
		delete(f.ttl, args[1])
		// 过期时间：EX <秒> 或 PX <毫秒>
		for i := 3; i+1 < len(args); i++ {
			amount, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil {
				continue
			}
			switch strings.ToUpper(args[i]) {
			case "EX":
				f.ttl[args[1]] = time.Duration(amount) * time.Second
			case "PX":
				f.ttl[args[1]] = time.Duration(amount) * time.Millisecond
			}
		}
		return "+OK\r\n"
	case command == "DEL" && len(args) >= 2:
		deleted := 0
		for _, key := range args[1:] {
			if _, ok := f.data[key]; ok {
				delete(f.data, key)
				delete(f.ttl, key)
				deleted++
			}
		}
		return fmt.Sprintf(":%d\r\n", deleted)
	}
	return "-ERR unsupported command\r\n"
}

func (f *Fake) serve(conn net.Conn) {
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
