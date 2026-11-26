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

// 没有设置令牌路由时，互通规则让重试可以跨 provider：范围内的渠道按管理员的优先级依次尝试，
// 降档规则按来源分别计算。没有规则时仍只在首选渠道的 provider 内。
func TestInteropScopeLetsRetryCrossProviders(t *testing.T) {
	seedInteropChannels(t)
	root := context.Background()

	withScope := WithRetryProviderScope(WithRetryProvider(root, "anthropic"), claudeScope)
	require.Equal(t, []int{3, 5, 2, 4, 6}, retriesAfter(t, withScope, 1),
		"范围内按优先级依次尝试；没有填 provider 的 #7 和范围之外的 #8 即使优先级更高也不会用")

	withoutScope := WithRetryProvider(root, "anthropic")
	require.Equal(t, []int{2}, retriesAfter(t, withoutScope, 1), "没有规则：只在 anthropic 内")

	// 首选渠道不是优先级最高的那个（例如亲和规则命中了 vertex 的 #3）：vertex 失败只限制 vertex 自己，
	// anthropic 还没有试过，优先级更高的 #1 照常可用。
	fromVertex := WithRetryProviderScope(WithRetryProvider(root, "vertex"), claudeScope)
	require.Equal(t, []int{1, 5, 2, 4, 6}, retriesAfter(t, fromVertex, 3))
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

// 互通范围里的几个 provider 配在同一个优先级（常见的配法）时，降档规则按来源分别计算：
// 一个来源失败两次之后只用它更低优先级的渠道，不影响其他来源同一优先级的渠道。
// 否则直连失败后同档只再试一次（还可能随机到同一个来源），其他来源同档的渠道轮不到。
func TestInteropScopeDescendsPerSourceAtEqualPriority(t *testing.T) {
	db := setupTestDB(t, &Channel{}, &Ability{})
	add := func(id int, provider string, priority int64) {
		t.Helper()
		weight := uint(1)
		require.NoError(t, db.Create(&Channel{Id: id, Type: common.ChannelTypeAnthropic, Status: common.ChannelStatusEnabled, Config: `{"provider":"` + provider + `"}`, Group: routingTestGroup, Models: routingTestModel, Priority: &priority, Weight: &weight}).Error)
		require.NoError(t, db.Create(&Ability{ChannelId: id, Group: routingTestGroup, Model: routingTestModel, Enabled: true, Priority: &priority}).Error)
	}
	// 三个来源在最高档（100）各有渠道：anthropic 三个，vertex、aws 各两个；anthropic 另有一个低档（50）的。
	add(1, "anthropic", 100)
	add(2, "anthropic", 100)
	add(3, "anthropic", 100)
	add(4, "vertex", 100)
	add(5, "vertex", 100)
	add(6, "aws", 100)
	add(7, "aws", 100)
	add(8, "anthropic", 50)
	sourceOf := map[int]string{1: "anthropic", 2: "anthropic", 3: "anthropic", 4: "vertex", 5: "vertex", 6: "aws", 7: "aws", 8: "anthropic"}
	sourcesOf := func(ids []int) []string {
		sources := make([]string, 0, len(ids))
		for _, id := range ids {
			sources = append(sources, sourceOf[id])
		}
		return sources
	}

	for i := 0; i < 40; i++ {
		ctx := WithRetryProviderScope(WithRetryProvider(context.Background(), "anthropic"), claudeScope)
		retries := retriesAfter(t, ctx, 1)
		// anthropic 在最高档只补重试一次（#2 或 #3 之一），vertex 和 aws 的四个渠道都会轮到，最后才是低档的 #8。
		require.Len(t, retries, 6, "retries: %v", retries)
		require.Equal(t, 8, retries[5], "低档的渠道排在最后: %v", retries)
		require.ElementsMatch(t, []string{"anthropic", "vertex", "vertex", "aws", "aws"}, sourcesOf(retries[:5]), "retries: %v", retries)
	}

	// 令牌只设置了 ignore（仍是一层）时同样按来源分别计算。
	for i := 0; i < 20; i++ {
		ctx := WithRetryProviderScope(WithRetryProvider(routedContext(TokenProviderRouting{Ignore: []string{"aws"}}), "anthropic"), claudeScope)
		retries := retriesAfter(t, ctx, 1)
		require.Len(t, retries, 4, "retries: %v", retries)
		require.Equal(t, 8, retries[3])
		require.ElementsMatch(t, []string{"anthropic", "vertex", "vertex"}, sourcesOf(retries[:3]), "retries: %v", retries)
	}

	// only 关闭时的后备层里有多个来源，也按来源分别计算：vertex 用完后，anthropic 和 aws 同档的渠道都会轮到。
	for i := 0; i < 20; i++ {
		routed := routedContext(TokenProviderRouting{Order: []string{"vertex"}})
		ctx := WithRetryProviderScope(WithRetryProvider(routed, "vertex"), claudeScope)
		retries := retriesAfter(t, ctx, 4)
		// vertex 的另一个渠道，然后是后备层：anthropic 最高档两个（首次加补一次）、aws 两个，最后是低档的 #8。
		require.Len(t, retries, 6, "retries: %v", retries)
		require.Equal(t, 5, retries[0])
		require.Equal(t, 8, retries[5])
		require.ElementsMatch(t, []string{"anthropic", "anthropic", "aws", "aws"}, sourcesOf(retries[1:5]), "retries: %v", retries)
	}

	// 没有互通规则（范围只有首选渠道的 provider）时，规则与之前完全一样：同档补一次，然后降档。
	for i := 0; i < 20; i++ {
		retries := retriesAfter(t, WithRetryProvider(context.Background(), "anthropic"), 1)
		require.Len(t, retries, 2, "retries: %v", retries)
		require.Contains(t, []int{2, 3}, retries[0])
		require.Equal(t, 8, retries[1])
	}
}

// 按来源分别计算时，同一个来源每失败一次就再降一档：以它已失败渠道里最低的优先级为上界，
// 降下去之后那一档剩下的渠道也不再使用（与只有一个 provider 时的规则相同）。
func TestInteropScopeDescendsOneTierPerFailureWithinSource(t *testing.T) {
	db := setupTestDB(t, &Channel{}, &Ability{})
	for _, spec := range []struct {
		id       int
		provider string
		priority int64
	}{
		{1, "anthropic", 100}, {2, "anthropic", 100},
		{3, "anthropic", 80}, {4, "anthropic", 80},
		{5, "anthropic", 60}, {6, "anthropic", 60},
		{7, "vertex", 100},
	} {
		weight := uint(1)
		require.NoError(t, db.Create(&Channel{Id: spec.id, Type: common.ChannelTypeAnthropic, Status: common.ChannelStatusEnabled, Config: `{"provider":"` + spec.provider + `"}`, Group: routingTestGroup, Models: routingTestModel, Priority: &spec.priority, Weight: &weight}).Error)
		require.NoError(t, db.Create(&Ability{ChannelId: spec.id, Group: routingTestGroup, Model: routingTestModel, Enabled: true, Priority: &spec.priority}).Error)
	}
	for i := 0; i < 40; i++ {
		ctx := WithRetryProviderScope(WithRetryProvider(context.Background(), "anthropic"), []string{"anthropic", "vertex"})
		retries := retriesAfter(t, ctx, 1)
		// 最高档：anthropic 补重试一次（#2），vertex 的 #7 也会轮到，两者先后随机。
		// 之后 anthropic 每失败一次降一档：80 档和 60 档各只用一个渠道。
		require.Len(t, retries, 4, "retries: %v", retries)
		require.ElementsMatch(t, []int{2, 7}, retries[:2], "retries: %v", retries)
		require.Contains(t, []int{3, 4}, retries[2], "retries: %v", retries)
		require.Contains(t, []int{5, 6}, retries[3], "retries: %v", retries)
	}
}
