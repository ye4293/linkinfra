package controller

import (
	"github.com/songquanpeng/one-api/relay/channel/anthropic"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestClaudeRankingIncludesCacheOnce(t *testing.T) {
	usage := &anthropic.Usage{InputTokens: 100, OutputTokens: 30, CacheReadInputTokens: 200, CacheCreationInputTokens: 50}
	require.EqualValues(t, 380, claudeRankingTokens(usage))
	usage.CacheCreation = &anthropic.CacheCreation{}
	require.EqualValues(t, 380, claudeRankingTokens(usage))
	usage.CacheCreation = &anthropic.CacheCreation{Ephemeral5mInputTokens: 20, Ephemeral1hInputTokens: 30}
	require.EqualValues(t, 380, claudeRankingTokens(usage))
	require.Zero(t, claudeRankingTokens(nil))
}
