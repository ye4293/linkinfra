package ali

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	dbmodel "github.com/songquanpeng/one-api/model"
	relaychannel "github.com/songquanpeng/one-api/relay/channel"
	openaiAdaptor "github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

// checkBalance verifies that the user has enough quota before sending the request
func checkBalance(c *gin.Context, meta *util.RelayMeta, quota int64) *model.ErrorWithStatusCode {
	userQuota, err := dbmodel.CacheGetUserQuota(c.Request.Context(), meta.UserId)
	if err != nil {
		return openaiAdaptor.ErrorWrapper(err, "get_user_quota_error", http.StatusInternalServerError)
	}
	if userQuota-quota < 0 {
		return openaiAdaptor.ErrorWrapper(fmt.Errorf("用户余额不足"), "User balance is not enough", http.StatusBadRequest)
	}
	return nil
}

const defaultDashScopeBaseURL = "https://dashscope.aliyuncs.com"

// GetBaseURL returns the DashScope base URL, preferring channel custom address
func (a *VideoAdaptor) GetBaseURL(meta *util.RelayMeta) string {
	if meta.BaseURL != "" {
		return strings.TrimRight(meta.BaseURL, "/")
	}
	return defaultDashScopeBaseURL
}

// DoCreate submits a video generation task (DashScope async mode)
func (a *VideoAdaptor) DoCreate(ctx context.Context, meta *util.RelayMeta, body []byte) (*http.Response, error) {
	url := a.GetBaseURL(meta) + "/api/v1/services/aigc/video-generation/video-synthesis"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+meta.APIKey)
	req.Header.Set("X-DashScope-Async", "enable")
	return http.DefaultClient.Do(req)
}

// DoQuery queries the task status
func (a *VideoAdaptor) DoQuery(ctx context.Context, meta *util.RelayMeta, taskID string) (*http.Response, error) {
	url := fmt.Sprintf("%s/api/v1/tasks/%s", a.GetBaseURL(meta), taskID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+meta.APIKey)
	return http.DefaultClient.Do(req)
}

// ParseCreateResponse parses the video creation task response body
func (a *VideoAdaptor) ParseCreateResponse(body []byte) (*AliVideoResponse, error) {
	var resp AliVideoResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ParseQueryResponse parses the task status query response body
func (a *VideoAdaptor) ParseQueryResponse(body []byte) (*AliVideoQueryResponse, error) {
	var resp AliVideoQueryResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

type VideoAdaptor struct {
	relaychannel.BaseVideoAdaptor
}

func (a *VideoAdaptor) GetProviderName() string { return "ali" }
func (a *VideoAdaptor) GetChannelName() string  { return "Alibaba Wanxiang" }
func (a *VideoAdaptor) GetSupportedModels() []string {
	return []string{"wan-x1", "wan-x1-14b", "wan2.1-i2v-14b-720p", "wan2.1-i2v-14b-480p", "wan2.1-t2v-14b", "wan2.1-t2v-turbo"}
}

func (a *VideoAdaptor) HandleVideoRequest(c *gin.Context, req *model.VideoRequest, meta *util.RelayMeta) (*relaychannel.VideoTaskResult, *model.ErrorWithStatusCode) {
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "read_request_body_failed", http.StatusBadRequest)
	}

	var requestData map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &requestData); err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "parse_request_body_failed", http.StatusBadRequest)
	}

	duration := "5"
	resolution := "1080P"
	modelName := meta.ActualModelName
	if m, ok := requestData["model"].(string); ok && m != "" {
		modelName = m
	}

	if parameters, ok := requestData["parameters"].(map[string]interface{}); ok {
		if dv, exists := parameters["duration"]; exists {
			switch v := dv.(type) {
			case float64:
				duration = strconv.Itoa(int(v))
			case string:
				duration = v
			}
		}
		if sv, ok := parameters["size"].(string); ok {
			resolution = parseAliVideoResolution(sv)
		}
		if rv, ok := parameters["resolution"].(string); ok {
			if rv == "480P" || rv == "720P" || rv == "1080P" {
				resolution = rv
			}
		}
	}

	videoType := "image-to-video"
	if strings.Contains(strings.ToLower(modelName), "t2v") {
		videoType = "text-to-video"
	}
	quota := common.CalculateVideoQuota(modelName, videoType, "*", duration, resolution, "")

	// 基于实际请求参数做精确余额校验，与重构前行为一致
	if balErr := checkBalance(c, meta, quota); balErr != nil {
		return nil, balErr
	}

	ch, err := dbmodel.GetChannelById(meta.ChannelId, true)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "get_channel_error", http.StatusInternalServerError)
	}

	url := fmt.Sprintf("%s/api/v1/services/aigc/video-generation/video-synthesis", meta.BaseURL)
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "create_request_error", http.StatusInternalServerError)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+ch.Key)
	httpReq.Header.Set("X-DashScope-Async", "enable")
	relaychannel.ApplyHeadersOverride(httpReq, meta)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "request_error", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "read_response_error", http.StatusInternalServerError)
	}

	var aliResp AliVideoResponse
	if parseErr := json.Unmarshal(body, &aliResp); parseErr != nil {
		return nil, openaiAdaptor.ErrorWrapper(parseErr, "parse_ali_video_response_failed", http.StatusInternalServerError)
	}

	if aliResp.Code != "" {
		return nil, openaiAdaptor.ErrorWrapper(
			fmt.Errorf("ali API error: [%s] %s", aliResp.Code, aliResp.Message),
			"api_error", http.StatusBadRequest)
	}

	taskId := ""
	if aliResp.Output != nil {
		taskId = aliResp.Output.TaskID
	}

	log.Printf("ali-video-duration: %s, resolution: %s, model: %s, quota: %d", duration, resolution, modelName, quota)

	return &relaychannel.VideoTaskResult{
		TaskId:     taskId,
		TaskStatus: "succeed",
		Duration:   duration,
		Resolution: resolution,
		Quota:      quota,
	}, nil
}

func (a *VideoAdaptor) HandleVideoResult(c *gin.Context, videoTask *dbmodel.Video, ch *dbmodel.Channel, cfg *dbmodel.ChannelConfig) (*model.GeneralFinalVideoResponse, *model.ErrorWithStatusCode) {
	taskId := videoTask.TaskId
	url := fmt.Sprintf("%s/api/v1/tasks/%s", *ch.BaseURL, taskId)

	_, body, err := relaychannel.SendVideoResultQuery(url, relaychannel.BearerAuthHeaders(ch.Key))
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "request_error", http.StatusInternalServerError)
	}

	log.Printf("Ali video query response body: %s", string(body))

	var aliResp AliVideoQueryResponse
	if parseErr := json.Unmarshal(body, &aliResp); parseErr != nil {
		return nil, openaiAdaptor.ErrorWrapper(parseErr, "json_parse_error", http.StatusInternalServerError)
	}

	generalResponse := &model.GeneralFinalVideoResponse{
		TaskId:     taskId,
		VideoId:    taskId,
		TaskStatus: "processing",
		Duration:   videoTask.Duration,
	}

	if aliResp.Code != "" {
		generalResponse.TaskStatus = "failed"
		generalResponse.Message = fmt.Sprintf("阿里云API错误: [%s] %s (request_id: %s)", aliResp.Code, aliResp.Message, aliResp.RequestID)
		return generalResponse, nil
	}

	if aliResp.Output == nil {
		generalResponse.TaskStatus = "failed"
		generalResponse.Message = fmt.Sprintf("未收到响应数据 (request_id: %s)", aliResp.RequestID)
		return generalResponse, nil
	}

	switch aliResp.Output.TaskStatus {
	case "SUCCEEDED":
		generalResponse.TaskStatus = "succeed"
		generalResponse.Message = fmt.Sprintf("Video generation completed, request_id: %s", aliResp.RequestID)
		if aliResp.Output.VideoURL != "" {
			generalResponse.VideoResult = aliResp.Output.VideoURL
			generalResponse.VideoResults = []model.VideoResultItem{{Url: aliResp.Output.VideoURL}}
		}
	case "FAILED":
		generalResponse.TaskStatus = "failed"
		if aliResp.Output.Code != "" && aliResp.Output.Message != "" {
			generalResponse.Message = fmt.Sprintf("视频生成失败: [%s] %s (request_id: %s)", aliResp.Output.Code, aliResp.Output.Message, aliResp.RequestID)
		} else if aliResp.Output.Message != "" {
			generalResponse.Message = fmt.Sprintf("视频生成失败: %s (request_id: %s)", aliResp.Output.Message, aliResp.RequestID)
		} else {
			generalResponse.Message = fmt.Sprintf("视频生成失败 (request_id: %s)", aliResp.RequestID)
		}
	case "UNKNOWN":
		generalResponse.TaskStatus = "failed"
		generalResponse.Message = fmt.Sprintf("任务已过期或未知 (request_id: %s)", aliResp.RequestID)
	default:
		generalResponse.TaskStatus = "processing"
		generalResponse.Message = fmt.Sprintf("Video generation in progress, request_id: %s", aliResp.RequestID)
	}

	return generalResponse, nil
}

func parseAliVideoResolution(size string) string {
	sizes1080P := map[string]bool{
		"1920*1080": true, "1080*1920": true, "1440*1440": true,
		"1632*1248": true, "1248*1632": true,
	}
	sizes720P := map[string]bool{
		"1280*720": true, "720*1280": true, "960*960": true,
		"1088*832": true, "832*1088": true,
	}
	sizes480P := map[string]bool{
		"832*480": true, "480*832": true, "624*624": true,
	}
	normalized := strings.ReplaceAll(size, "x", "*")
	if sizes1080P[normalized] {
		return "1080P"
	}
	if sizes720P[normalized] {
		return "720P"
	}
	if sizes480P[normalized] {
		return "480P"
	}
	parts := strings.Split(normalized, "*")
	if len(parts) == 2 {
		w, e1 := strconv.Atoi(parts[0])
		h, e2 := strconv.Atoi(parts[1])
		if e1 == nil && e2 == nil {
			px := w * h
			if px >= 1500000 {
				return "1080P"
			}
			if px >= 600000 {
				return "720P"
			}
			return "480P"
		}
	}
	return "1080P"
}
