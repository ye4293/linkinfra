package baidu

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

// Adaptor 保留百度渠道编号，使用千帆 V2 和 Anthropic 兼容协议。
// Chat 复用 OpenAI 响应处理，Responses/Messages 由原生控制器处理。
type Adaptor struct {
	openai.Adaptor
}

// NormalizeBaseURL 供推理和模型发现共用，兼容 SDK 风格的地址配置。
func NormalizeBaseURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || base == "https://aip.baidubce.com" {
		base = common.ChannelBaseURLs[common.ChannelTypeBaidu]
	}
	for _, suffix := range []string{"/anthropic/v1", "/anthropic", "/v2", "/v1"} {
		if strings.HasSuffix(base, suffix) {
			return strings.TrimSuffix(base, suffix)
		}
	}
	return base
}

func (a *Adaptor) GetRequestURL(meta *util.RelayMeta) (string, error) {
	base := NormalizeBaseURL(meta.BaseURL)
	path := meta.RequestURLPath
	if path == "" {
		switch meta.Mode {
		case constant.RelayModeClaude:
			path = "/v1/messages"
		case constant.RelayModeOpenaiResponse:
			path = "/v1/responses"
		case constant.RelayModeEmbeddings:
			path = "/v1/embeddings"
		default:
			path = "/v1/chat/completions"
		}
	}
	if meta.Mode == constant.RelayModeClaude {
		return base + "/anthropic" + path, nil
	}
	// 保留子路径与查询参数，只替换客户端协议版本前缀。
	path = strings.TrimPrefix(path, "/v1/")
	path = strings.TrimPrefix(path, "/v2/")
	return base + "/v2/" + path, nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Request, meta *util.RelayMeta) error {
	channel.SetupCommonRequestHeader(c, req, meta)
	key := meta.ActualAPIKey
	if key == "" {
		key = meta.APIKey
	}
	if meta.Mode == constant.RelayModeClaude {
		req.Header.Set("x-api-key", key)
		version := c.Request.Header.Get("anthropic-version")
		if version == "" {
			version = "2023-06-01"
		}
		req.Header.Set("anthropic-version", version)
		if beta := c.Request.Header.Get("anthropic-beta"); beta != "" {
			req.Header.Set("anthropic-beta", beta)
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if appID := c.Request.Header.Get("appid"); appID != "" {
		req.Header.Set("appid", appID)
	}
	return nil
}

// 千帆原生支持 max_tokens 与 max_completion_tokens，保留各自语义和优先级。
func (a *Adaptor) ConvertRequest(c *gin.Context, mode int, request *model.GeneralOpenAIRequest) (any, error) {
	if request == nil {
		return nil, errors.New("request is nil")
	}
	// 后台渠道测试直接传入构造好的对象，不一定有原始 HTTP 请求体。
	if c == nil || c.Request == nil || c.Request.Body == nil || c.Request.Body == http.NoBody {
		return request, nil
	}
	// 保留千帆扩展参数及显式零值，只替换已经解析的模型映射。
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
	return raw, err
}

// 必须用外层适配器发起请求，避免嵌入方法绕过千帆 URL 和鉴权设置。
func (a *Adaptor) DoRequest(c *gin.Context, meta *util.RelayMeta, body io.Reader) (*http.Response, error) {
	return channel.DoRequestHelper(a, c, meta, body)
}

func (a *Adaptor) GetChannelName() string            { return "qianfanv2" }
func (a *Adaptor) GetModelList() []string            { return ModelList }
func (a *Adaptor) GetModelDetails() []model.APIModel { return ModelDetails }
