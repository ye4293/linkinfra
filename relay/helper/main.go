package helper

import (
	"strings"

	"github.com/songquanpeng/one-api/relay/channel"
	"github.com/songquanpeng/one-api/relay/channel/ali"
	"github.com/songquanpeng/one-api/relay/channel/anthropic"
	"github.com/songquanpeng/one-api/relay/channel/doubao"
	"github.com/songquanpeng/one-api/relay/channel/aws"
	"github.com/songquanpeng/one-api/relay/channel/keling"
	"github.com/songquanpeng/one-api/relay/channel/baidu"
	"github.com/songquanpeng/one-api/relay/channel/cohere"
	"github.com/songquanpeng/one-api/relay/channel/flux"
	"github.com/songquanpeng/one-api/relay/channel/gemini"
	"github.com/songquanpeng/one-api/relay/channel/luma"
	"github.com/songquanpeng/one-api/relay/channel/minimax"
	"github.com/songquanpeng/one-api/relay/channel/moonshot"
	"github.com/songquanpeng/one-api/relay/channel/pixverse"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/channel/runway"
	"github.com/songquanpeng/one-api/relay/channel/tencent"
	"github.com/songquanpeng/one-api/relay/channel/vertexai"
	"github.com/songquanpeng/one-api/relay/channel/xai"
	"github.com/songquanpeng/one-api/relay/channel/xunfei"
	"github.com/songquanpeng/one-api/relay/channel/zhipu"
	"github.com/songquanpeng/one-api/relay/constant"
)

// GetVideoAdaptor 根据模型名称返回对应的视频适配器
func GetVideoAdaptor(modelName string) channel.VideoAdaptor {
	lower := strings.ToLower(modelName)
	switch {
	// Minimax
	case strings.HasPrefix(modelName, "video-01"),
		strings.HasPrefix(modelName, "S2V-01"),
		strings.HasPrefix(modelName, "T2V-01"),
		strings.HasPrefix(modelName, "I2V-01"),
		strings.HasPrefix(lower, "minimax"):
		return &minimax.VideoAdaptor{}
	// Zhipu
	case modelName == "cogvideox":
		return &zhipu.VideoAdaptor{}
	// Runway
	case modelName == "gen3a_turbo":
		return &runway.VideoAdaptor{}
	case strings.HasPrefix(strings.ToLower(modelName), "luma"):
		return &luma.VideoAdaptor{}
	case strings.HasPrefix(strings.ToLower(modelName), "wan"):
		return &ali.VideoAdaptor{}
	case modelName == "v3.5":
		return &pixverse.VideoAdaptor{}
	case strings.HasPrefix(modelName, "grok-imagine-video"):
		return &xai.VideoAdaptor{}
	case strings.HasPrefix(modelName, "doubao"):
		return &doubao.VideoAdaptor{}
	case strings.HasPrefix(modelName, "kling") &&
		modelName != "kling-identify-face" &&
		modelName != "kling-advanced-lip-sync":
		return &keling.VideoAdaptor{}
	case strings.HasPrefix(modelName, "veo"):
		return &vertexai.VideoAdaptor{}
	case strings.HasPrefix(strings.ToLower(modelName), "sora") ||
		strings.Contains(strings.ToLower(modelName), "remix"):
		return &openai.VideoAdaptor{}
	}
	return nil
}

// GetVideoAdaptorByProvider 根据供应商名称返回对应的视频适配器（用于结果查询）
func GetVideoAdaptorByProvider(provider string) channel.VideoAdaptor {
	switch provider {
	case "minimax":
		return &minimax.VideoAdaptor{}
	case "zhipu":
		return &zhipu.VideoAdaptor{}
	case "runway":
		return &runway.VideoAdaptor{}
	case "luma":
		return &luma.VideoAdaptor{}
	case "ali":
		return &ali.VideoAdaptor{}
	case "pixverse":
		return &pixverse.VideoAdaptor{}
	case "grok":
		return &xai.VideoAdaptor{}
	case "doubao":
		return &doubao.VideoAdaptor{}
	case "kling":
		return &keling.VideoAdaptor{}
	case "vertexai":
		return &vertexai.VideoAdaptor{}
	case "sora":
		return &openai.VideoAdaptor{}
	}
	return nil
}

func GetAdaptor(apiType int) channel.Adaptor {
	switch apiType {
	case constant.APITypeAli:
		return &ali.Adaptor{}
	case constant.APITypeAnthropic:
		return &anthropic.Adaptor{}
	case constant.APITypeBaidu:
		return &baidu.Adaptor{}
	case constant.APITypeGemini:
		return &gemini.Adaptor{}
	case constant.APITypeOpenAI:
		return &openai.Adaptor{}
	case constant.APITypeTencent:
		return &tencent.Adaptor{}
	case constant.APITypeXunfei:
		return &xunfei.Adaptor{}
	case constant.APITypeZhipu:
		return &zhipu.Adaptor{}
	case constant.APITypeAwsClaude:
		return &aws.Adaptor{}
	case constant.APITypeCohere:
		return &cohere.Adaptor{}
	case constant.APITypeMinimax:
		return &minimax.Adaptor{}
	case constant.APITypeMoonshot:
		return &moonshot.Adaptor{}
	case constant.APITypeXAI:
		return &xai.Adaptor{}
	case constant.APITypeVertexAI:
		return &vertexai.Adaptor{}
	case constant.APITypeFlux:
		return &flux.Adaptor{}
	}
	return nil
}
