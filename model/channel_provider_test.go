package model

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProviderConfigAndRequestScope(t *testing.T) {
	channel := &Channel{Id: 1, Config: `{"provider":" OpenAI ","api_version":"test","support_count_tokens":true}`}
	cfg, err := channel.LoadConfig()
	require.NoError(t, err)
	require.Equal(t, "test", cfg.APIVersion)
	require.True(t, cfg.SupportCountTokens)
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	channel.Config = string(raw)
	require.Equal(t, "openai", ChannelProvider(channel))
	root := context.Background()
	ctx := WithRetryProvider(root, ChannelProvider(channel))
	require.True(t, RetryProviderAllows(ctx, channel))
	require.False(t, RetryProviderAllows(ctx, &Channel{Id: 2, Config: `{"provider":"azure"}`}))
	require.False(t, RetryProviderAllows(ctx, &Channel{Id: 3}))
	require.False(t, RetryProviderAllows(ctx, nil))
	require.Equal(t, "", RetryProvider(root), "provider must not leak to other requests")
	require.True(t, RetryProviderAllows(root, &Channel{Config: `{"provider":"azure"}`}))
	// 当前请求固定最初的 provider，不跟随可变渠道配置改变。
	channel.Config = `{"provider":"azure"}`
	require.Equal(t, "openai", RetryProvider(ctx))
	require.False(t, RetryProviderAllows(ctx, channel))
}
