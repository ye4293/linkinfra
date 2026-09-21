package controller

import "github.com/songquanpeng/one-api/relay/channel/anthropic"

// Claude 原生输入不含缓存；优先采用详细创建用量，避免同时累加总数和拆分数。
func claudeRankingTokens(usage *anthropic.Usage) int64 {
	if usage == nil {
		return 0
	}
	return int64(usage.InputTokens) + int64(usage.OutputTokens) + usage.RankingCacheTokens()
}
