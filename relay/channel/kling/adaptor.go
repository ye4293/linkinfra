package kling

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

type Adaptor struct {
	RequestType string // text2video/omni-video/image2video/multi-image2video
	FullPath    string // 透传模式下使用完整路径（包含 ID 等参数）
}

func (a *Adaptor) Init(meta *util.RelayMeta) {
	// 初始化逻辑
}

func (a *Adaptor) GetRequestURL(meta *util.RelayMeta) (string, error) {
	baseURL := meta.BaseURL
	if baseURL == "" {
		baseURL = "https://api-beijing.klingai.com"
	}

	// 透传模式：直接使用完整路径
	if a.FullPath != "" {
		return baseURL + a.FullPath, nil
	}

	// 根据请求类型确定路径前缀
	var pathPrefix string
	switch a.RequestType {
	// 音频类接口
	case RequestTypeTextToAudio, RequestTypeVideoToAudio, RequestTypeTTS:
		pathPrefix = "/v1/audio"
	// 图片类接口
	case RequestTypeImageGeneration, RequestTypeOmniImage, RequestTypeMultiImage2Image, RequestTypeImageExpand:
		pathPrefix = "/v1/images"
	// 通用类接口（包括查询和管理接口）
	case RequestTypeCustomElements, RequestTypeAdvancedCustomElements, RequestTypeCustomVoices,
		RequestTypePresetsElements, RequestTypeDeleteElements,
		RequestTypePresetsVoices, RequestTypeDeleteVoices:
		pathPrefix = "/v1/general"
	// 默认：视频类接口
	default:
		pathPrefix = "/v1/videos"
	}

	return fmt.Sprintf("%s%s/%s", baseURL, pathPrefix, a.RequestType), nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Request, meta *util.RelayMeta) error {
	req.Header.Set("Content-Type", "application/json")
	// Authorization header 已经在 middleware/distributor.go 中设置为 JWT token
	// 如果没有设置，则使用 APIKey
	authHeader := c.Request.Header.Get("Authorization")
	if authHeader == "" {
		req.Header.Set("Authorization", "Bearer "+meta.APIKey)
	} else {
		req.Header.Set("Authorization", authHeader)
	}
	return nil
}

// ConvertRequest 转换请求并注入回调URL和外部任务ID
func (a *Adaptor) ConvertRequest(c *gin.Context, meta *util.RelayMeta, requestBody map[string]interface{}, callbackURL string, externalTaskID int64) ([]byte, error) {
	// 注入 model_name（如果请求体中没有）
	if _, exists := requestBody["model_name"]; !exists {
		if modelValue, ok := c.Get("model"); ok {
			if modelStr, isString := modelValue.(string); isString && modelStr != "" {
				requestBody["model_name"] = modelStr
			}
		}
	}

	// 删除 model 字段（Kling API 使用 model_name）
	delete(requestBody, "model")

	// 注入回调 URL（如果系统配置了回调域名）
	if callbackURL != "" {
		requestBody["callback_url"] = callbackURL
	}

	// 注入外部任务ID（用于关联系统内部任务）
	// if externalTaskID > 0 {
	// 	requestBody["external_task_id"] = fmt.Sprintf("%d", externalTaskID)
	// }

	return json.Marshal(requestBody)
}

func (a *Adaptor) DoRequest(c *gin.Context, meta *util.RelayMeta, requestBody io.Reader) (*http.Response, error) {
	return a.DoRequestWithMethod(c, meta, "POST", requestBody)
}

// DoRequestWithMethod 执行指定 HTTP 方法的请求
func (a *Adaptor) DoRequestWithMethod(c *gin.Context, meta *util.RelayMeta, method string, requestBody io.Reader) (*http.Response, error) {
	fullRequestURL, err := a.GetRequestURL(meta)
	if err != nil {
		return nil, err
	}

	// 对于查询接口，需要将路径参数添加到 URL 中
	// 例如: /v1/general/custom-voices/{id}
	if c.Param("id") != "" {
		fullRequestURL = fullRequestURL + "/" + c.Param("id")
	}

	// 添加查询参数
	if len(c.Request.URL.RawQuery) > 0 {
		fullRequestURL = fullRequestURL + "?" + c.Request.URL.RawQuery
	}

	req, err := http.NewRequest(method, fullRequestURL, requestBody)
	if err != nil {
		return nil, err
	}

	if err := a.SetupRequestHeader(c, req, meta); err != nil {
		return nil, err
	}

	return util.HTTPClient.Do(req)
}

// DoResponse 处理响应并返回完整的 Kling 响应（透传）
func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, meta *util.RelayMeta) (*KlingResponse, *model.ErrorWithStatusCode) {
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, &model.ErrorWithStatusCode{
			StatusCode: http.StatusInternalServerError,
			Error:      model.Error{Message: "failed to read response: " + readErr.Error()},
		}
	}

	var klingResp KlingResponse
	if unmarshalErr := json.Unmarshal(body, &klingResp); unmarshalErr != nil {
		return nil, &model.ErrorWithStatusCode{
			StatusCode: http.StatusInternalServerError,
			Error:      model.Error{Message: "failed to parse response: " + unmarshalErr.Error()},
		}
	}

	// 记录日志（不管成功失败）
	logger.Debug(c, fmt.Sprintf("Kling response: code=%d, task_id=%s, status=%s, message=%s",
		klingResp.Code, klingResp.GetTaskID(), klingResp.GetTaskStatus(), klingResp.Message))

	// 不管成功失败，都透传原始 Kling 返回数据
	return &klingResp, nil
}

// QueryTaskStatus 查询任务状态
func (a *Adaptor) QueryTaskStatus(taskID string, meta *util.RelayMeta) (*QueryTaskResponse, error) {
	baseURL := meta.BaseURL
	if baseURL == "" {
		baseURL = "https://api-beijing.klingai.com"
	}

	url := fmt.Sprintf("%s/v1/videos/%s", baseURL, taskID)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+meta.APIKey)

	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result QueryTaskResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	if result.Code != 0 {
		return nil, fmt.Errorf("query failed: %s", result.Message)
	}

	return &result, nil
}
