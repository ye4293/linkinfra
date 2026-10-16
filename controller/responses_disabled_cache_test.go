package controller

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/go-redis/redis/v8"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
	"github.com/stretchr/testify/require"
)

func TestResponseIDCacheRejectsDisabledChannel(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	oldRedis, oldMemory, oldRDB := common.RedisEnabled, config.MemoryCacheEnabled, common.RDB
	t.Cleanup(func() {
		common.RedisEnabled, config.MemoryCacheEnabled, common.RDB = oldRedis, oldMemory, oldRDB
	})
	for id := 1; id <= 2; id++ {
		priority, weight := int64(100-id), uint(1)
		ch := model.Channel{Id: id, Status: common.ChannelStatusEnabled, Group: "default", Models: "gpt-test", Priority: &priority, Weight: &weight}
		require.NoError(t, db.Create(&ch).Error)
		require.NoError(t, db.Create(&model.Ability{ChannelId: id, Group: "default", Model: "gpt-test", Enabled: true, Priority: &priority}).Error)
	}
	config.MemoryCacheEnabled = true
	model.InitChannelCache()
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 1).Update("status", common.ChannelStatusManuallyDisabled).Error)
	cached, err := model.CacheGetChannelCopy(1)
	require.NoError(t, err)
	require.Equal(t, common.ChannelStatusEnabled, cached.Status, "模拟另一个实例禁用渠道后，本地缓存尚未同步")

	// 最小 RESP 服务只返回历史渠道 1，测试不依赖外部 Redis。
	client := redis.NewClient(&redis.Options{Dialer: func(context.Context, string, string) (net.Conn, error) {
		conn, server := net.Pipe()
		go func() {
			defer server.Close()
			reader := bufio.NewReader(server)
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				count, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
				for i := 0; i < count; i++ {
					line, err = reader.ReadString('\n')
					if err != nil {
						return
					}
					size, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "$")))
					if _, err = io.CopyN(io.Discard, reader, int64(size+2)); err != nil {
						return
					}
				}
				if _, err = fmt.Fprint(server, "$3\r\n1:0\r\n"); err != nil {
					return
				}
			}
		}()
		return conn, nil
	}})
	t.Cleanup(func() { _ = client.Close() })
	common.RedisEnabled, common.RDB = true, client
	channel, key, err := model.CacheGetRandomSatisfiedChannel(context.Background(), "default", "gpt-test", 0, "resp-test")
	require.NoError(t, err)
	require.Equal(t, 2, channel.Id)
	require.Equal(t, -1, key, "旧渠道的 key 索引也不能带入新渠道")
}
