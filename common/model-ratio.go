package common

import (
	"encoding/json"
	"strings"

	"github.com/songquanpeng/one-api/common/logger"
)

const (
	USD2RMB = 7
	USD     = 500 // $0.002 = 1 -> $1 = 500
	RMB     = USD / USD2RMB
)

// ModelRatio
// https://platform.openai.com/docs/models/model-endpoint-compatibility
// https://cloud.baidu.com/doc/WENXINWORKSHOP/s/Blfmc9dlf
// https://openai.com/pricing
// 1 === $0.002 / 1K tokens
// 1 === ￥0.014 / 1k tokens
var ModelRatio = map[string]float64{
	// https://openai.com/pricing
	"gpt-4":                   15,
	"gpt-4-0314":              15,
	"gpt-4-0613":              15,
	"gpt-4-32k":               30,
	"gpt-4-32k-0314":          30,
	"gpt-4-32k-0613":          30,
	"gpt-4-1106-preview":      5,    // $0.01 / 1K tokens
	"gpt-4-0125-preview":      5,    // $0.01 / 1K tokens
	"gpt-4-turbo-preview":     5,    // $0.01 / 1K tokens
	"gpt-4-vision-preview":    5,    // $0.01 / 1K tokens
	"gpt-4o":                  2.5,  // $0.005 / 1K tokens
	"gpt-4o-2024-05-13":       2.5,  // $0.005 / 1K tokens
	"gpt-3.5-turbo":           0.25, // $0.0005 / 1K tokens
	"gpt-3.5-turbo-0301":      0.75,
	"gpt-3.5-turbo-0613":      0.75,
	"gpt-3.5-turbo-16k":       1.5, // $0.003 / 1K tokens
	"gpt-3.5-turbo-16k-0613":  1.5,
	"gpt-3.5-turbo-instruct":  0.75, // $0.0015 / 1K tokens
	"gpt-3.5-turbo-1106":      0.5,  // $0.001 / 1K tokens
	"gpt-3.5-turbo-0125":      0.25, // $0.0005 / 1K tokens
	"davinci-002":             1,    // $0.002 / 1K tokens
	"babbage-002":             0.2,  // $0.0004 / 1K tokens
	"text-ada-001":            0.2,
	"text-babbage-001":        0.25,
	"text-curie-001":          1,
	"text-davinci-002":        10,
	"text-davinci-003":        10,
	"text-davinci-edit-001":   10,
	"code-davinci-edit-001":   10,
	"whisper-1":               15,    // $0.006 / minute -> $0.006 / 150 words -> $0.006 / 200 tokens -> $0.03 / 1k tokens
	"gpt-4o-mini-transcribe":  0.625, // $1.25 / 1M tokens (文字输入基础价格) -> 0.625 * $0.002 = $1.25/1M
	"gpt-4o-mini-tts":         0.3,   // $0.6 / 1M tokens (文字输入价格) -> 0.3 * $0.002 = $0.6/1M tokens
	"tts-1":                   7.5,   // $0.015 / 1K characters
	"tts-1-1106":              7.5,
	"tts-1-hd":                15, // $0.030 / 1K characters
	"tts-1-hd-1106":           15,
	"davinci":                 10,
	"curie":                   10,
	"babbage":                 10,
	"ada":                     10,
	"text-embedding-ada-002":  0.05,
	"text-embedding-3-small":  0.01,
	"text-embedding-3-large":  0.065,
	"text-search-ada-doc-001": 10,
	"text-moderation-stable":  0.1,
	"text-moderation-latest":  0.1,
	"dall-e-2":                8,  // $0.016 - $0.020 / image
	"dall-e-3":                20, // $0.040 - $0.120 / image
	"gpt-4.1":                 1,
	"gpt-4.1-2025-04-14":      1,
	"gpt-4.1-mini-2025-04-14": 0.2,
	"gpt-4.1-mini":            0.2,
	"gpt-4.1-nano-2025-04-14": 0.05,
	"gpt-4.1-nano":            0.05,
	// https://www.anthropic.com/api#pricing
	"claude-instant-1.2":       0.8 / 1000 * USD,
	"claude-2.0":               8.0 / 1000 * USD,
	"claude-2.1":               8.0 / 1000 * USD,
	"claude-3-haiku-20240307":  0.25 / 1000 * USD,
	"claude-3-sonnet-20240229": 3.0 / 1000 * USD,
	"claude-3-opus-20240229":   15.0 / 1000 * USD,
	// https://cloud.baidu.com/doc/WENXINWORKSHOP/s/hlrk4akp7
	"ERNIE-Bot":       0.8572,     // ￥0.012 / 1k tokens
	"ERNIE-Bot-turbo": 0.5715,     // ￥0.008 / 1k tokens
	"ERNIE-Bot-4":     0.12 * RMB, // ￥0.12 / 1k tokens
	"ERNIE-Bot-8K":    0.024 * RMB,
	"Embedding-V1":    0.1429, // ￥0.002 / 1k tokens
	"bge-large-zh":    0.002 * RMB,
	"bge-large-en":    0.002 * RMB,
	"bge-large-8k":    0.002 * RMB,
	// https://ai.google.dev/pricing
	"gemini-1.5-pro": 1,
	// https://open.bigmodel.cn/pricing
	"glm-4":                     0.1 * RMB,
	"glm-4v":                    0.1 * RMB,
	"glm-3-turbo":               0.005 * RMB,
	"chatglm_turbo":             0.3572, // ￥0.005 / 1k tokens
	"chatglm_pro":               0.7143, // ￥0.01 / 1k tokens
	"chatglm_std":               0.3572, // ￥0.005 / 1k tokens
	"chatglm_lite":              0.1429, // ￥0.002 / 1k tokens
	"qwen-turbo":                0.5715, // ￥0.008 / 1k tokens  // https://help.aliyun.com/zh/dashscope/developer-reference/tongyi-thousand-questions-metering-and-billing
	"qwen-plus":                 1.4286, // ￥0.02 / 1k tokens
	"qwen-max":                  1.4286, // ￥0.02 / 1k tokens
	"qwen-max-longcontext":      1.4286, // ￥0.02 / 1k tokens
	"text-embedding-v1":         0.05,   // ￥0.0007 / 1k tokens
	"SparkDesk":                 1.2858, // ￥0.018 / 1k tokens
	"SparkDesk-v1.1":            1.2858, // ￥0.018 / 1k tokens
	"SparkDesk-v2.1":            1.2858, // ￥0.018 / 1k tokens
	"SparkDesk-v3.1":            1.2858, // ￥0.018 / 1k tokens
	"SparkDesk-v3.5":            1.2858, // ￥0.018 / 1k tokens
	"360GPT_S2_V9":              0.8572, // ¥0.012 / 1k tokens
	"embedding-bert-512-v1":     0.0715, // ¥0.001 / 1k tokens
	"embedding_s1_v1":           0.0715, // ¥0.001 / 1k tokens
	"semantic_similarity_s1_v1": 0.0715, // ¥0.001 / 1k tokens
	"hunyuan":                   7.143,  // ¥0.1 / 1k tokens  // https://cloud.tencent.com/document/product/1729/97731#e0e6be58-60c8-469f-bdeb-6c264ce3b4d0
	"ChatStd":                   0.01 * RMB,
	"ChatPro":                   0.1 * RMB,
	// https://platform.moonshot.cn/pricing
	"moonshot-v1-8k":   0.012 * RMB,
	"moonshot-v1-32k":  0.024 * RMB,
	"moonshot-v1-128k": 0.06 * RMB,
	// https://platform.baichuan-ai.com/price
	"Baichuan2-Turbo":      0.008 * RMB,
	"Baichuan2-Turbo-192k": 0.016 * RMB,
	"Baichuan2-53B":        0.02 * RMB,
	// https://api.minimax.chat/document/price
	"abab6-chat":    0.1 * RMB,
	"abab5.5-chat":  0.015 * RMB,
	"abab5.5s-chat": 0.005 * RMB,
	// https://docs.mistral.ai/platform/pricing/
	"open-mistral-7b":       0.25 / 1000 * USD,
	"open-mixtral-8x7b":     0.7 / 1000 * USD,
	"mistral-small-latest":  2.0 / 1000 * USD,
	"mistral-medium-latest": 2.7 / 1000 * USD,
	"mistral-large-latest":  8.0 / 1000 * USD,
	"mistral-embed":         0.1 / 1000 * USD,
	// https://wow.groq.com/
	"llama2-70b-4096":    0.7 / 1000 * USD,
	"llama2-7b-2048":     0.1 / 1000 * USD,
	"mixtral-8x7b-32768": 0.27 / 1000 * USD,
	"gemma-7b-it":        0.1 / 1000 * USD,
	// https://platform.lingyiwanwu.com/docs#-计费单元
	"yi-34b-chat-0205": 2.5 / 1000 * RMB,
	"yi-34b-chat-200k": 12.0 / 1000 * RMB,
	"yi-vl-plus":       6.0 / 1000 * RMB,
	//https://cohere.com/pricing  command系列没有给出定价 所以采用的openrouter的标准
	"command":               0.5,
	"command-light":         0.5,
	"command-nightly":       0.5,
	"command-light-nightly": 0.5,
	"command-r":             0.25,
	"command-r-plus":        1.5,
	// https://platform.deepseek.com/api-docs/pricing/
	"deepseek-chat":  1.0 / 1000 * RMB,
	"deepseek-coder": 1.0 / 1000 * RMB,
	// 图片模型的token计费 - 基于输入token价格设置基础比率
	"gpt-image-1": 2.5, // 5/1M input tokens转换为配额比率
	"gpt-image-2": 4.0, // 8/1M input tokens转换为配额比率
	// Gemini 专用画图模型
	"gemini-2.5-flash-image-preview": 0.15, // $0.3/1M ÷ $2/1M(基础价格) = 0.15
}

var CompletionRatio = map[string]float64{
	// 图片模型的输出token比率：输出token价格 / 输入token价格
	"gpt-image-1": 8, // 输出token价格是输入token的8倍 (40/5)
	// Gemini 专用画图模型的输出token比率：30/0.3 = 100
	"gemini-2.5-flash-image-preview": 100, // 输出token价格是输入token的100倍
	// 音频转录模型的文字输出token比率：文字输出价格是文字输入的4倍
	"gpt-4o-mini-transcribe": 4, // 文字输出$5/1M, 文字输入$1.25/1M -> 5/1.25 = 4
	// TTS模型的语音输出token比率：语音输出价格相对于文字输入的倍率
	"gpt-4o-mini-tts": 20, // 语音输出$12/1M, 文字输入$0.6/1M -> 12/0.6 = 20
}

// 音频输入token倍率：音频输入token相对于文本输入token的价格倍率
var AudioInputRatio = map[string]float64{
	// 支持音频输入的模型配置，先留空待填充
	// "gpt-4o-audio-preview": 100,  // 示例：音频输入token是文本token的100倍
	"gpt-4o-mini-transcribe": 2.4, // 音频输入$3/1M, 文字输入$1.25/1M -> 3/1.25 = 2.4
	// "gpt-4o-realtime-preview": 100,
}

// 音频输出token倍率：音频输出token相对于文本输出token的价格倍率
var AudioOutputRatio = map[string]float64{
	// 支持音频输出的模型配置，先留空待填充
	// "gpt-4o-audio-preview": 200,  // 示例：音频输出token是文本输出token的200倍
	// "gpt-4o-realtime-preview": 200,
}

// 图片输入token倍率：图片输入token相对于文本输入token的价格倍率
var ImageInputRatio = map[string]float64{
	// 支持图片输入的模型配置
}

// 图片输出token倍率：图片输出token相对于文本输出token的价格倍率
var ImageOutputRatio = map[string]float64{
	// 支持图片输出的模型配置
}

// 缓存token倍率：缓存读取token相对于文本输入token的价格倍率
// Claude 缓存读取价格是输入价格的 0.1 倍 (90% 折扣)
// 缓存写入价格是输入价格的 1.25 倍
var CacheRatio = map[string]float64{
	// Claude 模型的缓存读取倍率
	"claude-3-5-sonnet-20240620": 0.1,
	"claude-3-5-sonnet-20241022": 0.1,
	"claude-3-5-haiku-20241022":  0.1,
	"claude-3-opus-20240229":     0.1,
	"claude-3-sonnet-20240229":   0.1,
	"claude-3-haiku-20240307":    0.1,
	"claude-sonnet-4-20250514":   0.1,
	// Gemini 模型的缓存读取倍率（官方定价：缓存读取为输入价格的 25%）
	"gemini-2.5-pro":             0.25,
	"gemini-2.5-flash":           0.25,
	"gemini-2.0-flash":           0.25,
	"gemini-2.0-flash-lite":      0.25,
	"gemini-1.5-pro":             0.25,
	"gemini-1.5-flash":           0.25,
}

var DefaultModelRatio map[string]float64
var DefaultCompletionRatio map[string]float64
var DefaultAudioInputRatio map[string]float64
var DefaultAudioOutputRatio map[string]float64
var DefaultImageInputRatio map[string]float64
var DefaultImageOutputRatio map[string]float64
var DefaultCacheRatio map[string]float64
var ModelPrice map[string]float64

func init() {
	DefaultModelRatio = make(map[string]float64)
	for k, v := range ModelRatio {
		DefaultModelRatio[k] = v
	}
	DefaultCompletionRatio = make(map[string]float64)
	for k, v := range CompletionRatio {
		DefaultCompletionRatio[k] = v
	}
	DefaultAudioInputRatio = make(map[string]float64)
	for k, v := range AudioInputRatio {
		DefaultAudioInputRatio[k] = v
	}
	DefaultAudioOutputRatio = make(map[string]float64)
	for k, v := range AudioOutputRatio {
		DefaultAudioOutputRatio[k] = v
	}
	DefaultImageInputRatio = make(map[string]float64)
	for k, v := range ImageInputRatio {
		DefaultImageInputRatio[k] = v
	}
	DefaultImageOutputRatio = make(map[string]float64)
	for k, v := range ImageOutputRatio {
		DefaultImageOutputRatio[k] = v
	}
	DefaultCacheRatio = make(map[string]float64)
	for k, v := range CacheRatio {
		DefaultCacheRatio[k] = v
	}
	ModelPrice = make(map[string]float64)
	for k, v := range DefaultModelPrice {
		ModelPrice[k] = v
	}
}

func AddNewMissingRatio(oldRatio string) string {
	newRatio := make(map[string]float64)
	err := json.Unmarshal([]byte(oldRatio), &newRatio)
	if err != nil {
		logger.SysError("error unmarshalling old ratio: " + err.Error())
		return oldRatio
	}
	for k, v := range DefaultModelRatio {
		if _, ok := newRatio[k]; !ok {
			newRatio[k] = v
		}
	}
	jsonBytes, err := json.Marshal(newRatio)
	if err != nil {
		logger.SysError("error marshalling new ratio: " + err.Error())
		return oldRatio
	}
	return string(jsonBytes)
}

func AddNewMissingModelPrice(oldPrice string) string {
	newPrice := make(map[string]float64)
	err := json.Unmarshal([]byte(oldPrice), &newPrice)
	if err != nil {
		logger.SysError("error unmarshalling old model price: " + err.Error())
		return oldPrice
	}
	for k, v := range DefaultModelPrice {
		if _, ok := newPrice[k]; !ok {
			newPrice[k] = v
		}
	}
	jsonBytes, err := json.Marshal(newPrice)
	if err != nil {
		logger.SysError("error marshalling new model price: " + err.Error())
		return oldPrice
	}
	return string(jsonBytes)
}

func AddNewMissingCompletionRatio(oldRatio string) string {
	newRatio := make(map[string]float64)
	err := json.Unmarshal([]byte(oldRatio), &newRatio)
	if err != nil {
		logger.SysError("error unmarshalling old completion ratio: " + err.Error())
		return oldRatio
	}
	for k, v := range DefaultCompletionRatio {
		if _, ok := newRatio[k]; !ok {
			newRatio[k] = v
		}
	}
	jsonBytes, err := json.Marshal(newRatio)
	if err != nil {
		logger.SysError("error marshalling new completion ratio: " + err.Error())
		return oldRatio
	}
	return string(jsonBytes)
}

func AddNewMissingAudioInputRatio(oldRatio string) string {
	newRatio := make(map[string]float64)
	err := json.Unmarshal([]byte(oldRatio), &newRatio)
	if err != nil {
		logger.SysError("error unmarshalling old audio input ratio: " + err.Error())
		return oldRatio
	}
	for k, v := range DefaultAudioInputRatio {
		if _, ok := newRatio[k]; !ok {
			newRatio[k] = v
		}
	}
	jsonBytes, err := json.Marshal(newRatio)
	if err != nil {
		logger.SysError("error marshalling new audio input ratio: " + err.Error())
		return oldRatio
	}
	return string(jsonBytes)
}

func AddNewMissingAudioOutputRatio(oldRatio string) string {
	newRatio := make(map[string]float64)
	err := json.Unmarshal([]byte(oldRatio), &newRatio)
	if err != nil {
		logger.SysError("error unmarshalling old audio output ratio: " + err.Error())
		return oldRatio
	}
	for k, v := range DefaultAudioOutputRatio {
		if _, ok := newRatio[k]; !ok {
			newRatio[k] = v
		}
	}
	jsonBytes, err := json.Marshal(newRatio)
	if err != nil {
		logger.SysError("error marshalling new audio output ratio: " + err.Error())
		return oldRatio
	}
	return string(jsonBytes)
}

func ModelRatio2JSONString() string {
	jsonBytes, err := json.Marshal(ModelRatio)
	if err != nil {
		logger.SysError("error marshalling model ratio: " + err.Error())
	}
	return string(jsonBytes)
}

func UpdateModelRatioByJSONString(jsonStr string) error {
	ModelRatio = make(map[string]float64)
	return json.Unmarshal([]byte(jsonStr), &ModelRatio)
}

func GetModelRatio(name string) float64 {
	if strings.HasPrefix(name, "qwen-") && strings.HasSuffix(name, "-internet") {
		name = strings.TrimSuffix(name, "-internet")
	}
	if strings.HasPrefix(name, "command-") && strings.HasSuffix(name, "-internet") {
		name = strings.TrimSuffix(name, "-internet")
	}
	ratio, ok := ModelRatio[name]
	if !ok {
		ratio, ok = DefaultModelPrice[name]
	}
	if !ok {
		return 30
	}
	return ratio
}

func CompletionRatio2JSONString() string {
	jsonBytes, err := json.Marshal(CompletionRatio)
	if err != nil {
		logger.SysError("error marshalling completion ratio: " + err.Error())
	}
	return string(jsonBytes)
}

func UpdateCompletionRatioByJSONString(jsonStr string) error {
	CompletionRatio = make(map[string]float64)
	return json.Unmarshal([]byte(jsonStr), &CompletionRatio)
}

func AudioInputRatio2JSONString() string {
	jsonBytes, err := json.Marshal(AudioInputRatio)
	if err != nil {
		logger.SysError("error marshalling audio input ratio: " + err.Error())
	}
	return string(jsonBytes)
}

func UpdateAudioInputRatioByJSONString(jsonStr string) error {
	AudioInputRatio = make(map[string]float64)
	return json.Unmarshal([]byte(jsonStr), &AudioInputRatio)
}

func AudioOutputRatio2JSONString() string {
	jsonBytes, err := json.Marshal(AudioOutputRatio)
	if err != nil {
		logger.SysError("error marshalling audio output ratio: " + err.Error())
	}
	return string(jsonBytes)
}

func UpdateAudioOutputRatioByJSONString(jsonStr string) error {
	AudioOutputRatio = make(map[string]float64)
	return json.Unmarshal([]byte(jsonStr), &AudioOutputRatio)
}

func ImageInputRatio2JSONString() string {
	jsonBytes, err := json.Marshal(ImageInputRatio)
	if err != nil {
		logger.SysError("error marshalling image input ratio: " + err.Error())
	}
	return string(jsonBytes)
}

func UpdateImageInputRatioByJSONString(jsonStr string) error {
	ImageInputRatio = make(map[string]float64)
	return json.Unmarshal([]byte(jsonStr), &ImageInputRatio)
}

func ImageOutputRatio2JSONString() string {
	jsonBytes, err := json.Marshal(ImageOutputRatio)
	if err != nil {
		logger.SysError("error marshalling image output ratio: " + err.Error())
	}
	return string(jsonBytes)
}

func UpdateImageOutputRatioByJSONString(jsonStr string) error {
	ImageOutputRatio = make(map[string]float64)
	return json.Unmarshal([]byte(jsonStr), &ImageOutputRatio)
}

func AddNewMissingImageInputRatio(oldRatio string) string {
	newRatio := make(map[string]float64)
	err := json.Unmarshal([]byte(oldRatio), &newRatio)
	if err != nil {
		logger.SysError("error unmarshalling old image input ratio: " + err.Error())
		return oldRatio
	}
	for k, v := range DefaultImageInputRatio {
		if _, ok := newRatio[k]; !ok {
			newRatio[k] = v
		}
	}
	jsonBytes, err := json.Marshal(newRatio)
	if err != nil {
		logger.SysError("error marshalling new image input ratio: " + err.Error())
		return oldRatio
	}
	return string(jsonBytes)
}

func AddNewMissingImageOutputRatio(oldRatio string) string {
	newRatio := make(map[string]float64)
	err := json.Unmarshal([]byte(oldRatio), &newRatio)
	if err != nil {
		logger.SysError("error unmarshalling old image output ratio: " + err.Error())
		return oldRatio
	}
	for k, v := range DefaultImageOutputRatio {
		if _, ok := newRatio[k]; !ok {
			newRatio[k] = v
		}
	}
	jsonBytes, err := json.Marshal(newRatio)
	if err != nil {
		logger.SysError("error marshalling new image output ratio: " + err.Error())
		return oldRatio
	}
	return string(jsonBytes)
}

func CacheRatio2JSONString() string {
	jsonBytes, err := json.Marshal(CacheRatio)
	if err != nil {
		logger.SysError("error marshalling cache ratio: " + err.Error())
	}
	return string(jsonBytes)
}

func UpdateCacheRatioByJSONString(jsonStr string) error {
	CacheRatio = make(map[string]float64)
	return json.Unmarshal([]byte(jsonStr), &CacheRatio)
}

func AddNewMissingCacheRatio(oldRatio string) string {
	newRatio := make(map[string]float64)
	err := json.Unmarshal([]byte(oldRatio), &newRatio)
	if err != nil {
		logger.SysError("error unmarshalling old cache ratio: " + err.Error())
		return oldRatio
	}
	for k, v := range DefaultCacheRatio {
		if _, ok := newRatio[k]; !ok {
			newRatio[k] = v
		}
	}
	jsonBytes, err := json.Marshal(newRatio)
	if err != nil {
		logger.SysError("error marshalling new cache ratio: " + err.Error())
		return oldRatio
	}
	return string(jsonBytes)
}

// GetCacheRatio 获取缓存token倍率
// 如果找到了缓存倍率就直接使用，如果没找到就默认使用文字补全的倍率（CompletionRatio）
func GetCacheRatio(name string) float64 {
	if ratio, ok := CacheRatio[name]; ok {
		return ratio
	}
	if ratio, ok := DefaultCacheRatio[name]; ok {
		return ratio
	}
	// 没有配置缓存倍率的模型，默认使用补全倍率
	return GetCompletionRatio(name)
}

func GetCompletionRatio(name string) float64 {
	if ratio, ok := CompletionRatio[name]; ok {
		return ratio
	}
	if ratio, ok := DefaultCompletionRatio[name]; ok {
		return ratio
	}
	if strings.HasPrefix(name, "gpt-3.5") {
		if name == "gpt-3.5-turbo" || strings.HasSuffix(name, "0125") {
			// https://openai.com/blog/new-embedding-models-and-api-updates
			// Updated GPT-3.5 Turbo model and lower pricing
			return 3
		}
		if strings.HasSuffix(name, "1106") {
			return 2
		}
		return 4.0 / 3.0
	}
	if strings.HasPrefix(name, "gpt-4.1") {
		return 4
	}
	if strings.HasPrefix(name, "gpt-4") {
		if strings.HasPrefix(name, "gpt-4-turbo") ||
			strings.HasPrefix(name, "gpt-4o") ||
			strings.HasSuffix(name, "preview") {
			return 3
		}
		return 2
	}
	if strings.HasPrefix(name, "claude-3") {
		return 5
	}
	if strings.HasPrefix(name, "claude-") {
		return 3
	}
	if strings.HasPrefix(name, "mistral-") {
		return 3
	}
	if strings.HasPrefix(name, "gemini-") {
		return 3
	}
	switch name {
	case "llama2-70b-4096":
		return 0.8 / 0.64
	case "llama3-8b-8192":
		return 2
	case "llama3-70b-8192":
		return 0.79 / 0.59
	case "command", "command-light", "command-nightly", "command-light-nightly":
		return 2
	case "command-r":
		return 3
	case "command-r-plus":
		return 5
	}
	return 1
}

var DefaultModelPrice = map[string]float64{
	"gpt-4-gizmo-*":           0.1,
	"generate_core":           0.03,
	"generate_sd3":            0.065,
	"generate_ultra":          0.08,
	"sd3-turbo":               0.04,
	"upscale_conservative":    0.03,
	"upscale_creative":        0.25,
	"upscale_creative_result": 0.0,
	"edit_erase":              0.03,
	"edit_inpaint":            0.03,
	"edit_outpaint":           0.04,
	"edit_search_replace":     0.04,
	"edit_remove_background":  0.02,
	"control_sketch":          0.03,
	"control_structure":       0.03,
	"image_to_video":          0.2,
	// Flux 模型价格（预估，实际以响应 cost 为准）
	"flux-pro":            0.055,
	"flux-schnell":        0.003,
	"flux-dev":            0.030,
	"flux-2-pro":          0.04,
	"flux-2-flex":         0.025,
	"flux-kontext-pro":    0.06,
	"flux-kontext-max":    0.08,
	"flux-pro-1.1":        0.05,
	"flux-pro-1.1-ultra":  0.06,
	"flux-pro-1.0-fill":   0.04,
	"flux-pro-1.0-expand": 0.045,
	"flux-2-max":          0.09,
	"sd3-large":           0.065,
	"sd3-medium":          0.035,
	"sd3-large-turbo":     0.04,
	"cogvideox":           0.1,
	"video-01":            0.45,
	// Kling 视频模型定价（临时占位值，待官方确认）
	"kling-v1":                   0.14,
	"kling-v1-5":                 0.28,
	"kling-v1-5-std":             50,  // Kling 1.5 标准版
	"kling-v1-5-pro":             100, // Kling 1.5 专业版
	"kling-v1-6-std":             60,  // Kling 1.6 标准版
	"kling-v1-6-pro":             120, // Kling 1.6 专业版
	"gen3a_turbo":                0.25,
	"luma":                       0.4,
	"dall-e-3":                   0.04,
	"dall-e-2":                   0.02,
	"viggle":                     0.2,
	"v3.5":                       0.45,
	"video-01-live2d":            0.45,
	"S2V-01":                     0.6,
	"image-01":                   0.025,
	"grok-2-image-1212":          0.07,
	"grok-2-image":               0.07,
	"doubao-seedream-4-0-250828": 0.03,
	"doubao-seedream-4-5-251128": 0.04,
	"doubao-seedream-5-0-260128": 0.035, // 豆包图片模型按次计费
}

//后续进行修正

var DefaultModelTypeRatio = map[string]float64{
	"turbo": 1,
	"fast":  1,
	"relax": 1,
}

func ModelPrice2JSONString() string {
	jsonBytes, err := json.Marshal(ModelPrice)
	if err != nil {
		logger.SysError("error marshalling model price: " + err.Error())
	}
	return string(jsonBytes)
}

func UpdateModelPriceByJSONString(jsonStr string) error {
	ModelPrice = make(map[string]float64)
	return json.Unmarshal([]byte(jsonStr), &ModelPrice)
}

func GetModelPrice(name string, printErr bool) float64 {
	if strings.HasPrefix(name, "gpt-4-gizmo") {
		name = "gpt-4-gizmo-*"
	}
	price, ok := ModelPrice[name]
	if !ok {
		if printErr {
			logger.SysError("model price not found: " + name)
		}
		return -1
	}
	return price
}

// GetAudioInputRatio 获取音频输入token倍率
// 如果找不到对应模型的音频输入倍率，返回 1.0（与基础输入倍率相同）
func GetAudioInputRatio(name string) float64 {
	if ratio, ok := AudioInputRatio[name]; ok {
		return ratio
	}
	// 找不到时返回 1.0，表示音频输入与文本输入价格相同
	return 1.0
}

// GetAudioOutputRatio 获取音频输出token倍率
// 如果找不到对应模型的音频输出倍率，返回文字输出倍率（CompletionRatio）
func GetAudioOutputRatio(name string) float64 {
	if ratio, ok := AudioOutputRatio[name]; ok {
		return ratio
	}
	// 找不到时返回文字输出倍率
	return GetCompletionRatio(name)
}

// GetImageInputRatio 获取图片输入token倍率
// 如果找不到对应模型的图片输入倍率，返回 1.0（与基础输入倍率相同）
func GetImageInputRatio(name string) float64 {
	if ratio, ok := ImageInputRatio[name]; ok {
		return ratio
	}
	// 找不到时返回 1.0，表示图片输入与文本输入价格相同
	return 1.0
}

// GetImageOutputRatio 获取图片输出token倍率
// 如果找不到对应模型的图片输出倍率，返回文字输出倍率（CompletionRatio）
func GetImageOutputRatio(name string) float64 {
	if ratio, ok := ImageOutputRatio[name]; ok {
		return ratio
	}
	// 找不到时返回文字输出倍率
	return GetCompletionRatio(name)
}
