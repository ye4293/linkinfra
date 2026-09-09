package openai

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/util"
)

// Keep DeepSeek on the OpenAI adaptor so chat requests retain the existing
// passthrough behavior. All three protocols share one channel and API key.
func deepseekRequestURL(meta *util.RelayMeta) string {
	base := strings.TrimRight(meta.BaseURL, "/")
	if base == "" {
		base = common.ChannelBaseURLs[common.ChannelTypeDeepseek]
	}
	// Accept both the service root and either protocol's base URL.
	base = strings.TrimSuffix(base, "/v1")
	base = strings.TrimSuffix(base, "/anthropic")
	path := meta.RequestURLPath
	if path == "" {
		switch meta.Mode {
		case constant.RelayModeClaude:
			path = "/v1/messages"
		case constant.RelayModeOpenaiResponse:
			path = "/v1/responses"
		default:
			path = "/v1/chat/completions"
		}
	}
	if meta.Mode == constant.RelayModeClaude {
		base += "/anthropic"
	}
	return util.GetFullRequestURL(base, path, meta.ChannelType)
}

func setupDeepseekRequestHeader(c *gin.Context, req *http.Request, meta *util.RelayMeta) {
	key := meta.ActualAPIKey
	if key == "" {
		key = meta.APIKey
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if meta.Mode == constant.RelayModeClaude {
		version := c.Request.Header.Get("anthropic-version")
		if version == "" {
			version = "2023-06-01"
		}
		req.Header.Set("anthropic-version", version)
		if beta := c.Request.Header.Get("anthropic-beta"); beta != "" {
			req.Header.Set("anthropic-beta", beta)
		}
	}
}
