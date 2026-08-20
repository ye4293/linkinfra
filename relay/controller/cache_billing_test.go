package controller

import (
	"testing"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/relay/channel/anthropic"
	"github.com/songquanpeng/one-api/relay/channel/openai"
)

func TestOpenAIResponseCacheReadAndWriteBilling(t *testing.T) {
	const modelName = "cache-billing-test-model"
	oldModelRatio, hadModelRatio := common.ModelRatio[modelName]
	oldCompletionRatio, hadCompletionRatio := common.CompletionRatio[modelName]
	oldCacheRatio, hadCacheRatio := common.CacheRatio[modelName]
	oldCreateRatio, hadCreateRatio := common.CreateCacheRatio[modelName]
	oldQuotaPerUnit := config.QuotaPerUnit
	t.Cleanup(func() {
		restoreRatio(common.ModelRatio, modelName, oldModelRatio, hadModelRatio)
		restoreRatio(common.CompletionRatio, modelName, oldCompletionRatio, hadCompletionRatio)
		restoreRatio(common.CacheRatio, modelName, oldCacheRatio, hadCacheRatio)
		restoreRatio(common.CreateCacheRatio, modelName, oldCreateRatio, hadCreateRatio)
		config.QuotaPerUnit = oldQuotaPerUnit
	})

	common.ModelRatio[modelName] = 1
	common.CompletionRatio[modelName] = 1
	common.CacheRatio[modelName] = 0.1
	common.CreateCacheRatio[modelName] = 1.25
	config.QuotaPerUnit = 500000

	usage := &openai.ResponseUsage{
		InputTokens:  1000,
		OutputTokens: 100,
		InputTokensDetails: &openai.InputTokensDetails{
			CachedTokens:     400,
			CacheWriteTokens: 300,
		},
	}

	quota, cost := CalculateOpenaiResponseQuotaByRatio(usage, modelName, 1)
	// 300 ordinary + 400*0.1 read + 300*1.25 write + 100 output = 815.
	if quota != 815 {
		t.Fatalf("quota = %d, want 815", quota)
	}
	if cost.CacheTokens != 400 || cost.CacheWriteTokens != 300 {
		t.Fatalf("unexpected cache token split: %+v", cost)
	}
}

func TestClaudeCacheReadFiveMinuteAndOneHourBilling(t *testing.T) {
	const modelName = "claude-cache-billing-test-model"
	oldModelRatio, hadModelRatio := common.ModelRatio[modelName]
	oldCompletionRatio, hadCompletionRatio := common.CompletionRatio[modelName]
	oldCacheRatio, hadCacheRatio := common.CacheRatio[modelName]
	oldCreateRatio, hadCreateRatio := common.CreateCacheRatio[modelName]
	oldQuotaPerUnit := config.QuotaPerUnit
	t.Cleanup(func() {
		restoreRatio(common.ModelRatio, modelName, oldModelRatio, hadModelRatio)
		restoreRatio(common.CompletionRatio, modelName, oldCompletionRatio, hadCompletionRatio)
		restoreRatio(common.CacheRatio, modelName, oldCacheRatio, hadCacheRatio)
		restoreRatio(common.CreateCacheRatio, modelName, oldCreateRatio, hadCreateRatio)
		config.QuotaPerUnit = oldQuotaPerUnit
	})

	common.ModelRatio[modelName] = 1
	common.CompletionRatio[modelName] = 1
	common.CacheRatio[modelName] = 0.1
	common.CreateCacheRatio[modelName] = 1.25
	config.QuotaPerUnit = 500000

	usage := &anthropic.Usage{
		InputTokens:          100,
		OutputTokens:         100,
		CacheReadInputTokens: 100,
		CacheCreation: &anthropic.CacheCreation{
			Ephemeral5mInputTokens: 100,
			Ephemeral1hInputTokens: 100,
		},
	}

	quota, _ := CalculateClaudeQuotaByRatio(usage, modelName, 1)
	// 100 input + 100 output + 100*0.1 read + 100*1.25 5m + 100*2 1h = 535.
	if quota != 535 {
		t.Fatalf("quota = %d, want 535", quota)
	}
}

func restoreRatio(values map[string]float64, key string, value float64, existed bool) {
	if existed {
		values[key] = value
	} else {
		delete(values, key)
	}
}
