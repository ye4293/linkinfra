package minimax

import (
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

// Adaptor 嵌入 openai.Adaptor，复用 OpenAI 兼容链路：
//   - chat: /v1/chat/completions（废弃旧 /v1/text/chatcompletion_v2）
//   - responses: /v1/responses
//
// override GetRequestURL/SetupRequestHeader 增加 Claude 原生协议（/v1/messages）分支。
// Claude / Responses 走原生 passthrough（不经 ConvertRequest，响应由 controller 层处理）。
type Adaptor struct {
	openai.Adaptor
}

// GetRequestURL 兼容 SDK 基址，Claude 请求使用 Anthropic 前缀。
func (a *Adaptor) GetRequestURL(meta *util.RelayMeta) (string, error) {
	base := strings.TrimRight(meta.BaseURL, "/")
	if base == "" {
		base = common.ChannelBaseURLs[common.ChannelTypeMinimax]
	}
	base = strings.TrimSuffix(base, "/v1")
	base = strings.TrimSuffix(base, "/anthropic")
	normalized := *meta
	normalized.BaseURL = base
	if meta.Mode == constant.RelayModeClaude {
		path := meta.RequestURLPath
		if path == "" {
			path = "/v1/messages"
		}
		return util.GetFullRequestURL(base+"/anthropic", path, meta.ChannelType), nil
	}
	return a.Adaptor.GetRequestURL(&normalized)
}

// 显式使用外层接收者，否则内嵌 OpenAI.DoRequest 会绕过 URL 和请求头覆盖。
func (a *Adaptor) DoRequest(c *gin.Context, meta *util.RelayMeta, body io.Reader) (*http.Response, error) {
	return channel.DoRequestHelper(a, c, meta, body)
}

// SetupRequestHeader：Claude 分支 Bearer 渠道 key + anthropic-version/beta 透传
// （minimax anthropic 端点需 version 头识别 Claude 格式请求体）；否则复用 openai。
func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Request, meta *util.RelayMeta) error {
	if meta.Mode == constant.RelayModeClaude {
		channel.SetupCommonRequestHeader(c, req, meta)
		key := meta.ActualAPIKey
		if key == "" {
			key = meta.APIKey
		}
		req.Header.Set("Authorization", "Bearer "+key)
		anthropicVersion := c.Request.Header.Get("anthropic-version")
		if anthropicVersion == "" {
			anthropicVersion = "2023-06-01"
		}
		req.Header.Set("anthropic-version", anthropicVersion)
		if beta := c.Request.Header.Get("anthropic-beta"); beta != "" {
			req.Header.Set("anthropic-beta", beta)
		}
		return nil
	}
	return a.Adaptor.SetupRequestHeader(c, req, meta)
}

func (a *Adaptor) GetChannelName() string {
	return "minimax"
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetModelDetails() []model.APIModel {
	return ModelDetails
}
