package model

import (
	"context"
	"strings"
)

type retryProviderContextKey struct{}
type responseStateChannelContextKey struct{}

// WithResponseStateChannel 用于服务端引用或没有 provider 的状态，固定资源而非仅固定厂商。
func WithResponseStateChannel(ctx context.Context, channelID int) context.Context {
	return context.WithValue(ctx, responseStateChannelContextKey{}, channelID)
}

func ResponseStateChannel(ctx context.Context) int {
	id, _ := ctx.Value(responseStateChannelContextKey{}).(int)
	return id
}

func HasRetryBoundary(ctx context.Context) bool {
	return RetryProvider(ctx) != "" || ResponseStateChannel(ctx) > 0
}

func ChannelProvider(channel *Channel) string {
	if channel == nil {
		return ""
	}
	cfg, err := channel.LoadConfig()
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(cfg.Provider))
}

// WithRetryProvider 在首次选渠后固定本次请求的 provider，重试时不覆盖。
func WithRetryProvider(ctx context.Context, provider string) context.Context {
	return context.WithValue(ctx, retryProviderContextKey{}, strings.ToLower(strings.TrimSpace(provider)))
}

func RetryProvider(ctx context.Context) string {
	provider, _ := ctx.Value(retryProviderContextKey{}).(string)
	return provider
}

func RetryProviderAllows(ctx context.Context, channel *Channel) bool {
	if id := ResponseStateChannel(ctx); id > 0 && (channel == nil || channel.Id != id) {
		return false
	}
	provider := RetryProvider(ctx)
	return provider == "" || (channel != nil && ChannelProvider(channel) == provider)
}

func filterRetryProviderChannels(ctx context.Context, channels []Channel) []Channel {
	if !HasRetryBoundary(ctx) {
		return channels
	}
	filtered := make([]Channel, 0, len(channels))
	for _, ch := range channels {
		if RetryProviderAllows(ctx, &ch) {
			filtered = append(filtered, ch)
		}
	}
	return filtered
}
