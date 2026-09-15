package mimo

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

// Adaptor follows MiniMax's OpenAI-compatible chat and native Responses/Messages
// handling. All three protocols share the channel's base URL and API key.
type Adaptor struct {
	openai.Adaptor
}

func (a *Adaptor) GetRequestURL(meta *util.RelayMeta) (string, error) {
	base := strings.TrimRight(meta.BaseURL, "/")
	if base == "" {
		base = common.ChannelBaseURLs[common.ChannelTypeMimo]
	}
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
	return util.GetFullRequestURL(base, path, meta.ChannelType), nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Request, meta *util.RelayMeta) error {
	channel.SetupCommonRequestHeader(c, req, meta)
	key := meta.ActualAPIKey
	if key == "" {
		key = meta.APIKey
	}
	// MiMo accepts Bearer authentication for all three protocols.
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
	return nil
}

// Use the outer adaptor so URL/header overrides also apply to actual requests.
func (a *Adaptor) DoRequest(c *gin.Context, meta *util.RelayMeta, requestBody io.Reader) (*http.Response, error) {
	return channel.DoRequestHelper(a, c, meta, requestBody)
}

func (a *Adaptor) GetChannelName() string {
	return "mimo"
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetModelDetails() []model.APIModel {
	return ModelDetails
}
