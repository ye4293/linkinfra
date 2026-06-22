package xai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/config"
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
		return openaiAdaptor.ErrorWrapper(fmt.Errorf("insufficient user balance"), "User balance is not enough", http.StatusBadRequest)
	}
	return nil
}

type VideoAdaptor struct {
	relaychannel.BaseVideoAdaptor
}

func (a *VideoAdaptor) GetProviderName() string { return "grok" }
func (a *VideoAdaptor) GetChannelName() string  { return "xAI Grok" }
func (a *VideoAdaptor) GetSupportedModels() []string {
	return []string{"grok-imagine-video", "grok-imagine-video-extensions"}
}

func (a *VideoAdaptor) HandleVideoRequest(c *gin.Context, req *model.VideoRequest, meta *util.RelayMeta) (*relaychannel.VideoTaskResult, *model.ErrorWithStatusCode) {
	baseUrl := meta.BaseURL
	if baseUrl == "" {
		baseUrl = "https://api.x.ai"
	}

	isExtension := strings.HasSuffix(meta.OriginModelName, "-extensions")

	var grokParams struct {
		Duration   int    `json:"duration"`
		Resolution string `json:"resolution"`
		Video      *struct {
			URL string `json:"url"`
		} `json:"video,omitempty"`
		Image *struct {
			URL string `json:"url"`
		} `json:"image,omitempty"`
	}

	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "read_request_body_failed", http.StatusBadRequest)
	}
	if err := json.Unmarshal(bodyBytes, &grokParams); err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "invalid_video_request", http.StatusBadRequest)
	}

	duration := grokParams.Duration
	hasVideoInput := grokParams.Video != nil && grokParams.Video.URL != ""
	hasImageInput := grokParams.Image != nil && grokParams.Image.URL != ""

	// 解析输入视频时长（编辑或延长时）
	var videoDuration float64
	if hasVideoInput {
		if dur, durErr := GetRemoteMP4Duration(grokParams.Video.URL); durErr != nil {
			log.Printf("[Grok Video] 解析输入视频时长失败 - url=%s, error=%v", grokParams.Video.URL, durErr)
		} else {
			videoDuration = dur
			log.Printf("[Grok Video] 输入视频时长 - %.2f 秒", videoDuration)
		}
	}

	resolution := grokParams.Resolution
	if resolution == "" {
		resolution = "480p"
	}

	// 确定请求端点 & duration 校验
	var fullRequestUrl string
	var sendBody []byte

	if isExtension {
		// extensions: duration 1-10, 默认 6
		if duration <= 0 {
			duration = 6
		}
		if duration > 10 {
			duration = 10
		}
		if !hasVideoInput {
			return nil, openaiAdaptor.ErrorWrapper(
				fmt.Errorf("video.url is required for video extensions"),
				"invalid_video_request", http.StatusBadRequest)
		}
		fullRequestUrl = baseUrl + "/v1/videos/extensions"
		// xAI API 只认识 grok-imagine-video，需要把 model 名替换回去
		var bodyMap map[string]any
		if err := json.Unmarshal(bodyBytes, &bodyMap); err == nil {
			bodyMap["model"] = "grok-imagine-video"
			if modified, err := json.Marshal(bodyMap); err == nil {
				sendBody = modified
			} else {
				sendBody = bodyBytes
			}
		} else {
			sendBody = bodyBytes
		}
		log.Printf("[Grok Video] 视频延长请求 - video_url=%s, duration=%d, resolution=%s", grokParams.Video.URL, duration, resolution)
	} else if hasVideoInput {
		// edits: 没有 duration 字段，需要从请求体中移除
		fullRequestUrl = baseUrl + "/v1/videos/edits"
		var bodyMap map[string]any
		if err := json.Unmarshal(bodyBytes, &bodyMap); err == nil {
			delete(bodyMap, "duration")
			if modified, err := json.Marshal(bodyMap); err == nil {
				sendBody = modified
			} else {
				sendBody = bodyBytes
			}
		} else {
			sendBody = bodyBytes
		}
		log.Printf("[Grok Video] 视频编辑请求 - video_url=%s, video_duration=%.2f, resolution=%s", grokParams.Video.URL, videoDuration, resolution)
	} else {
		// generations: duration 1-15, 默认 8
		if duration <= 0 {
			duration = 8
		}
		if duration > 15 {
			duration = 15
		}
		fullRequestUrl = baseUrl + "/v1/videos/generations"
		sendBody = bodyBytes
		log.Printf("[Grok Video] 视频生成请求 - duration=%d, resolution=%s", duration, resolution)
	}

	// 计算 quota
	outputPrice := 0.05
	if resolution == "720p" {
		outputPrice = 0.07
	}

	var quota int64
	if isExtension {
		// extensions: 输出费 = outputPrice × extension_duration, 输入费 = $0.01 × 输入视频时长
		total := float64(duration)*outputPrice + videoDuration*0.01
		quota = int64(total * config.QuotaPerUnit)
	} else if hasVideoInput {
		// edits: 输出费 = outputPrice × 输入视频时长, 输入费 = $0.01 × 输入视频时长
		// 如果解析失败 videoDuration=0, 用预扣费兜底
		if videoDuration > 0 {
			total := videoDuration*(outputPrice+0.01)
			quota = int64(total * config.QuotaPerUnit)
		} else {
			// 解析失败，使用预扣费默认值 $0.20
			quota = int64(0.2 * config.QuotaPerUnit)
		}
	} else {
		// generations: 输出费 = outputPrice × duration, 输入费按图片/文本
		total := float64(duration) * outputPrice
		if hasImageInput {
			total += 0.002
		}
		quota = int64(total * config.QuotaPerUnit)
	}
	log.Printf("[Grok Video] 预扣费用 - quota=%d, resolution=%s", quota, resolution)

	if balErr := checkBalance(c, meta, quota); balErr != nil {
		return nil, balErr
	}

	httpReq, err := http.NewRequest(http.MethodPost, fullRequestUrl, bytes.NewReader(sendBody))
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "create_request_error", http.StatusInternalServerError)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+meta.APIKey)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "request_error", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "read_response_error", http.StatusInternalServerError)
	}

	log.Printf("[Grok Video] 响应状态码: %d, 响应体: %s", resp.StatusCode, string(body))

	var grokResponse GrokVideoResponse
	if err := json.Unmarshal(body, &grokResponse); err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "response_parse_error", http.StatusInternalServerError)
	}
	grokResponse.StatusCode = resp.StatusCode

	if grokResponse.StatusCode != 200 && grokResponse.StatusCode != 202 {
		errorMsg := grokResponse.GetError()
		if errorMsg == "" {
			errorMsg = string(body)
		}
		return nil, openaiAdaptor.ErrorWrapper(
			fmt.Errorf("%s: %s", grokResponse.Code, errorMsg),
			"api_error", grokResponse.StatusCode)
	}

	// Credentials 字段由 invokeVideoAdaptorRequest 在 CreateVideoLog 之后写入 DB
	// 此处不直接调用 UpdateVideoCredentials，避免记录未创建导致写入失败
	durationStr := strconv.Itoa(duration)
	if hasVideoInput && !isExtension {
		// edits 没有 duration 字段，用解析出的输入视频时长
		if videoDuration > 0 {
			durationStr = fmt.Sprintf("%.0f", videoDuration)
		} else {
			durationStr = ""
		}
	}
	return &relaychannel.VideoTaskResult{
		TaskId:        grokResponse.RequestId,
		TaskStatus:    "succeed",
		Duration:      durationStr,
		Resolution:    resolution,
		Quota:         quota,
		Credentials:   meta.APIKey,
		VideoDuration: videoDuration,
	}, nil
}

func (a *VideoAdaptor) HandleVideoResult(c *gin.Context, videoTask *dbmodel.Video, ch *dbmodel.Channel, cfg *dbmodel.ChannelConfig) (*model.GeneralFinalVideoResponse, *model.ErrorWithStatusCode) {
	taskId := videoTask.TaskId

	// Return cached URL if already stored
	if videoTask.StoreUrl != "" {
		log.Printf("[Grok Video] 使用缓存的视频URL - taskId=%s", taskId)
		var videoUrls []string
		if err := json.Unmarshal([]byte(videoTask.StoreUrl), &videoUrls); err != nil {
			videoUrls = []string{videoTask.StoreUrl}
		}
		videoResults := make([]model.VideoResultItem, len(videoUrls))
		for i, u := range videoUrls {
			videoResults[i] = model.VideoResultItem{Url: u}
		}
		return &model.GeneralFinalVideoResponse{
			TaskId:       taskId,
			VideoId:      taskId,
			TaskStatus:   "succeed",
			Message:      "Video retrieved from cache",
			VideoResult:  videoUrls[0],
			VideoResults: videoResults,
			Duration:     videoTask.Duration,
		}, nil
	}

	// Resolve API key: prefer saved credentials, fall back to channel key
	apiKey := videoTask.Credentials
	if apiKey == "" {
		keys := strings.Split(ch.Key, "\n")
		for _, k := range keys {
			k = strings.TrimSpace(k)
			if k != "" {
				apiKey = k
				break
			}
		}
	}

	baseUrl := *ch.BaseURL
	if baseUrl == "" {
		baseUrl = "https://api.x.ai"
	}
	url := fmt.Sprintf("%s/v1/videos/%s", baseUrl, taskId)

	httpReq, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "create_request_error", http.StatusInternalServerError)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "request_error", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "read_response_error", http.StatusInternalServerError)
	}

	log.Printf("[Grok Video] 查询响应 - taskId=%s, statusCode=%d, body=%s", taskId, resp.StatusCode, string(body))

	var grokResult GrokVideoResult
	if err := json.Unmarshal(body, &grokResult); err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "json_parse_error", http.StatusInternalServerError)
	}

	generalResponse := &model.GeneralFinalVideoResponse{
		TaskId:     taskId,
		VideoId:    taskId,
		TaskStatus: "processing",
		Message:    "Video is still processing",
		Duration:   videoTask.Duration,
	}

	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		errorMsg := grokResult.GetError()
		if errorMsg == "" {
			errorMsg = string(body)
		}
		failMessage := fmt.Sprintf("%s: %s", grokResult.Code, errorMsg)
		log.Printf("[Grok Video] 查询错误 - taskId=%s, code: %s, error: %s", taskId, grokResult.Code, errorMsg)
		generalResponse.TaskStatus = "failed"
		generalResponse.Message = failMessage
		return generalResponse, nil
	}

	if grokResult.Video != nil && grokResult.Video.URL != "" {
		log.Printf("[Grok Video] 视频完成 - taskId=%s, url=%s", taskId, grokResult.Video.URL)
		if updateErr := dbmodel.UpdateVideoStoreUrl(taskId, grokResult.Video.URL); updateErr != nil {
			log.Printf("[Grok Video] 保存URL失败 - taskId=%s, error=%v", taskId, updateErr)
		}
		generalResponse.TaskStatus = "succeed"
		generalResponse.Message = "Video generation completed"
		generalResponse.VideoResult = grokResult.Video.URL
		generalResponse.VideoResults = []model.VideoResultItem{{Url: grokResult.Video.URL}}
		if grokResult.Video.Duration > 0 {
			generalResponse.Duration = strconv.Itoa(grokResult.Video.Duration)
		}
		// 转换 usage: cost_in_usd_ticks → cost_in_usd (1 USD = 10,000,000,000 ticks)
		if grokResult.Usage != nil && grokResult.Usage.CostInUsdTicks > 0 {
			costInUsd := float64(grokResult.Usage.CostInUsdTicks) / 10_000_000_000.0
			generalResponse.Usage = &model.VideoUsage{CostInUsd: costInUsd}
			log.Printf("[Grok Video] 费用 - taskId=%s, ticks=%d, usd=%.6f", taskId, grokResult.Usage.CostInUsdTicks, costInUsd)
		}
	} else if grokResult.Status == "failed" || grokResult.GetError() != "" {
		// status=failed 或 error 对象非空，都视为失败
		errMsg := grokResult.GetError()
		if errMsg == "" {
			errMsg = "unknown error"
		}
		generalResponse.TaskStatus = "failed"
		generalResponse.Message = fmt.Sprintf("Video generation failed: %s", errMsg)
	} else if grokResult.Status == "pending" || grokResult.Status == "in_progress" {
		generalResponse.TaskStatus = "processing"
		generalResponse.Message = "Video generation in progress"
	} else {
		generalResponse.TaskStatus = "processing"
		generalResponse.Message = fmt.Sprintf("Video status: %s", grokResult.Status)
	}

	return generalResponse, nil
}
