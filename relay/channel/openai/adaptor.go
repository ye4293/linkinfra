package openai

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/relay/channel"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

type Adaptor struct {
	ChannelType int
}

// ConvertImageRequest implements channel.Adaptor.
func (a *Adaptor) ConvertImageRequest(request *model.ImageRequest) (any, error) {
	panic("unimplemented")
}

func (a *Adaptor) Init(meta *util.RelayMeta) {
	a.ChannelType = meta.ChannelType
}

func (a *Adaptor) GetRequestURL(meta *util.RelayMeta) (string, error) {
	switch meta.ChannelType {
	case common.ChannelTypeDeepseek:
		return deepseekRequestURL(meta), nil
	case common.ChannelTypeAzure:
		// https://learn.microsoft.com/en-us/azure/cognitive-services/openai/chatgpt-quickstart?pivots=rest-api&tabs=command-line#rest-api
		requestURL := strings.Split(meta.RequestURLPath, "?")[0]
		requestURL = fmt.Sprintf("%s?api-version=%s", requestURL, meta.Config.APIVersion)
		task := strings.TrimPrefix(requestURL, "/v1/")
		model_ := meta.ActualModelName
		// 2025年5月10日后创建的渠道不移除. （与 new-api relay/channel/openai/adaptor.go 一致）
		if meta.ChannelCreateTime < constant.AzureNoRemoveDotTime {
			model_ = strings.Replace(model_, ".", "", -1)
		}
		// /v1/responses 使用扁平端点（不带 deployment 前缀），model 由请求体指定
		responsePath := strings.TrimRight(strings.Split(meta.RequestURLPath, "?")[0], "/")
		if responsePath == "/v1/responses" || responsePath == "/v1/responses/compact" {
			requestURL = fmt.Sprintf("/openai%s?api-version=%s", strings.TrimPrefix(responsePath, "/v1"), meta.Config.APIVersion)
			return util.GetFullRequestURL(meta.BaseURL, requestURL, meta.ChannelType), nil
		}
		//https://github.com/songquanpeng/one-api/issues/1191
		// {your endpoint}/openai/deployments/{your azure_model}/chat/completions?api-version={api_version}
		requestURL = fmt.Sprintf("/openai/deployments/%s/%s", model_, task)
		return util.GetFullRequestURL(meta.BaseURL, requestURL, meta.ChannelType), nil
	default:
		return util.GetFullRequestURL(meta.BaseURL, meta.RequestURLPath, meta.ChannelType), nil
	}
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Request, meta *util.RelayMeta) error {
	channel.SetupCommonRequestHeader(c, req, meta)
	if meta.ChannelType == common.ChannelTypeDeepseek {
		setupDeepseekRequestHeader(c, req, meta)
		return nil
	}
	if meta.ChannelType == common.ChannelTypeAzure {
		req.Header.Set("api-key", meta.APIKey)
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+meta.APIKey)
	if meta.ChannelType == common.ChannelTypeOpenRouter {
		req.Header.Set("HTTP-Referer", "https://www.ezlinkai.com/")
		req.Header.Set("X-Title", "EZLINK AI")
	}
	return nil
}

func (a *Adaptor) ConvertRequest(c *gin.Context, relayMode int, request *model.GeneralOpenAIRequest) (any, error) {
	if request == nil {
		return nil, errors.New("request is nil")
	}

	if strings.Contains(strings.ToLower(request.Model), "audio") && request.Stream {
		request.StreamOptions = &model.StreamOptions{
			IncludeUsage: true,
		}
	}

	// 统一将 max_tokens 转换为 max_completion_tokens，不再区分模型
	if request.MaxTokens > 0 {
		request.MaxCompletionTokens = request.MaxTokens
		request.MaxTokens = 0
	}

	return request, nil
}

func (a *Adaptor) DoRequest(c *gin.Context, meta *util.RelayMeta, requestBody io.Reader) (*http.Response, error) {
	return channel.DoRequestHelper(a, c, meta, requestBody)
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, meta *util.RelayMeta) (usage *model.Usage, err *model.ErrorWithStatusCode) {
	if meta.IsStream {
		var responseText string
		err, responseText, usage = StreamHandler(c, resp, meta.Mode)
		if usage == nil || usage.TotalTokens == 0 {
			usage = ResponseText2Usage(responseText, meta.ActualModelName, meta.PromptTokens)
		}
	} else {
		err, usage = Handler(c, resp, meta.PromptTokens, meta.ActualModelName)
	}
	return
}

func (a *Adaptor) GetModelList() []string {
	_, modelList := GetCompatibleChannelMeta(a.ChannelType)
	return modelList
}

func (a *Adaptor) GetChannelName() string {
	channelName, _ := GetCompatibleChannelMeta(a.ChannelType)
	return channelName
}

func (a *Adaptor) GetModelDetails() []model.APIModel {
	return ModelDetails
}
