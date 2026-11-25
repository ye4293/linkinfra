package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
	relaymodel "github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/service"
	"github.com/stretchr/testify/require"
)

func TestRetryStaysWithinOriginalProvider(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	add := func(id int, provider, group, name string, priority int64, enabled bool) {
		t.Helper()
		weight := uint(1)
		cfg, err := json.Marshal(model.ChannelConfig{Provider: provider})
		require.NoError(t, err)
		status := common.ChannelStatusEnabled
		if !enabled {
			status = common.ChannelStatusManuallyDisabled
		}
		channel := model.Channel{Id: id, Type: common.ChannelTypeOpenAI, Status: status, Config: string(cfg), Group: group, Models: name, Priority: &priority, Weight: &weight}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, db.Create(&model.Ability{ChannelId: id, Group: group, Model: name, Enabled: enabled, Priority: &priority}).Error)
	}
	add(1, "openai", "default", "gpt-test", 100, true)
	add(2, "azure", "default", "gpt-test", 100, true)
	add(3, "", "default", "gpt-test", 100, true)
	add(4, " OpenAI ", "default", "gpt-test", 90, true)
	add(5, "openai", "default", "gpt-test", 80, true)
	add(6, "openai", "private", "gpt-test", 200, true)
	add(7, "openai", "default", "other-model", 200, true)
	add(8, "openai", "default", "gpt-test", 200, false)

	ctx := model.WithRetryProvider(context.Background(), "openai")
	failed := []int{1}
	channel, err := selectRetryChannel(ctx, "default", "gpt-test", &failed)
	require.NoError(t, err)
	require.Equal(t, 4, channel.Id, "must skip higher-priority Azure/unconfigured channels")
	failed = append(failed, channel.Id)
	channel, err = selectRetryChannel(ctx, "default", "gpt-test", &failed)
	require.NoError(t, err)
	require.Equal(t, 5, channel.Id)
	failed = append(failed, channel.Id)
	channel, err = selectRetryChannel(ctx, "default", "gpt-test", &failed)
	require.Error(t, err)
	require.Nil(t, channel)
	require.Equal(t, []int{1, 4, 5}, failed, "must not restart the failed-channel cycle")

	azure := model.WithRetryProvider(context.Background(), "AZURE")
	failed = []int{2}
	channel, err = selectRetryChannel(azure, "default", "gpt-test", &failed)
	require.Error(t, err)
	require.Nil(t, channel)

	// 未填写 provider：保留之前的跨渠道及耗尽后重置行为。
	failed = []int{1, 2, 3, 4, 5}
	channel, err = selectRetryChannel(context.Background(), "default", "gpt-test", &failed)
	require.NoError(t, err)
	require.Contains(t, []int{1, 2, 3}, channel.Id)
	require.Empty(t, failed)
}

func TestResponsesRetryDelay(t *testing.T) {
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, seconds, milliseconds string
		attempt                     int
		want                        time.Duration
		blocked                     bool
	}{
		{name: "default", attempt: 1, want: 2 * time.Second},
		{name: "backoff", attempt: 4, want: 16 * time.Second},
		{name: "backoff_bounded", attempt: 100, want: 16 * time.Second},
		{name: "seconds", seconds: "12", want: 12 * time.Second},
		{name: "milliseconds", seconds: "12", milliseconds: "250", want: 250 * time.Millisecond},
		{name: "invalid", seconds: "invalid", milliseconds: "-1", want: 2 * time.Second},
		{name: "date", seconds: now.Add(10 * time.Second).Format(http.TimeFormat), want: 10 * time.Second},
		{name: "past_date", seconds: now.Add(-time.Second).Format(http.TimeFormat), want: time.Millisecond},
		{name: "zero", seconds: "0", want: time.Millisecond},
		{name: "long_seconds", seconds: "31", blocked: true},
		{name: "long_milliseconds", milliseconds: "30001", blocked: true},
		{name: "overflow_duration", seconds: "9223372036854775807", blocked: true},
		{name: "long_date", seconds: now.Add(time.Minute).Format(http.TimeFormat), blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delay, err := responsesRetryDelay(tc.seconds, tc.milliseconds, tc.attempt, now)
			if tc.blocked {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, delay)
		})
	}
}

func TestProviderRetryOnceAtHighestThenLowerPriority(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	for _, spec := range []struct {
		id       int
		priority int64
		provider string
	}{
		{1, 100, "openai"}, {2, 100, "openai"}, {9, 100, "openai"},
		{3, 90, "azure"}, {4, 80, " OpenAI "}, {5, 80, "openai"},
		{6, 0, "openai"}, {7, 0, "openai"}, {8, -10, "azure"},
	} {
		weight := uint(1)
		channel := model.Channel{Id: spec.id, Status: common.ChannelStatusEnabled, Group: "default", Models: "gpt-test", Priority: &spec.priority, Weight: &weight, Config: `{"provider":"` + spec.provider + `"}`}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, db.Create(&model.Ability{ChannelId: spec.id, Group: "default", Model: "gpt-test", Enabled: true, Priority: &spec.priority}).Error)
	}
	ctx := model.WithRetryProvider(context.Background(), "openai")
	failed := []int{1}
	channel, err := selectRetryChannel(ctx, "default", "gpt-test", &failed)
	require.NoError(t, err)
	require.Contains(t, []int{2, 9}, channel.Id, "首次失败后应先在最高档补重试一次")
	failed = append(failed, channel.Id)
	// 自动禁用失败渠道后，仍以它的优先级继续降级，不能从最高档重新开始。
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("status", common.ChannelStatusAutoDisabled).Error)
	require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", channel.Id).Update("enabled", false).Error)
	channel, err = selectRetryChannel(ctx, "default", "gpt-test", &failed)
	require.NoError(t, err)
	require.Contains(t, []int{4, 5}, channel.Id, "最高档补重试失败后，即使仍有同档渠道也必须降级，并跳过其他 provider")
	failed = append(failed, channel.Id)
	channel, err = selectRetryChannel(ctx, "default", "gpt-test", &failed)
	require.NoError(t, err)
	require.Contains(t, []int{6, 7}, channel.Id, "之后每次失败继续降到下一档")
	failed = append(failed, channel.Id)
	channel, err = selectRetryChannel(ctx, "default", "gpt-test", &failed)
	require.Error(t, err)
	require.Nil(t, channel, "降级耗尽后不能返回高档或跨 provider")
	require.Len(t, failed, 4)

	// 不带 provider 时保留同档补重试；首次选渠也仍从最高档开始。
	failed = []int{1}
	channel, err = selectRetryChannel(context.Background(), "default", "gpt-test", &failed)
	require.NoError(t, err)
	require.Contains(t, []int{2, 9}, channel.Id)
	channel, _, err = model.CacheGetRandomSatisfiedChannel(ctx, "default", "gpt-test", 0, "")
	require.NoError(t, err)
	require.Contains(t, []int{1, 2, 9}, channel.Id)

	// 原渠道绑定不能因优先级降级而被放宽。
	failed = []int{1}
	channel, err = selectRetryChannel(model.WithResponseStateChannel(ctx, 1), "default", "gpt-test", &failed)
	require.Error(t, err)
	require.Nil(t, channel)
}

func TestProviderRetryHighestTierHasOnlyOneExtraAttempt(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	priority, weight := int64(100), uint(1)
	for id := 1; id <= 3; id++ {
		channel := model.Channel{Id: id, Status: common.ChannelStatusEnabled, Group: "default", Models: "gpt-test", Priority: &priority, Weight: &weight, Config: `{"provider":"openai"}`}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, db.Create(&model.Ability{ChannelId: id, Group: "default", Model: "gpt-test", Enabled: true, Priority: &priority}).Error)
	}
	ctx := model.WithRetryProvider(context.Background(), "openai")
	failed := []int{1}
	channel, err := selectRetryChannel(ctx, "default", "gpt-test", &failed)
	require.NoError(t, err)
	require.Contains(t, []int{2, 3}, channel.Id)
	failed = append(failed, channel.Id)
	channel, err = selectRetryChannel(ctx, "default", "gpt-test", &failed)
	require.Error(t, err)
	require.Nil(t, channel, "没有更低档时应停止，不能继续消耗最高档的剩余渠道")
	require.Len(t, failed, 2)
}

// 令牌设置了来源路由时的失败重试：按令牌的顺序逐个来源尝试，被忽略和没有选中的来源不会被重试到；
// 用完后在路由之内重新循环，不会换到路由之外的来源。
func TestRetryFollowsTokenProviderRouting(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	// 这些渠道都没有填 provider：首选渠道不产生重试范围，重试可以跨来源，来源取渠道类型名。
	for _, spec := range []struct {
		id, channelType int
		priority        int64
	}{
		{1, common.ChannelTypeAnthropic, 100}, {2, common.ChannelTypeAnthropic, 50},
		{3, common.ChannelTypeAwsClaude, 100}, {4, common.ChannelTypeAwsClaude, 50},
		{5, common.ChannelTypeOpenRouter, 200},
	} {
		weight := uint(1)
		channel := model.Channel{Id: spec.id, Type: spec.channelType, Status: common.ChannelStatusEnabled, Group: "default", Models: "claude-test", Priority: &spec.priority, Weight: &weight}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, db.Create(&model.Ability{ChannelId: spec.id, Group: "default", Model: "claude-test", Enabled: true, Priority: &spec.priority}).Error)
	}
	// retrySequence 从首选渠道失败开始，依次重试并把选中的渠道记为失败，返回重试到的渠道。
	retrySequence := func(ctx context.Context, first, attempts int) []int {
		t.Helper()
		failed := []int{first}
		var sequence []int
		for i := 0; i < attempts; i++ {
			channel, err := selectRetryChannel(ctx, "default", "claude-test", &failed)
			require.NoError(t, err)
			sequence = append(sequence, channel.Id)
			failed = appendUniqueChannelID(failed, channel.Id)
		}
		return sequence
	}
	routed := func(routing model.TokenProviderRouting) context.Context {
		return model.WithTokenProviderRouting(context.Background(), routing)
	}

	// 没有设置路由：按管理员的优先级跨来源重试（原有行为）。最后两个渠道同档，先后随机。
	unrouted := retrySequence(context.Background(), 1, 4)
	require.Equal(t, []int{5, 3}, unrouted[:2])
	require.ElementsMatch(t, []int{2, 4}, unrouted[2:])

	// aws 在前、anthropic 在后，只用这两个来源：先把 aws 用完，再到 anthropic；openrouter 优先级最高也不会用。
	ctx := routed(model.TokenProviderRouting{Order: []string{"aws", "anthropic"}, Only: true})
	require.Equal(t, []int{4, 1, 2}, retrySequence(ctx, 3, 3))
	// 用完后在路由之内从头循环：重新从 aws 开始，仍然不到 openrouter。
	require.Equal(t, []int{4, 1, 2, 3, 4, 1}, retrySequence(ctx, 3, 6))

	// only 关闭：选中的来源用完后到其余来源，其余来源之间按管理员的优先级。
	ctx = routed(model.TokenProviderRouting{Order: []string{"aws"}})
	require.Equal(t, []int{4, 5, 1, 2}, retrySequence(ctx, 3, 4))

	// 忽略的来源在重试里不出现，包括循环之后。
	ctx = routed(model.TokenProviderRouting{Ignore: []string{"openrouter", "aws"}})
	require.Equal(t, []int{2, 1, 2, 1}, retrySequence(ctx, 1, 4))

	// 路由之内只有一个渠道：重试回到这个渠道本身，不会换到路由之外。
	ctx = routed(model.TokenProviderRouting{Order: []string{"openrouter"}, Only: true})
	require.Equal(t, []int{5, 5}, retrySequence(ctx, 5, 2))
}

// 首选渠道填了 provider 时，重试范围仍是这一个 provider；令牌路由只能在范围内进一步收窄，
// 降档规则（原档补重试一次，之后只往更低的优先级）不变。
func TestRetryBoundaryStillAppliesWithTokenProviderRouting(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	for _, spec := range []struct {
		id       int
		priority int64
		provider string
	}{
		{1, 100, "anthropic"}, {2, 100, "anthropic"}, {3, 80, "anthropic"}, {4, 60, "anthropic"},
		{5, 100, "vertex"}, {6, 80, "vertex"},
	} {
		weight := uint(1)
		channel := model.Channel{Id: spec.id, Status: common.ChannelStatusEnabled, Group: "default", Models: "claude-test", Priority: &spec.priority, Weight: &weight, Config: `{"provider":"` + spec.provider + `"}`}
		require.NoError(t, db.Create(&channel).Error)
		require.NoError(t, db.Create(&model.Ability{ChannelId: spec.id, Group: "default", Model: "claude-test", Enabled: true, Priority: &spec.priority}).Error)
	}
	for name, routing := range map[string]model.TokenProviderRouting{
		"没有设置路由":       {},
		"anthropic 在前": {Order: []string{"anthropic", "vertex"}, Only: true},
		"vertex 在前":    {Order: []string{"vertex", "anthropic"}},
		"只忽略别的来源":      {Ignore: []string{"aws"}},
	} {
		ctx := model.WithRetryProvider(model.WithTokenProviderRouting(context.Background(), routing), "anthropic")
		failed := []int{1}
		channel, err := selectRetryChannel(ctx, "default", "claude-test", &failed)
		require.NoError(t, err, name)
		require.Equal(t, 2, channel.Id, "%s：原档补重试一次", name)
		failed = append(failed, channel.Id)
		channel, err = selectRetryChannel(ctx, "default", "claude-test", &failed)
		require.NoError(t, err, name)
		require.Equal(t, 3, channel.Id, "%s：之后降档，不到 vertex", name)
		failed = append(failed, channel.Id)
		channel, err = selectRetryChannel(ctx, "default", "claude-test", &failed)
		require.NoError(t, err, name)
		require.Equal(t, 4, channel.Id, name)
		failed = append(failed, channel.Id)
		channel, err = selectRetryChannel(ctx, "default", "claude-test", &failed)
		require.Error(t, err, name)
		require.Nil(t, channel, "%s：范围内用完后停止，不重新循环，也不到令牌允许的其他来源", name)
		require.Equal(t, []int{1, 2, 3, 4}, failed, name)
	}

	// 重试范围内的来源被令牌忽略时选不到渠道（首选阶段就会被挡住，这里确认重试也不会绕过）。
	ctx := model.WithRetryProvider(model.WithTokenProviderRouting(context.Background(), model.TokenProviderRouting{Ignore: []string{"anthropic"}}), "anthropic")
	failed := []int{1}
	channel, err := selectRetryChannel(ctx, "default", "claude-test", &failed)
	require.ErrorIs(t, err, model.ErrTokenProviderRoutingNoChannel)
	require.Nil(t, channel)
}

// 重试换到另一个 provider 后因为不可重试的错误停止时，返回首次尝试的错误；
// 没有互通规则、或错误来自首选渠道的 provider 时，仍返回当前的错误。
func TestErrorAfterCrossProviderRetry(t *testing.T) {
	first := &relaymodel.ErrorWithStatusCode{StatusCode: http.StatusTooManyRequests, Error: relaymodel.Error{Message: "rate limited", Type: "rate_limit_error"}}
	current := &relaymodel.ErrorWithStatusCode{StatusCode: http.StatusBadRequest, Error: relaymodel.Error{Message: "feature not supported", Type: "invalid_request_error"}}
	newContext := func(firstProvider string, scope []string, currentProvider string) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx := context.Background()
		if firstProvider != "" {
			ctx = model.WithRetryProvider(ctx, firstProvider)
		}
		if scope != nil {
			ctx = model.WithRetryProviderScope(ctx, scope)
		}
		c.Request = httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
		c.Set(service.ChannelProviderContextKey, currentProvider)
		return c
	}
	scope := []string{"anthropic", "vertex"}

	// 互通规则生效，错误来自另一个 provider：返回首次的错误。
	result, restored := errorAfterCrossProviderRetry(newContext("anthropic", scope, "vertex"), first, current)
	require.True(t, restored)
	require.Equal(t, http.StatusTooManyRequests, result.StatusCode)
	require.Equal(t, "rate limited", result.Error.Message)
	// 返回的是副本：调用方改写文案不会改到原来的错误。
	result.Error.Message = "rewritten"
	require.Equal(t, "rate limited", first.Error.Message)

	for name, c := range map[string]*gin.Context{
		"错误来自首选渠道的 provider": newContext("anthropic", scope, "anthropic"),
		"没有互通规则":             newContext("anthropic", nil, "vertex"),
		"首选渠道没有填 provider":   newContext("", scope, "vertex"),
		"没有重试范围":             newContext("", nil, "vertex"),
		"读不到当前渠道的 provider":  newContext("anthropic", scope, ""),
	} {
		result, restored := errorAfterCrossProviderRetry(c, first, current)
		require.False(t, restored, name)
		require.Same(t, current, result, name)
	}

	// 没有首次的错误可用时返回当前的错误。
	result, restored = errorAfterCrossProviderRetry(newContext("anthropic", scope, "vertex"), nil, current)
	require.False(t, restored)
	require.Same(t, current, result)
}
