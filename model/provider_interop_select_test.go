package model

import (
	"context"
	"fmt"
	"testing"

	"github.com/songquanpeng/one-api/common"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedInteropChannels 建三个 provider 各两个渠道，优先级互不相同，重试的先后因此是确定的：
//
//	anthropic #1（100）、#2（40）；vertex #3（90）、#4（30）；aws #5（80）、#6（20）
//
// 另有两个不该被互通规则带进来的渠道：#7 没有填 provider（Anthropic 类型，95），#8 是 azure（85）。
func seedInteropChannels(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupTestDB(t, &Channel{}, &Ability{})
	for _, spec := range []struct {
		id, channelType int
		config          string
		priority        int64
	}{
		{1, common.ChannelTypeAnthropic, `{"provider":"anthropic"}`, 100},
		{2, common.ChannelTypeAnthropic, `{"provider":"anthropic"}`, 40},
		{3, common.ChannelTypeVertexAI, `{"provider":"vertex"}`, 90},
		{4, common.ChannelTypeVertexAI, `{"provider":" Vertex "}`, 30},
		{5, common.ChannelTypeAwsClaude, `{"provider":"aws"}`, 80},
		{6, common.ChannelTypeAwsClaude, `{"provider":"aws"}`, 20},
		{7, common.ChannelTypeAnthropic, ``, 95},
		{8, common.ChannelTypeAzure, `{"provider":"azure"}`, 85},
	} {
		weight := uint(1)
		channel := Channel{Id: spec.id, Type: spec.channelType, Status: common.ChannelStatusEnabled, Config: spec.config, Group: routingTestGroup, Models: routingTestModel, Priority: &spec.priority, Weight: &weight, Key: fmt.Sprintf("key-%d", spec.id)}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, db.Create(&Ability{ChannelId: spec.id, Group: routingTestGroup, Model: routingTestModel, Enabled: true, Priority: &spec.priority}).Error)
	}
	return db
}

// retriesAfter 模拟一次请求的失败重试：首选渠道失败后不断选下一个渠道并记为失败，直到选不出来。
// 返回依次重试到的渠道。有重试范围时选不出来就停止（不重新循环），与重试循环的行为一致。
func retriesAfter(t *testing.T, ctx context.Context, first int) []int {
	t.Helper()
	failed := []int{first}
	var sequence []int
	for len(failed) < 20 {
		channel, _, err := CacheGetRandomSatisfiedChannel(ctx, routingTestGroup, routingTestModel, 0, "", failed)
		if err != nil {
			return sequence
		}
		sequence = append(sequence, channel.Id)
		failed = append(failed, channel.Id)
	}
	t.Fatalf("retry did not stop: %v", sequence)
	return nil
}

var claudeScope = []string{"anthropic", "vertex", "aws"}

// 互通规则扩大后的重试范围：范围内的 provider 都可以，范围外和没有填 provider 的渠道不行。
func TestRetryProviderScopeAllows(t *testing.T) {
	root := context.Background()
	ctx := WithRetryProviderScope(WithRetryProvider(root, "anthropic"), []string{" Anthropic ", "VERTEX", "", "aws"})
	require.Equal(t, "anthropic", RetryProvider(ctx), "首选渠道的 provider 不变")
	require.Equal(t, claudeScope, RetryProviderScope(ctx), "范围里的名字规范化，空值去掉")
	require.True(t, HasRetryBoundary(ctx))

	require.True(t, RetryProviderAllows(ctx, &Channel{Id: 1, Config: `{"provider":"anthropic"}`}))
	require.True(t, RetryProviderAllows(ctx, &Channel{Id: 2, Config: `{"provider":" Vertex "}`}))
	require.True(t, RetryProviderAllows(ctx, &Channel{Id: 3, Config: `{"provider":"aws"}`}))
	require.False(t, RetryProviderAllows(ctx, &Channel{Id: 4, Config: `{"provider":"azure"}`}), "范围之外")
	require.False(t, RetryProviderAllows(ctx, &Channel{Id: 5, Type: common.ChannelTypeAnthropic}), "没有填 provider 的渠道不参与互通")
	require.False(t, RetryProviderAllows(ctx, nil))

	// 没有扩大范围时只允许首选渠道的 provider（原有行为）。
	single := WithRetryProvider(root, "anthropic")
	require.Empty(t, RetryProviderScope(single))
	require.True(t, RetryProviderAllows(single, &Channel{Config: `{"provider":"anthropic"}`}))
	require.False(t, RetryProviderAllows(single, &Channel{Config: `{"provider":"vertex"}`}))

	// 首选渠道没有填 provider：没有重试范围，范围列表不起作用。
	unbounded := WithRetryProviderScope(root, claudeScope)
	require.False(t, HasRetryBoundary(unbounded))
	require.True(t, RetryProviderAllows(unbounded, &Channel{Config: `{"provider":"azure"}`}))
	require.True(t, RetryProviderAllows(unbounded, &Channel{}))

	// 绑定了原渠道时只能是那个渠道，范围不放宽它。
	bound := WithResponseStateChannel(ctx, 1)
	require.True(t, RetryProviderAllows(bound, &Channel{Id: 1, Config: `{"provider":"anthropic"}`}))
	require.False(t, RetryProviderAllows(bound, &Channel{Id: 2, Config: `{"provider":"vertex"}`}))

	// 范围不会泄漏到别的请求。
	require.Empty(t, RetryProviderScope(root))
}

// 没有设置令牌路由时，互通规则让重试可以跨 provider：整个范围一起按优先级降档。
// 没有规则时仍只在首选渠道的 provider 内。
func TestInteropScopeLetsRetryCrossProviders(t *testing.T) {
	seedInteropChannels(t)
	root := context.Background()

	withScope := WithRetryProviderScope(WithRetryProvider(root, "anthropic"), claudeScope)
	require.Equal(t, []int{3, 5, 2, 4, 6}, retriesAfter(t, withScope, 1),
		"范围内按优先级依次降档；没有填 provider 的 #7 和范围之外的 #8 即使优先级更高也不会用")

	withoutScope := WithRetryProvider(root, "anthropic")
	require.Equal(t, []int{2}, retriesAfter(t, withoutScope, 1), "没有规则：只在 anthropic 内")

	// 首选渠道是别的 provider 时，范围同样是这一组。
	fromVertex := WithRetryProviderScope(WithRetryProvider(root, "vertex"), claudeScope)
	require.Equal(t, []int{5, 2, 4, 6}, retriesAfter(t, fromVertex, 3), "降档后不再回到更高的优先级（#1）")
}

// 令牌路由与互通规则一起生效：重试候选是两者的交集，按令牌的顺序逐个来源尝试，
// 降档按来源分别计算（换到下一个来源时从它的最高档开始）。
func TestInteropScopeWithTokenProviderRouting(t *testing.T) {
	seedInteropChannels(t)
	for name, tc := range map[string]struct {
		routing TokenProviderRouting
		first   int
		retries []int
	}{
		"只选 vertex，其余作后备":            {TokenProviderRouting{Order: []string{"vertex"}}, 3, []int{4, 1, 5, 2, 6}},
		"vertex 再 anthropic，只用选中的来源": {TokenProviderRouting{Order: []string{"vertex", "anthropic"}, Only: true}, 3, []int{4, 1, 2}},
		"只用 vertex":   {TokenProviderRouting{Order: []string{"vertex"}, Only: true}, 3, []int{4}},
		"忽略 aws":      {TokenProviderRouting{Ignore: []string{"aws"}}, 1, []int{3, 2, 4}},
		"aws 在前":      {TokenProviderRouting{Order: []string{"aws", "vertex"}}, 5, []int{6, 3, 4, 1, 2}},
		"忽略首选之外的全部来源": {TokenProviderRouting{Ignore: []string{"vertex", "aws"}}, 1, []int{2}},
	} {
		routed := routedContext(tc.routing)
		// 首选：这时还没有重试范围，只看令牌路由和管理员的优先级。
		first, _, err := CacheGetRandomSatisfiedChannel(routed, routingTestGroup, routingTestModel, 0, "")
		require.NoError(t, err, name)
		require.Equal(t, tc.first, first.Id, name)
		// 首选渠道确定后固定重试范围。
		ctx := WithRetryProviderScope(WithRetryProvider(routed, ChannelProvider(first)), claudeScope)
		require.Equal(t, tc.retries, retriesAfter(t, ctx, first.Id), name)
	}

	// 被忽略的来源不会因为互通规则被选中：范围里只剩被忽略的来源时，原因是令牌路由。
	ctx := WithRetryProviderScope(WithRetryProvider(routedContext(TokenProviderRouting{Ignore: []string{"vertex", "aws"}}), "anthropic"), claudeScope)
	_, _, err := CacheGetRandomSatisfiedChannel(ctx, routingTestGroup, routingTestModel, 0, "", []int{1, 2})
	require.ErrorIs(t, err, ErrTokenProviderRoutingNoChannel)

	// 没有规则时，令牌的顺序不能让一次请求中途换 provider：重试仍只在首选渠道的 provider 内。
	routed := routedContext(TokenProviderRouting{Order: []string{"vertex"}})
	require.Equal(t, []int{4}, retriesAfter(t, WithRetryProvider(routed, "vertex"), 3))
}

// 设置了令牌路由时，来源内部的降档规则不变：原档只补重试一次，之后只往更低的优先级，
// 同档剩下的渠道不再使用；换到下一个来源时从它的最高档重新开始。
func TestTokenRoutingKeepsDescendingRuleWithinSource(t *testing.T) {
	db := seedInteropChannels(t)
	// vertex 的最高档（90）再加两个渠道，连同 #3 一共三个。
	for _, id := range []int{9, 10} {
		priority, weight := int64(90), uint(1)
		require.NoError(t, db.Create(&Channel{Id: id, Type: common.ChannelTypeVertexAI, Status: common.ChannelStatusEnabled, Config: `{"provider":"vertex"}`, Group: routingTestGroup, Models: routingTestModel, Priority: &priority, Weight: &weight}).Error)
		require.NoError(t, db.Create(&Ability{ChannelId: id, Group: routingTestGroup, Model: routingTestModel, Enabled: true, Priority: &priority}).Error)
	}
	topTier := []int{3, 9, 10}

	for i := 0; i < 20; i++ {
		// 只用 vertex：首选在最高档，补重试一次仍在最高档，然后降到 #4，结束。最高档剩下的一个不再使用。
		routed := routedContext(TokenProviderRouting{Order: []string{"vertex"}, Only: true})
		first, _, err := CacheGetRandomSatisfiedChannel(routed, routingTestGroup, routingTestModel, 0, "")
		require.NoError(t, err)
		require.Contains(t, topTier, first.Id)
		retries := retriesAfter(t, WithRetryProviderScope(WithRetryProvider(routed, "vertex"), claudeScope), first.Id)
		require.Len(t, retries, 2)
		require.Contains(t, topTier, retries[0])
		require.NotEqual(t, first.Id, retries[0])
		require.Equal(t, 4, retries[1])

		// vertex 之后是 anthropic：anthropic 从自己的最高档（#1，100）开始，不受 vertex 已经降到 30 的影响。
		routed = routedContext(TokenProviderRouting{Order: []string{"vertex", "anthropic"}, Only: true})
		retries = retriesAfter(t, WithRetryProviderScope(WithRetryProvider(routed, "vertex"), claudeScope), 3)
		require.Len(t, retries, 4)
		require.Contains(t, []int{9, 10}, retries[0])
		require.Equal(t, []int{4, 1, 2}, retries[1:])
	}
}
