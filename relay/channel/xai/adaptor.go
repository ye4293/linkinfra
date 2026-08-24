package xai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/relay/channel"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

type Adaptor struct {
	ChannelType int
}

func (a *Adaptor) Init(meta *util.RelayMeta) {
	a.ChannelType = meta.ChannelType
}

func (a *Adaptor) GetRequestURL(meta *util.RelayMeta) (string, error) {
	base := strings.TrimRight(meta.BaseURL, "/")
	// 保留 Responses 查询/删除等带资源 ID 的路径以及查询参数。
	if meta.RequestURLPath != "" {
		return util.GetFullRequestURL(base, meta.RequestURLPath, meta.ChannelType), nil
	}
	switch meta.Mode {
	case constant.RelayModeClaude:
		return base + "/v1/messages", nil
	case constant.RelayModeOpenaiResponse:
		return base + "/v1/responses", nil
	}
	fullrequestUrl := fmt.Sprintf("%s%s", base, "/v1/chat/completions")
	return fullrequestUrl, nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Request, meta *util.RelayMeta) error {
	channel.SetupCommonRequestHeader(c, req, meta)
	req.Header.Set("Authorization", "Bearer "+meta.APIKey)
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
	if relayMode == constant.RelayModeClaude || relayMode == constant.RelayModeOpenaiResponse {
		return request, nil
	}
	// 处理模型名称以 "-search" 结尾的情况
	if strings.HasSuffix(request.Model, "-search") {
		request.Model = strings.TrimSuffix(request.Model, "-search")

		// 直接设置 SearchParameters
		mode := "on"
		request.SearchParameters = &model.SearchParameters{
			Mode: &mode,
		}
	}

	// 第一个转换：处理模型名称和 ReasoningEffort
	modelName := request.Model
	suffixes := []string{"low", "high", "medium"}

	for _, suffix := range suffixes {
		if strings.HasSuffix(modelName, "-"+suffix) {
			// 移除后缀，设置为基础模型名
			request.Model = strings.TrimSuffix(modelName, "-"+suffix)
			// 设置 ReasoningEffort
			request.ReasoningEffort = suffix
			break
		}
	}

	// 第二个转换：处理 MaxTokens 和 MaxCompletionTokens
	if request.MaxTokens > 0 {
		request.MaxCompletionTokens = request.MaxTokens
		request.MaxTokens = 0
	}

	if request.Stream {
		// 直接在原请求结构中设置 StreamOptions
		request.StreamOptions = &model.StreamOptions{
			IncludeUsage: true,
		}
	}

	return request, nil
}

func (a *Adaptor) ConvertImageRequest(request *model.ImageRequest) (any, error) {
	xaiRequest := model.ImageRequest{
		Model:          request.Model,
		Prompt:         request.Prompt,
		N:              request.N,
		ResponseFormat: request.ResponseFormat,
	}
	return xaiRequest, nil
}

func (a *Adaptor) DoRequest(c *gin.Context, meta *util.RelayMeta, requestBody io.Reader) (*http.Response, error) {
	return channel.DoRequestHelper(a, c, meta, requestBody)
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, meta *util.RelayMeta) (usage *model.Usage, err *model.ErrorWithStatusCode) {
	if meta.IsStream {
		err, _, usage = StreamHandler(c, resp, 1)
	} else {
		err, usage = Handler(c, resp, meta.PromptTokens, meta.ActualModelName)
	}
	return
}

// HandleErrorResponse 处理XAI特有的错误响应格式
func (a *Adaptor) HandleErrorResponse(resp *http.Response) *model.ErrorWithStatusCode {
	defer func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return &model.ErrorWithStatusCode{
			Error: model.Error{
				Message: "failed to read error response body",
				Type:    "api_error",
				Code:    "read_error_failed",
			},
			StatusCode: resp.StatusCode,
		}
	}

	responseBodyStr := string(responseBody)

	var xaiError map[string]interface{}
	if unmarshalErr := json.Unmarshal(responseBody, &xaiError); unmarshalErr == nil {
		// 优先解析 OpenAI 兼容格式: {"error": {"message": ..., "type": ..., "code": ...}}
		if errObj, hasError := xaiError["error"]; hasError {
			if errMap, ok := errObj.(map[string]interface{}); ok {
				errMsg, _ := errMap["message"].(string)
				errType, _ := errMap["type"].(string)
				errCode, _ := errMap["code"].(string)
				// 对 429 限流响应，不上报会触发渠道永久禁用的 type
				if resp.StatusCode == http.StatusTooManyRequests && (errType == "insufficient_quota" || errType == "rate_limit_error") {
					errType = "rate_limit_error"
				}
				return &model.ErrorWithStatusCode{
					Error: model.Error{
						Message: errMsg,
						Type:    errType,
						Code:    errCode,
					},
					StatusCode: resp.StatusCode,
				}
			}
		}

		// 解析 XAI 特有格式: {"code": ..., "error": "..."}
		if code, hasCode := xaiError["code"]; hasCode {
			if errorMsg, hasError := xaiError["error"]; hasError {
				return &model.ErrorWithStatusCode{
					Error: model.Error{
						Message: fmt.Sprintf("%v", errorMsg),
						Type:    "api_error",
						Code:    fmt.Sprintf("%v", code),
					},
					StatusCode: resp.StatusCode,
				}
			}
		}

		if message, hasMessage := xaiError["message"]; hasMessage {
			return &model.ErrorWithStatusCode{
				Error: model.Error{
					Message: fmt.Sprintf("%v", message),
					Type:    "api_error",
					Code:    "unknown_error",
				},
				StatusCode: resp.StatusCode,
			}
		}
	}

	return &model.ErrorWithStatusCode{
		Error: model.Error{
			Message: responseBodyStr,
			Type:    "api_error",
			Code:    "unknown_error",
		},
		StatusCode: resp.StatusCode,
	}
}

func (a *Adaptor) GetModelList() []string {
	return modelList
}

func (a *Adaptor) GetChannelName() string {
	return channelName
}

func (a *Adaptor) GetModelDetails() []model.APIModel {
	return ModelDetails
}
