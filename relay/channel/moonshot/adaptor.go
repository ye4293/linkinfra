package moonshot

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/relay/channel"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/util"
)

// Adaptor 嵌入 openai.Adaptor，复用 OpenAI 协议链路（ConvertRequest/DoResponse 等），
// 仅 override GetRequestURL/SetupRequestHeader 增加 Claude 原生协议（/v1/messages）分支。
type Adaptor struct {
	openai.Adaptor
}

// GetRequestURL：Claude 原生请求走 moonshot anthropic 兼容端点
// （渠道 base 填 https://api.moonshot.cn，拼 /anthropic/v1/messages；
// OpenAI 端点同域 api.moonshot.cn/v1/chat/completions，一 base 兼顾）；
// 否则复用 openai adaptor 的 URL 逻辑。
func (a *Adaptor) GetRequestURL(meta *util.RelayMeta) (string, error) {
	if meta.Mode == constant.RelayModeClaude {
		return fmt.Sprintf("%s/anthropic/v1/messages", strings.TrimRight(meta.BaseURL, "/")), nil
	}
	return a.Adaptor.GetRequestURL(meta)
}

// SetupRequestHeader：Claude 分支用 Bearer 渠道 key + anthropic-version/beta 透传
// （moonshot anthropic 端点需 version 头识别 Claude 格式请求体）；否则复用 openai。
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
	return "moonshot"
}
