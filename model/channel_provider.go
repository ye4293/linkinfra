package model

import (
	"context"
	"strings"
)

type retryProviderContextKey struct{}

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
	provider := RetryProvider(ctx)
	return provider == "" || (channel != nil && ChannelProvider(channel) == provider)
}

func filterRetryProviderChannels(ctx context.Context, channels []Channel) []Channel {
	if RetryProvider(ctx) == "" {
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
