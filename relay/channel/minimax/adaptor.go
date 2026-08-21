package minimax

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
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

// GetRequestURL：Claude 原生请求走 minimax anthropic 兼容端点
// （渠道 base 填 https://api.minimax.io，拼 /anthropic/v1/messages）；
// 否则复用 openai（按 RequestURLPath 拼 /v1/chat/completions 或 /v1/responses）。
func (a *Adaptor) GetRequestURL(meta *util.RelayMeta) (string, error) {
	if meta.Mode == constant.RelayModeClaude {
		return fmt.Sprintf("%s/anthropic/v1/messages", strings.TrimRight(meta.BaseURL, "/")), nil
	}
	return a.Adaptor.GetRequestURL(meta)
}

// SetupRequestHeader：Claude 分支 Bearer 渠道 key + anthropic-version/beta 透传
// （minimax anthropic 端点需 version 头识别 Claude 格式请求体）；否则复用 openai。
func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Request, meta *util.RelayMeta) error {
	if meta.Mode == constant.RelayModeClaude {
		channel.SetupCommonRequestHeader(c, req, meta)
		req.Header.Set("Authorization", "Bearer "+meta.ActualAPIKey)
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
