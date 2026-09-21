package model

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"unicode"
)

type rankingUsageKey struct{}
type rankingUsage struct {
	Model    string
	Provider string
	Tokens   int64
}

// 部署名只通过明确的映射公开，不猜测 AWS ARN、Azure 部署名对应的模型。
var rankingAliases = func() map[string]string {
	aliases := map[string]string{}
	_ = json.Unmarshal([]byte(os.Getenv("RANKING_MODEL_ALIASES")), &aliases)
	return aliases
}()

// RankingModelIdentity 保留版本后缀，仅移除已知作者命名空间。
func RankingModelIdentity(name string) (string, string) {
	name = strings.TrimSpace(name)
	if alias, ok := rankingAliases[name]; ok {
		name = alias
	}
	name = strings.ToLower(name)
	if len(name) > 180 || strings.ContainsFunc(name, unicode.IsSpace) {
		return "", ""
	}
	if parts := strings.Split(name, "/"); len(parts) == 2 {
		switch parts[0] {
		case "openai", "anthropic", "google", "deepseek", "qwen", "z-ai", "moonshotai", "mistralai", "meta-llama", "x-ai", "minimax", "xiaomi":
			name = parts[1]
		default:
			return "", ""
		}
	}
	for _, excluded := range []string{"image", "embedding", "tts", "audio", "realtime", "vision-preview-image"} {
		if strings.Contains(name, excluded) {
			return "", ""
		}
	}
	for _, author := range []struct{ prefixes, name string }{
		{"gpt-,chatgpt-,o1,o3,o4", "OpenAI"}, {"claude-", "Anthropic"},
		{"gemini-,gemma-", "Google"}, {"deepseek-", "DeepSeek"},
		{"qwen", "Alibaba"}, {"glm-,chatglm", "Zhipu"}, {"kimi-,moonshot-", "Moonshot"},
		{"grok-", "xAI"}, {"mistral-,mixtral-,ministral-,devstral-,codestral-", "Mistral"},
		{"llama-", "Meta"}, {"minimax-,abab", "Minimax"}, {"mimo-", "MiMo"},
		{"doubao-", "Doubao"}, {"hunyuan-", "Tencent"}, {"ernie-", "Baidu"},
	} {
		for _, prefix := range strings.Split(author.prefixes, ",") {
			if strings.HasPrefix(name, prefix) {
				return name, author.name
			}
		}
	}
	return "", ""
}

// WithRankingUsage 在最终文本结算处调用，值随 context 传入异步日志，避免再次查渠道。
func WithRankingUsage(ctx context.Context, name, provider string, tokens int64) context.Context {
	name, _ = RankingModelIdentity(name)
	if tokens < 0 {
		return ctx
	}
	return context.WithValue(ctx, rankingUsageKey{}, rankingUsage{name, strings.ToLower(strings.TrimSpace(provider)), tokens})
}

func applyRankingUsage(ctx context.Context, log *Log) {
	if usage, ok := ctx.Value(rankingUsageKey{}).(rankingUsage); ok {
		log.RankingModelName = usage.Model
		log.RankingTokens = &usage.Tokens
		log.Provider = usage.Provider
	}
}
