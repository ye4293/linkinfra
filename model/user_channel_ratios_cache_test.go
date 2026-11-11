package model

import (
	"testing"
	"time"

	"github.com/songquanpeng/one-api/common/redistest"
	"github.com/stretchr/testify/require"
)

// Redis 开启时的用户折扣缓存：不读旧版本实例留下的缓存（旧 key，不含标签折扣），
// 自己写入的缓存保留标签折扣并带过期时间；清除缓存时新旧两个 key 都删除。
func TestUserChannelRatiosCacheIgnoresLegacyPayload(t *testing.T) {
	db := setupTestDB(t, &User{})
	fake := redistest.Use(t)
	require.NoError(t, db.Create(&User{Id: 1, Username: "tagged", ChannelRatios: `{"48":0.9,"tag:vertex-claude":0.85}`}).Error)
	require.NoError(t, db.Create(&User{Id: 2, Username: "tag-only", AffCode: "tag-only", AccessToken: "tag-only", ChannelRatios: `{"tag:vertex-claude":0.5}`}).Error)
	// 旧版本实例写缓存时丢掉标签项：用户 1 只剩类型折扣，只配了标签折扣的用户 2 被写成空串。
	fake.Set("user_channel_ratios:1", `{"48":0.9}`)
	fake.Set("user_channel_ratios:2", "")

	for id, want := range map[int]float64{1: 0.85, 2: 0.5} {
		ratio, matched := ResolveUserChannelRatio(id, 48, "vertex-claude")
		require.Equal(t, want, ratio, "user %d", id)
		require.Equal(t, "vertex-claude", matched, "user %d", id)
	}

	// 新写入的缓存带标签折扣；之后直接命中缓存，不再读库。
	cached, ok := fake.Get("user_channel_ratios:v2:1")
	require.True(t, ok)
	require.JSONEq(t, `{"48":0.9,"tag:vertex-claude":0.85}`, cached)
	// 缓存带过期时间：绕过保存接口改了库里的折扣（没有人清缓存）时，最多陈旧一个同步周期。
	require.Positive(t, fake.TTL("user_channel_ratios:v2:1"))
	require.Equal(t, time.Duration(UserId2ChannelRatiosCacheSeconds)*time.Second, fake.TTL("user_channel_ratios:v2:1"))
	require.NoError(t, db.Model(&User{}).Where("id = ?", 1).Update("channel_ratios", "").Error)
	ratio, matched := ResolveUserChannelRatio(1, 48, "vertex-claude")
	require.Equal(t, 0.85, ratio)
	require.Equal(t, "vertex-claude", matched)

	// 清除缓存：新旧两个 key 都删除，下一次读取回源读库。
	InvalidateUserChannelRatiosCache(1)
	_, ok = fake.Get("user_channel_ratios:v2:1")
	require.False(t, ok)
	_, ok = fake.Get("user_channel_ratios:1")
	require.False(t, ok)
	ratio, matched = ResolveUserChannelRatio(1, 48, "vertex-claude")
	require.Equal(t, 1.0, ratio)
	require.Empty(t, matched)

	// 没有任何折扣的用户写入空串占位，避免反复读库；命中空串按没有折扣处理。
	cached, ok = fake.Get("user_channel_ratios:v2:1")
	require.True(t, ok)
	require.Empty(t, cached)
	ratio, _ = ResolveUserChannelRatio(1, 48, "vertex-claude")
	require.Equal(t, 1.0, ratio)
}
