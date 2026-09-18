package ali

import "github.com/songquanpeng/one-api/relay/model"

var ModelList = []string{
	// 旗舰
	"qwen3.8-max", "qwen3.8-max-0902",
	"qwen3.7-max",
	"qwen3-max", "qwen3-max-preview",
	"qwen-max", "qwen-max-latest", "qwen-max-longcontext",
	// 通用
	"qwen3.7-plus", "qwen3.6-plus", "qwen3.5-plus",
	"qwen3.8-flash", "qwen3.7-flash", "qwen3.6-flash", "qwen3.5-flash",
	"qwen-plus", "qwen-plus-latest",
	"qwen-flash",
	"qwen-turbo", "qwen-turbo-latest",
	// 推理 / 代码
	"qwq-plus", "qwq-32b",
	"qwen3-coder-next", "qwen3-coder-plus", "qwen3-coder-flash",
	// 视觉
	"qwen3-vl-plus", "qwen3-vl-flash",
	"qwen-vl-plus", "qwen-vl-max",
	// 全模态理解；3.8 Omni Flash 输出文本，语音输出需选择支持的型号
	"qwen3.8-omni-flash", "qwen3.5-omni-plus", "qwen3.5-omni-flash",
	// 嵌入
	"qwen3.7-text-embedding", "qwen3.7-text-embedding-flash", "text-embedding-v4",
	"text-embedding-v1", "text-embedding-v2", "text-embedding-v3",
}

var ModelDetails = []model.APIModel{}
