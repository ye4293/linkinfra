package model

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/redistest"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const (
	routingTestGroup = "default"
	routingTestModel = "claude-test"
)

// seedRoutingChannels 建三个来源的渠道（分组 default、模型 claude-test）：
//
//	anthropic：#1（优先级 100，支持 count_tokens）、#2（50）
//	vertex   ：#3（100）、#4（100）、#5（50）
//	aws      ：#6（100，支持 count_tokens），没有填 provider，来源取渠道类型名
//
// 另外三个不该被选中的渠道：#7 属于别的分组，#8 只有别的模型，#9 已禁用。
func seedRoutingChannels(t *testing.T) *gorm.DB {
	t.Helper()
	db := setupTestDB(t, &Channel{}, &Ability{})
	add := func(id, channelType int, config, group, model string, priority int64, enabled bool) {
		t.Helper()
		weight := uint(1)
		status := common.ChannelStatusEnabled
		if !enabled {
			status = common.ChannelStatusManuallyDisabled
		}
		channel := Channel{Id: id, Type: channelType, Status: status, Config: config, Group: group, Models: model, Priority: &priority, Weight: &weight, Key: fmt.Sprintf("key-%d", id)}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, db.Create(&Ability{ChannelId: id, Group: group, Model: model, Enabled: enabled, Priority: &priority}).Error)
	}
	add(1, common.ChannelTypeAnthropic, `{"provider":"anthropic","support_count_tokens":true}`, routingTestGroup, routingTestModel, 100, true)
	add(2, common.ChannelTypeAnthropic, `{"provider":"anthropic"}`, routingTestGroup, routingTestModel, 50, true)
	add(3, common.ChannelTypeVertexAI, `{"provider":"vertex"}`, routingTestGroup, routingTestModel, 100, true)
	add(4, common.ChannelTypeVertexAI, `{"provider":" Vertex "}`, routingTestGroup, routingTestModel, 100, true)
	add(5, common.ChannelTypeVertexAI, `{"provider":"vertex"}`, routingTestGroup, routingTestModel, 50, true)
	add(6, common.ChannelTypeAwsClaude, `{"support_count_tokens":true}`, routingTestGroup, routingTestModel, 100, true)
	add(7, common.ChannelTypeVertexAI, `{"provider":"vertex"}`, "vip", routingTestModel, 200, true)
	add(8, common.ChannelTypeVertexAI, `{"provider":"vertex"}`, routingTestGroup, "other-model", 200, true)
	add(9, common.ChannelTypeVertexAI, `{"provider":"vertex"}`, routingTestGroup, routingTestModel, 300, false)
	return db
}

func routedContext(routing TokenProviderRouting) context.Context {
	return WithTokenProviderRouting(context.Background(), routing)
}

// selectedSet 重复选渠，返回出现过的渠道 ID（升序）。同档按权重随机，多次选渠才能看到全部候选。
func selectedSet(t *testing.T, ctx context.Context, exclude ...int) []int {
	t.Helper()
	seen := map[int]bool{}
	for i := 0; i < 120; i++ {
		channel, keyIndex, err := CacheGetRandomSatisfiedChannel(ctx, routingTestGroup, routingTestModel, 0, "", exclude)
		require.NoError(t, err)
		require.Equal(t, -1, keyIndex)
		seen[channel.Id] = true
	}
	ids := make([]int, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// selectError 返回选渠失败的错误；选到了渠道则测试失败。
func selectError(t *testing.T, ctx context.Context, exclude ...int) error {
	t.Helper()
	channel, _, err := CacheGetRandomSatisfiedChannel(ctx, routingTestGroup, routingTestModel, 0, "", exclude)
	require.Error(t, err)
	require.Nil(t, channel)
	return err
}

// 没有设置路由：只看管理员的优先级和权重，与引入来源路由之前一致。
func TestSelectionWithoutRoutingKeepsAdminPriority(t *testing.T) {
	seedRoutingChannels(t)
	ctx := context.Background()
	require.Equal(t, []int{1, 3, 4, 6}, selectedSet(t, ctx), "最高档里三个来源都参与")
	require.Equal(t, []int{2, 5}, selectedSet(t, ctx, 1, 3, 4, 6), "最高档用完后回退到下一档")
	err := selectError(t, ctx, 1, 2, 3, 4, 5, 6)
	require.False(t, errors.Is(err, ErrTokenProviderRoutingNoChannel), "不是来源路由造成的")
	require.EqualError(t, err, "no channels available with the required priority and weight")

	// 零值路由等同于没有设置。
	require.Equal(t, []int{1, 3, 4, 6}, selectedSet(t, routedContext(TokenProviderRouting{})))
}

// order：按来源的先后分层，前一个来源没有可选渠道才用下一个；来源内部仍按管理员的优先级。
func TestSelectionFollowsTokenProviderOrder(t *testing.T) {
	seedRoutingChannels(t)

	// only 关闭：其余来源合并成最后一层作后备。
	ctx := routedContext(TokenProviderRouting{Order: []string{"vertex"}})
	require.Equal(t, []int{3, 4}, selectedSet(t, ctx))
	require.Equal(t, []int{5}, selectedSet(t, ctx, 3, 4), "同一来源的低优先级渠道先于其他来源的高优先级渠道")
	require.Equal(t, []int{1, 6}, selectedSet(t, ctx, 3, 4, 5), "vertex 用完才到其余来源，其余来源之间按管理员的优先级")
	require.Equal(t, []int{2}, selectedSet(t, ctx, 1, 3, 4, 5, 6))
	err := selectError(t, ctx, 1, 2, 3, 4, 5, 6)
	require.False(t, errors.Is(err, ErrTokenProviderRoutingNoChannel), "所有来源都用完了，不是路由挡住的")

	// only 打开：只用选中的来源，按顺序；用完就报错，不到 aws。
	ctx = routedContext(TokenProviderRouting{Order: []string{"vertex", "anthropic"}, Only: true})
	require.Equal(t, []int{3, 4}, selectedSet(t, ctx))
	require.Equal(t, []int{5}, selectedSet(t, ctx, 3, 4))
	require.Equal(t, []int{1}, selectedSet(t, ctx, 3, 4, 5))
	require.Equal(t, []int{2}, selectedSet(t, ctx, 1, 3, 4, 5))
	err = selectError(t, ctx, 1, 2, 3, 4, 5)
	require.ErrorIs(t, err, ErrTokenProviderRoutingNoChannel, "aws 还有可用渠道，但在路由之外")
	require.ErrorContains(t, err, "order=[vertex anthropic] only=true")

	// 顺序由令牌决定：把 anthropic 排在前面，结果随之改变。
	ctx = routedContext(TokenProviderRouting{Order: []string{"anthropic", "vertex"}, Only: true})
	require.Equal(t, []int{1}, selectedSet(t, ctx))
	require.Equal(t, []int{2}, selectedSet(t, ctx, 1))
	require.Equal(t, []int{3, 4}, selectedSet(t, ctx, 1, 2))

	// 只选一个来源并打开 only：只在这个来源的渠道内。
	ctx = routedContext(TokenProviderRouting{Order: []string{"vertex"}, Only: true})
	require.Equal(t, []int{3, 4}, selectedSet(t, ctx))
	require.ErrorIs(t, selectError(t, ctx, 3, 4, 5), ErrTokenProviderRoutingNoChannel)

	// 排在前面的来源当前没有渠道（下线了，或用户等级里没有）：直接用后面的来源。
	ctx = routedContext(TokenProviderRouting{Order: []string{"gone", "anthropic"}, Only: true})
	require.Equal(t, []int{1}, selectedSet(t, ctx))
	// 选中的来源一个渠道都没有：报错说明是路由的原因，不放开到其他来源。
	ctx = routedContext(TokenProviderRouting{Order: []string{"gone"}, Only: true})
	require.ErrorIs(t, selectError(t, ctx), ErrTokenProviderRoutingNoChannel)

	// 没有填 provider 的渠道，来源是渠道类型名（这里是 aws）。
	ctx = routedContext(TokenProviderRouting{Order: []string{"aws"}, Only: true})
	require.Equal(t, []int{6}, selectedSet(t, ctx))
}

// ignore：永不使用的来源，在首选、回退到低优先级、only 关闭时的后备层里都不会被选中。
func TestSelectionNeverUsesIgnoredProviders(t *testing.T) {
	seedRoutingChannels(t)

	ctx := routedContext(TokenProviderRouting{Ignore: []string{"aws"}})
	require.Equal(t, []int{1, 3, 4}, selectedSet(t, ctx))
	require.Equal(t, []int{2, 5}, selectedSet(t, ctx, 1, 3, 4))
	require.ErrorIs(t, selectError(t, ctx, 1, 2, 3, 4, 5), ErrTokenProviderRoutingNoChannel, "只剩被忽略的 aws")

	ctx = routedContext(TokenProviderRouting{Ignore: []string{"anthropic", "vertex"}})
	require.Equal(t, []int{6}, selectedSet(t, ctx))

	// 后备层里也不包含被忽略的来源。
	ctx = routedContext(TokenProviderRouting{Order: []string{"vertex"}, Ignore: []string{"aws"}})
	require.Equal(t, []int{1}, selectedSet(t, ctx, 3, 4, 5))
	require.Equal(t, []int{2}, selectedSet(t, ctx, 1, 3, 4, 5))
	require.ErrorIs(t, selectError(t, ctx, 1, 2, 3, 4, 5), ErrTokenProviderRoutingNoChannel)
}

// 重试范围（首选渠道的 provider）与令牌路由取交集；provider 重试的降档规则在来源内部照常生效。
func TestSelectionIntersectsRetryBoundaryWithRouting(t *testing.T) {
	seedRoutingChannels(t)
	bounded := func(routing TokenProviderRouting, provider string) context.Context {
		return WithRetryProvider(routedContext(routing), provider)
	}

	// 令牌把 anthropic 排在前面，但本次请求的重试范围是 vertex：只在 vertex 的渠道里选。
	ctx := bounded(TokenProviderRouting{Order: []string{"anthropic", "vertex"}}, "vertex")
	require.Equal(t, []int{3, 4}, selectedSet(t, ctx))
	// 原档补重试一次，之后只往更低的优先级；用完后停止，不到令牌允许的其他来源。
	require.Equal(t, []int{4}, selectedSet(t, ctx, 3))
	require.Equal(t, []int{5}, selectedSet(t, ctx, 3, 4))
	err := selectError(t, ctx, 3, 4, 5)
	require.False(t, errors.Is(err, ErrTokenProviderRoutingNoChannel), "剩下的渠道是被重试范围挡住的")

	// 只设置了 ignore（一层）：降档规则与没有设置路由时一样。
	ctx = bounded(TokenProviderRouting{Ignore: []string{"aws"}}, "vertex")
	require.Equal(t, []int{4}, selectedSet(t, ctx, 3))
	require.Equal(t, []int{5}, selectedSet(t, ctx, 3, 4))
	selectError(t, ctx, 3, 4, 5)
	plain := WithRetryProvider(context.Background(), "vertex")
	require.Equal(t, []int{4}, selectedSet(t, plain, 3))
	require.Equal(t, []int{5}, selectedSet(t, plain, 3, 4))
	selectError(t, plain, 3, 4, 5)

	// 重试范围内的来源被令牌忽略：没有可选渠道，原因是路由。
	ctx = bounded(TokenProviderRouting{Ignore: []string{"vertex"}}, "vertex")
	require.ErrorIs(t, selectError(t, ctx), ErrTokenProviderRoutingNoChannel)

	// 绑定了原渠道（Responses 的服务端引用）：只能是那个渠道，令牌不允许它时同样选不到。
	ctx = WithResponseStateChannel(bounded(TokenProviderRouting{Order: []string{"anthropic"}}, "vertex"), 5)
	require.Equal(t, []int{5}, selectedSet(t, ctx))
	ctx = WithResponseStateChannel(bounded(TokenProviderRouting{Order: []string{"anthropic"}, Only: true}, "vertex"), 5)
	require.ErrorIs(t, selectError(t, ctx), ErrTokenProviderRoutingNoChannel)
}

// X-Response-ID 命中的渠道：令牌路由不允许时跳过并按正常规则选渠；
// 允许时继续使用，不因为它的来源排在后面而换回前面的来源。
func TestResponseIDCachedChannelRespectsRouting(t *testing.T) {
	seedRoutingChannels(t)
	fake := redistest.Use(t)
	fake.Set(fmt.Sprintf(common.CacheClaudeRsID, "resp-aws"), "6:0")

	pick := func(ctx context.Context) (int, int) {
		t.Helper()
		channel, keyIndex, err := CacheGetRandomSatisfiedChannel(ctx, routingTestGroup, routingTestModel, 0, "resp-aws")
		require.NoError(t, err)
		return channel.Id, keyIndex
	}
	for i := 0; i < 30; i++ {
		id, keyIndex := pick(context.Background())
		require.Equal(t, 6, id, "没有设置路由：使用缓存的渠道")
		require.Equal(t, 0, keyIndex)

		id, keyIndex = pick(routedContext(TokenProviderRouting{Order: []string{"vertex"}}))
		require.Equal(t, 6, id, "aws 在后备层里，仍在允许范围内：粘性优先于顺序")
		require.Equal(t, 0, keyIndex)

		id, keyIndex = pick(routedContext(TokenProviderRouting{Ignore: []string{"aws"}}))
		require.Contains(t, []int{1, 3, 4}, id, "被忽略的来源即使命中缓存也不用")
		require.Equal(t, -1, keyIndex, "缓存渠道的 key 索引不能带到新选的渠道上")

		id, keyIndex = pick(routedContext(TokenProviderRouting{Order: []string{"vertex"}, Only: true}))
		require.Contains(t, []int{3, 4}, id, "only 打开时不在选中来源里的缓存渠道不用")
		require.Equal(t, -1, keyIndex)
	}
}

// count_tokens 的选渠使用同一套令牌路由（包括顺序），但不受重试范围限制。
func TestCapabilitySelectionFollowsRouting(t *testing.T) {
	seedRoutingChannels(t)
	capable := func(ctx context.Context) []int {
		t.Helper()
		seen := map[int]bool{}
		for i := 0; i < 80; i++ {
			channel, err := CacheGetRandomSatisfiedChannelWithCapability(ctx, routingTestGroup, routingTestModel, FilterSupportCountTokens, 0, "")
			require.NoError(t, err)
			seen[channel.Id] = true
		}
		ids := make([]int, 0, len(seen))
		for id := range seen {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		return ids
	}

	require.Equal(t, []int{1, 6}, capable(context.Background()), "只有 #1 和 #6 支持 count_tokens")
	require.Equal(t, []int{1, 6}, capable(WithRetryProvider(context.Background(), "vertex")), "不受重试范围限制")
	require.Equal(t, []int{1}, capable(routedContext(TokenProviderRouting{Ignore: []string{"aws"}})))
	require.Equal(t, []int{6}, capable(routedContext(TokenProviderRouting{Order: []string{"aws"}})), "按令牌的顺序")
	require.Equal(t, []int{1}, capable(routedContext(TokenProviderRouting{Order: []string{"anthropic", "aws"}, Only: true})))
	require.Equal(t, []int{1, 6}, capable(routedContext(TokenProviderRouting{Order: []string{"vertex"}})), "vertex 的渠道不支持，落到后备层")

	_, err := CacheGetRandomSatisfiedChannelWithCapability(routedContext(TokenProviderRouting{Order: []string{"vertex"}, Only: true}),
		routingTestGroup, routingTestModel, FilterSupportCountTokens, 0, "")
	require.ErrorIs(t, err, ErrTokenProviderRoutingNoChannel, "支持 count_tokens 的渠道都在路由之外")

	// 没有任何渠道支持该能力：与路由无关，报错不变。
	_, err = CacheGetRandomSatisfiedChannelWithCapability(context.Background(), routingTestGroup, "other-model", FilterSupportCountTokens, 0, "")
	require.EqualError(t, err, "no channels available with required capability")
	_, err = CacheGetRandomSatisfiedChannelWithCapability(context.Background(), routingTestGroup, "missing-model", FilterSupportCountTokens, 0, "")
	require.EqualError(t, err, "no priorities available")
}

// 来源名：填了 provider 用 provider，没填用渠道类型名，统一小写；与模型广场的 source_key 一致。
// provider 的读取不依赖 config 其他字段解析成功。
func TestChannelSourceAndProviderRead(t *testing.T) {
	for name, tc := range map[string]struct {
		channel  *Channel
		provider string
		source   string
	}{
		"填了 provider":         {&Channel{Type: common.ChannelTypeVertexAI, Config: `{"provider":" Vertex "}`}, "vertex", "vertex"},
		"provider 是标准名称的别的写法": {&Channel{Type: common.ChannelTypeOpenAI, Config: `{"provider":"AZURE"}`}, "azure", "azure"},
		"没填，Anthropic 类型":     {&Channel{Type: common.ChannelTypeAnthropic}, "", "anthropic"},
		"没填，Vertex 类型":        {&Channel{Type: common.ChannelTypeVertexAI, Config: `{"region":"global"}`}, "", "vertex ai"},
		"没填，AWS 类型":           {&Channel{Type: common.ChannelTypeAwsClaude}, "", "aws"},
		"没填，未知类型":             {&Channel{Type: 9999}, "", "channel-type-9999"},
		"provider 只有空格":       {&Channel{Type: common.ChannelTypeAzure, Config: `{"provider":"  "}`}, "", "azure"},
		"config 不是 JSON":      {&Channel{Type: common.ChannelTypeAzure, Config: `not json`}, "", "azure"},
	} {
		require.Equal(t, tc.provider, ChannelProvider(tc.channel), name)
		require.Equal(t, tc.source, ChannelSource(tc.channel), name)
		require.Equal(t, MetricsSourceKey(common.GetCatalogProvider(tc.channel.Type, ChannelProvider(tc.channel))), ChannelSource(tc.channel), name)
	}
	require.Equal(t, "", ChannelProvider(nil))
	require.Equal(t, "", ChannelSource(nil))

	// config 的其他字段类型不对（Vertex 的区域映射被存成了字符串）：LoadConfig 报错，
	// 转发仍在用解析出的部分配置；provider 照常读得出来，重试边界和令牌的 ignore 才对它生效。
	broken := &Channel{Type: common.ChannelTypeVertexAI, Config: `{"provider":"vertex","vertex_model_region":"{bad json"}`}
	_, err := broken.LoadConfig()
	require.Error(t, err)
	require.Equal(t, "vertex", ChannelProvider(broken))
	require.Equal(t, "vertex", ChannelSource(broken))
	require.False(t, TokenProviderRouting{Ignore: []string{"vertex"}}.AllowsChannel(broken))
	require.True(t, RetryProviderAllows(WithRetryProvider(context.Background(), "vertex"), broken))

	// 保存渠道时的校验：provider 必须是字符串，且不超过 128 个字符。
	require.True(t, HasInvalidChannelProvider(&Channel{Config: `{"provider":123}`}))
	require.True(t, HasInvalidChannelProvider(&Channel{Config: `{"provider":["vertex"]}`}))
	require.True(t, HasInvalidChannelProvider(&Channel{Config: `{"provider":"vertex","provider":true}`}), "重复的 key 里有一次不是字符串")
	require.True(t, HasInvalidChannelProvider(&Channel{Config: `{"provider":"` + strings.Repeat("a", 129) + `"}`}))
	require.False(t, HasInvalidChannelProvider(&Channel{Config: `{"provider":" ` + strings.Repeat("a", 128) + ` "}`}))
	require.False(t, HasInvalidChannelProvider(&Channel{Config: `{"provider":" Vertex "}`}))
	require.False(t, HasInvalidChannelProvider(&Channel{Config: `{"provider":null}`}))
	require.False(t, HasInvalidChannelProvider(&Channel{Config: `{"provider":"vertex","vertex_model_region":"{bad json"}`}), "别的字段写错不算 provider 的问题")
	require.False(t, HasInvalidChannelProvider(&Channel{Config: `{"billing_tag":123}`}))
	require.False(t, HasInvalidChannelProvider(&Channel{Config: `not json`}))
	require.False(t, HasInvalidChannelProvider(&Channel{Config: `"vertex"`}))
	require.False(t, HasInvalidChannelProvider(&Channel{}))
	require.False(t, HasInvalidChannelProvider(nil))
}

// 令牌表单的来源列表：只含该分组下启用的渠道提供的来源，每个来源带可用的模型。
func TestListTokenSources(t *testing.T) {
	db := seedRoutingChannels(t)
	// #1 再多一个模型；#2 的能力被单独停用。
	priority := int64(100)
	require.NoError(t, db.Create(&Ability{ChannelId: 1, Group: routingTestGroup, Model: "claude-extra", Enabled: true, Priority: &priority}).Error)
	require.NoError(t, db.Create(&Ability{ChannelId: 1, Group: routingTestGroup, Model: " ", Enabled: true, Priority: &priority}).Error)
	require.NoError(t, db.Create(&Ability{ChannelId: 2, Group: routingTestGroup, Model: "claude-disabled", Enabled: false, Priority: &priority}).Error)
	// 渠道已禁用但能力记录还是启用状态：两处不同步时以渠道状态为准，与选渠一致。
	weight := uint(1)
	require.NoError(t, db.Create(&Channel{Id: 10, Type: common.ChannelTypeOpenRouter, Status: common.ChannelStatusManuallyDisabled, Config: `{"provider":"ghost"}`, Group: routingTestGroup, Models: routingTestModel, Priority: &priority, Weight: &weight}).Error)
	require.NoError(t, db.Create(&Ability{ChannelId: 10, Group: routingTestGroup, Model: routingTestModel, Enabled: true, Priority: &priority}).Error)

	sources, err := ListTokenSources(routingTestGroup)
	require.NoError(t, err)
	require.Equal(t, []TokenSource{
		{Name: "anthropic", DisplayName: "Anthropic", Models: []string{"claude-extra", routingTestModel}},
		{Name: "aws", DisplayName: "AWS", Models: []string{routingTestModel}},
		{Name: "vertex", DisplayName: "vertex", Models: []string{routingTestModel, "other-model"}},
	}, sources, "禁用的渠道（#9）和停用的能力不计入")

	sources, err = ListTokenSources("vip")
	require.NoError(t, err)
	require.Equal(t, []TokenSource{{Name: "vertex", DisplayName: "vertex", Models: []string{routingTestModel}}}, sources)

	sources, err = ListTokenSources("nobody")
	require.NoError(t, err)
	require.Empty(t, sources)

	// 来源名和令牌路由用的是同一个名字：列表里的每个来源都能被 ignore 挡住。
	for _, source := range []string{"anthropic", "aws", "vertex"} {
		ctx := routedContext(TokenProviderRouting{Order: []string{source}, Only: true})
		channel, _, err := CacheGetRandomSatisfiedChannel(ctx, routingTestGroup, routingTestModel, 0, "")
		require.NoError(t, err, source)
		require.Equal(t, source, ChannelSource(channel))
	}
}
