package baidu

import "github.com/songquanpeng/one-api/relay/model"

// 千帆 V2 模型 ID；完整可用列表通过 /v2/models 获取。
var ModelList = []string{
	"ernie-5.1",
	"ernie-5.0",
	"ernie-4.5-turbo-32k",
	"ernie-4.5-turbo-128k",
	"ernie-4.5-turbo-vl",
	"ernie-x1.1",
	"deepseek-v4-flash",
	"deepseek-v4-pro",
	"deepseek-v3.2",
	"qwen3.5-397b-a17b",
	"glm-5.3",
	"kimi-k2.6",
	"embedding",
	"bge-large-zh",
	"bge-large-en",
}

var ModelDetails = []model.APIModel{
	{
		Provider:    "Baidu",
		Name:        "ernie-5.1",
		Tags:        []string{"qianfanv2", "chat"},
		PriceType:   "pay-per-token",
		Description: "百度千帆 ERNIE 5.1，支持 Chat、Responses 和 Anthropic 兼容接口。",
	},
}
