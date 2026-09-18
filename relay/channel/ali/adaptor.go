package ali

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/relay/channel"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

// 阿里云百炼 Adaptor
// - chat / embedding 走 OpenAI 兼容端点 /compatible-mode/v1/...
// - Anthropic Messages 走 /apps/anthropic/v1/messages（由 RelayClaudeNative 透传）
// - OpenAI Responses 走 /compatible-mode/v1/responses（由 RelayOpenaiResponseNative 透传）
// 官方文档：https://help.aliyun.com/zh/model-studio/anthropic-api-messages

// EnableSearchModelSuffix 保留历史语义：模型名以 -internet 结尾时，启用百炼搜索增强
const EnableSearchModelSuffix = "-internet"

// compatibleChatRequest 是 compatible-mode chat 的请求包装，透出百炼专有的 enable_search 字段
type compatibleChatRequest struct {
	*model.GeneralOpenAIRequest
	EnableSearch bool `json:"enable_search,omitempty"`
}

type Adaptor struct{}

// ConvertImageRequest 当前未实现（图像生成接口未接入）
func (a *Adaptor) ConvertImageRequest(request *model.ImageRequest) (any, error) {
	panic("unimplemented")
}

func (a *Adaptor) Init(meta *util.RelayMeta) {}

func (a *Adaptor) GetRequestURL(meta *util.RelayMeta) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(meta.BaseURL), "/")
	if base == "" {
		base = defaultDashScopeBaseURL
	}
	switch meta.Mode {
	case constant.RelayModeClaude:
		return base + "/apps/anthropic/v1/messages", nil
	case constant.RelayModeOpenaiResponse:
		return base + "/compatible-mode/v1/responses", nil
	case constant.RelayModeEmbeddings:
		return base + "/compatible-mode/v1/embeddings", nil
	default:
		return base + "/compatible-mode/v1/chat/completions", nil
	}
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Request, meta *util.RelayMeta) error {
	channel.SetupCommonRequestHeader(c, req, meta)
	key := meta.ActualAPIKey
	if key == "" {
		key = meta.APIKey
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if meta.IsStream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if meta.Mode == constant.RelayModeClaude {
		anthropicVersion := c.Request.Header.Get("anthropic-version")
		if anthropicVersion == "" {
			anthropicVersion = "2023-06-01"
		}
		req.Header.Set("anthropic-version", anthropicVersion)
		if beta := c.Request.Header.Get("anthropic-beta"); beta != "" {
			req.Header.Set("anthropic-beta", beta)
		}
	}
	return nil
}

func (a *Adaptor) ConvertRequest(c *gin.Context, relayMode int, request *model.GeneralOpenAIRequest) (any, error) {
	if request == nil {
		return nil, errors.New("request is nil")
	}
	// Messages / Responses 由原生控制器走 passthrough，不经过 ConvertRequest
	if relayMode == constant.RelayModeClaude || relayMode == constant.RelayModeOpenaiResponse {
		return request, nil
	}
	// 模型名 -internet 后缀解析：去后缀后启用 enable_search（百炼 compatible-mode 支持该字段）
	enableSearch := false
	if strings.HasSuffix(request.Model, EnableSearchModelSuffix) {
		request.Model = strings.TrimSuffix(request.Model, EnableSearchModelSuffix)
		enableSearch = true
	}
	// 后台渠道测试可能没有 HTTP 请求体，直接使用构造好的请求。
	if c == nil || c.Request == nil || c.Request.Body == nil || c.Request.Body == http.NoBody {
		if enableSearch {
			return compatibleChatRequest{GeneralOpenAIRequest: request, EnableSearch: true}, nil
		}
		return request, nil
	}
	// 保留百炼扩展参数、工具定义及显式 false/0，仅修改模型映射和联网后缀。
	// max_tokens 和 max_completion_tokens 的语义依模型而异，不做通用替换。
	body, err := common.GetRequestBody(c)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, errors.New("request must be a JSON object")
	}
	raw["model"], err = json.Marshal(request.Model)
	if err != nil {
		return nil, err
	}
	if enableSearch {
		raw["enable_search"] = json.RawMessage("true")
	}
	return raw, nil
}

func (a *Adaptor) DoRequest(c *gin.Context, meta *util.RelayMeta, requestBody io.Reader) (*http.Response, error) {
	return channel.DoRequestHelper(a, c, meta, requestBody)
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, meta *util.RelayMeta) (usage *model.Usage, err *model.ErrorWithStatusCode) {
	// Messages / Responses 路径不经过此函数（原生控制器直接处理 resp）
	if meta.IsStream {
		var responseText string
		err, responseText, usage = openai.StreamHandler(c, resp, meta.Mode)
		if usage == nil || usage.TotalTokens == 0 {
			usage = openai.ResponseText2Usage(responseText, meta.ActualModelName, meta.PromptTokens)
		}
		return
	}
	err, usage = openai.Handler(c, resp, meta.PromptTokens, meta.ActualModelName)
	return
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return "ali"
}

func (a *Adaptor) GetModelDetails() []model.APIModel {
	return ModelDetails
}
