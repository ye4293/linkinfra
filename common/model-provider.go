package common

import (
	"strconv"
	"strings"
)

// ChannelTypeToProvider 渠道类型 → 供应商名称映射
var ChannelTypeToProvider = map[int]string{
	ChannelTypeOpenAI:         "OpenAI",
	ChannelTypeAPI2D:          "API2D",
	ChannelTypeAzure:          "Azure",
	ChannelTypeCloseAI:        "CloseAI",
	ChannelTypeOpenAISB:       "OpenAI-SB",
	ChannelTypeOpenAIMax:      "OpenAIMax",
	ChannelTypeOhMyGPT:        "OhMyGPT",
	ChannelTypeCustom:         "Custom",
	ChannelTypeAILS:           "AILS",
	ChannelTypeAIProxy:        "AIProxy",
	ChannelTypeAPI2GPT:        "API2GPT",
	ChannelTypeAIGC2D:         "AIGC2D",
	ChannelTypeAIProxyLibrary: "AIProxyLibrary",
	ChannelTypeFastGPT:        "FastGPT",
	ChannelTypePaLM:           "PaLM",
	ChannelTypeGemini:         "Google",
	ChannelTypeVertexAI:       "Vertex AI",
	ChannelTypeAnthropic:      "Anthropic",
	ChannelTypeAwsClaude:      "AWS",
	ChannelTypeOpenRouter:     "OpenRouter",
	ChannelTypeGroq:           "Groq",
	ChannelTypeOllama:         "Ollama",
	ChannelTypeTogetherAi:     "TogetherAI",
	ChannelTypeNovita:         "Novita",
	ChannelTypeBaidu:          "Baidu",
	ChannelTypeZhipu:          "Zhipu",
	ChannelTypeAli:            "Alibaba",
	ChannelTypeXunfei:         "Xunfei",
	ChannelType360:            "360",
	ChannelTypeTencent:        "Tencent",
	ChannelTypeMoonshot:       "Moonshot",
	ChannelTypeBaichuan:       "Baichuan",
	ChannelTypeMinimax:        "Minimax",
	ChannelTypeMimo:           "MiMo",
	ChannelTypeMistral:        "Mistral",
	ChannelTypeLingYiWanWu:    "01.AI",
	ChannelTypeCoze:           "Coze",
	ChannelTypeCohere:         "Cohere",
	ChannelTypeDeepseek:       "DeepSeek",
	channelTypeStability:      "StabilityAI",
	ChannelTypeKeling:         "Kling",
	ChannelTypeRunway:         "Runway",
	ChannelTypeRecraft:        "Recraft",
	ChannelTypeLuma:           "Luma",
	ChannelTypePixverse:       "Pixverse",
	ChannelTypeFlux:           "Flux",
	ChannelTypeXAI:            "xAI",
	ChannelTypeDummy:          "Dummy",
	// ChannelTypeReplicate 的 iota 值 = 40，实际被用作豆包渠道
	ChannelTypeReplicate: "Doubao",
}

// aggregatorChannelTypes 聚合平台渠道类型
// 这些渠道可以托管多个供应商的模型，需要用模型名推断真实供应商
var aggregatorChannelTypes = map[int]bool{
	ChannelTypeOpenRouter: true,
	ChannelTypeNovita:     true,
	ChannelTypeTogetherAi: true,
	ChannelTypeGroq:       true,
	ChannelTypeOllama:     true,
	ChannelTypeCoze:       true,
}

// modelNamePrefixes 模型名前缀 → 供应商映射（用于聚合渠道的模型识别）
var modelNamePrefixes = []struct {
	Prefix   string
	Provider string
}{
	{"gpt-", "OpenAI"}, {"o1-", "OpenAI"}, {"o3-", "OpenAI"}, {"o4-", "OpenAI"},
	{"dall-e", "OpenAI"}, {"tts-", "OpenAI"}, {"whisper-", "OpenAI"},
	{"text-embedding", "OpenAI"}, {"gpt-image", "OpenAI"},
	{"claude-", "Anthropic"},
	{"gemini-", "Google"}, {"palm-", "Google"},
	{"deepseek-", "DeepSeek"},
	{"grok-", "xAI"},
	{"qwen-", "Alibaba"}, {"qwen2", "Alibaba"}, {"qwen3", "Alibaba"},
	{"glm-", "Zhipu"}, {"chatglm", "Zhipu"},
	{"ernie", "Baidu"},
	{"moonshot-", "Moonshot"}, {"kimi-", "Moonshot"},
	{"mistral-", "Mistral"}, {"mixtral-", "Mistral"},
	{"llama-", "Meta"}, {"llama3", "Meta"},
	{"abab", "Minimax"},
	{"mimo-", "MiMo"},
	{"yi-", "01.AI"},
	{"doubao", "Doubao"},
	{"flux-", "Flux"}, {"flux.", "Flux"},
	{"kling-", "Kling"},
	{"luma-", "Luma"},
	{"suno-", "Suno"},
	{"stable-diffusion", "StabilityAI"}, {"sd3", "StabilityAI"}, {"sdxl", "StabilityAI"},
	{"hunyuan", "Tencent"},
	{"command-", "Cohere"},
	{"jamba", "AI21"},
}

// inferProviderFromModelName 根据模型名前缀推断供应商
func inferProviderFromModelName(modelName string) string {
	lower := strings.ToLower(modelName)
	for _, p := range modelNamePrefixes {
		if strings.HasPrefix(lower, strings.ToLower(p.Prefix)) {
			return p.Provider
		}
	}
	return ""
}

// GetProviderByChannelType 根据渠道类型获取供应商名称
func GetProviderByChannelType(channelType int) string {
	if provider, ok := ChannelTypeToProvider[channelType]; ok {
		return provider
	}
	return "Other"
}

// GetCatalogProvider 目录来源优先使用配置值，未配置时按渠道类型回退。
// 已知来源保留标准显示名称，自定义来源归一化，避免大小写或空格造成重复。
func GetCatalogProvider(channelType int, configured string) string {
	provider := strings.ToLower(strings.TrimSpace(configured))
	if provider != "" {
		for _, name := range ChannelTypeToProvider {
			if strings.EqualFold(name, provider) {
				return name
			}
		}
		return provider
	}
	if name, ok := ChannelTypeToProvider[channelType]; ok {
		return name
	}
	// 未知类型也必须分别展示，不能全部合并进 Other。
	return "channel-type-" + strconv.Itoa(channelType)
}

// GetModelProvider 综合判断模型供应商：聚合渠道用模型名推断，其他用渠道类型
func GetModelProvider(modelName string, channelType int) string {
	// 聚合渠道：优先用模型名推断
	if aggregatorChannelTypes[channelType] {
		if provider := inferProviderFromModelName(modelName); provider != "" {
			return provider
		}
		// 推断失败，兜底用渠道类型
		return GetProviderByChannelType(channelType)
	}
	// 非聚合渠道：直接用渠道类型
	return GetProviderByChannelType(channelType)
}
