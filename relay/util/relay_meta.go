package util

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/constant"
)

type RelayMeta struct {
	Mode         int
	ChannelType  int
	ChannelId    int
	TokenId      int
	TokenName    string
	UserId       int
	Group        string
	ModelMapping map[string]string
	// BaseURL is the proxy url set in the channel config
	BaseURL  string
	APIKey   string
	APIType  int
	Config   model.ChannelConfig
	IsStream bool
	// OriginModelName is the model name from the raw user request
	OriginModelName string
	// ActualModelName is the model name after mapping
	ActualModelName string
	RequestURLPath  string
	PromptTokens    int // only for DoResponse
	// 用于计算首字延迟
	FirstWordLatency  float64
	StartTime         time.Time // 请求开始时间
	FirstResponseTime time.Time // 首字响应时间
	isFirstResponse   bool      // 标记是否是第一个响应
	// 多Key相关信息
	ActualAPIKey string
	KeyIndex     *int // 使用指针以支持nil值
	IsMultiKey   bool
	Keys         []string // 存储所有解析的密钥
	DisablePing  bool
	// 流式响应是否包含 usage 信息
	ShouldIncludeUsage bool
	// 自定义请求头覆盖
	HeadersOverride map[string]string
	// 渠道创建时间（用于 Azure 部署名是否移除小数点）
	ChannelCreateTime int64
	// 渠道折扣倍率（来自 Channel.Discount），默认 1.0
	ChannelDiscount float64
	// 当前用户对当前渠道类型的额外折扣倍率，默认 1.0
	UserChannelRatio float64
}

// SetFirstResponseTime 设置首字响应时间（只设置一次）
func (m *RelayMeta) SetFirstResponseTime() {
	if m.isFirstResponse {
		m.FirstResponseTime = time.Now()
		m.isFirstResponse = false
		// 计算首字延迟（秒）
		if !m.StartTime.IsZero() {
			m.FirstWordLatency = m.FirstResponseTime.Sub(m.StartTime).Seconds()
		}
	}
}

// CombinedGroupRatio 返回计费用的组合折扣 = 等级折扣 × 渠道折扣 × 用户渠道折扣 × 模型折扣。
// 所有通过 meta 计费的 controller 都直接用它，避免 14 处重复表达式。
func (m *RelayMeta) CombinedGroupRatio(modelNames ...string) float64 {
	modelName := m.BillingModelName()
	if len(modelNames) > 0 {
		modelName = modelNames[0]
	}
	channelDiscount := m.ChannelDiscount
	if channelDiscount <= 0 {
		channelDiscount = 1.0
	}
	userChannelRatio := m.UserChannelRatio
	if userChannelRatio <= 0 {
		userChannelRatio = 1.0
	}
	return common.GetGroupRatio(m.Group) * channelDiscount * userChannelRatio * common.GetModelDiscount(modelName)
}

// GetFirstWordLatency 获取首字延迟（秒）
func (m *RelayMeta) GetFirstWordLatency() float64 {
	return m.FirstWordLatency
}

// BillingModelName 返回计费记录使用的模型名。
// 有模型重定向时返回实际调用的模型名，无重定向时与 OriginModelName 相同。
func (m *RelayMeta) BillingModelName() string {
	if m.ActualModelName != "" {
		return m.ActualModelName
	}
	return m.OriginModelName
}

func GetRelayMeta(c *gin.Context) *RelayMeta {
	// channelId := c.GetInt("channel_id")
	// channel, _ := model.GetChannelById(channelId, false)
	meta := RelayMeta{
		Mode:         constant.Path2RelayMode(c.Request.URL.Path),
		ChannelType:  c.GetInt("channel"),
		ChannelId:    c.GetInt("channel_id"),
		TokenId:      c.GetInt("token_id"),
		TokenName:    c.GetString("token_name"),
		UserId:       c.GetInt("id"),
		Group:        c.GetString("group"),
		ModelMapping: c.GetStringMapString("model_mapping"),
		BaseURL:      c.GetString("base_url"),
		// APIVersion:     c.GetString(common.ConfigKeyAPIVersion),
		// APIKey:          channel.Key,
		APIKey:          strings.TrimPrefix(c.Request.Header.Get("Authorization"), "Bearer "),
		RequestURLPath:  c.Request.URL.String(),
		OriginModelName: c.GetString("original_model"),
		ActualAPIKey:    c.GetString("actual_key"),
		IsMultiKey:      c.GetBool("is_multi_key"),
		StartTime:       time.Now(), // 记录请求开始时间
		isFirstResponse: true,       // 初始化为 true，等待第一个响应
	}

	if v, ok := c.Get("channel_create_time"); ok {
		switch t := v.(type) {
		case int64:
			meta.ChannelCreateTime = t
		case int:
			meta.ChannelCreateTime = int64(t)
		}
	}
	// 处理多密钥索引
	if keyIndexValue, exists := c.Get("key_index"); exists {
		if keyIndex, ok := keyIndexValue.(int); ok {
			meta.KeyIndex = &keyIndex
		}
	}

	// 处理多密钥列表（如果是多密钥渠道）
	if meta.IsMultiKey {
		if channelId := c.GetInt("channel_id"); channelId > 0 {
			if channel, err := model.GetChannelById(channelId, true); err == nil {
				meta.Keys = channel.ParseKeys()
			}
		}
	}
	cfg, ok := c.Get("Config")
	if ok {
		meta.Config = cfg.(model.ChannelConfig)
	}
	// 获取自定义请求头覆盖配置
	if headersOverride, exists := c.Get("headers_override"); exists {
		if headers, ok := headersOverride.(map[string]string); ok {
			meta.HeadersOverride = headers
		}
	}
	if meta.BaseURL == "" {
		meta.BaseURL = common.ChannelBaseURLs[meta.ChannelType]
	}
	if meta.Mode == constant.RelayModeGeminiGenerateContent || meta.Mode == constant.RelayModeGeminiStreamGenerateContent {
		if meta.Mode == constant.RelayModeGeminiStreamGenerateContent {
			meta.IsStream = true
		}
	}
	// 应用模型映射：如果有映射规则，使用映射后的模型名称
	meta.ActualModelName = meta.OriginModelName
	if meta.ModelMapping != nil && len(meta.ModelMapping) > 0 {
		if mappedModel, ok := meta.ModelMapping[meta.OriginModelName]; ok && mappedModel != "" {
			meta.ActualModelName = mappedModel
		}
	}
	meta.APIType = constant.ChannelType2APIType(meta.ChannelType)

	// 计费所需：渠道折扣 + 用户针对当前渠道类型的额外折扣
	// 这两项由 middleware/distributor 在选中渠道时写入 c，缺省 1.0。
	meta.ChannelDiscount = 1.0
	if v := c.GetFloat64("channel_discount"); v > 0 {
		meta.ChannelDiscount = v
	}
	meta.UserChannelRatio = 1.0
	if v := c.GetFloat64("user_channel_ratio"); v > 0 {
		meta.UserChannelRatio = v
	}
	return &meta
}

// IsVertexAIAPIKeyMode 检测 VertexAI 是否使用 API Key 模式
// 支持多种判断方式：1. 配置中明确指定 api_key 2. 密钥不是 JSON 格式（即普通 API Key）
func (m *RelayMeta) IsVertexAIAPIKeyMode() bool {
	// 配置中明确指定为 API Key 模式
	if m.Config.VertexKeyType == model.VertexKeyTypeAPIKey {
		return true
	}

	// 如果配置未明确指定，尝试通过密钥格式自动检测
	if m.Config.VertexKeyType == "" {
		// 检查密钥是否是 JSON 格式（服务账号凭证）
		testKey := m.ActualAPIKey
		if m.IsMultiKey && len(m.Keys) > 0 {
			testKey = m.Keys[0]
		}
		// 如果密钥不是以 { 开头，则认为是 API Key 模式
		if testKey != "" && !strings.HasPrefix(strings.TrimSpace(testKey), "{") {
			return true
		}
	}

	return false
}
