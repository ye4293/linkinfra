package controller

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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	dbmodel "github.com/songquanpeng/one-api/model"
	relaychannel "github.com/songquanpeng/one-api/relay/channel"
	"github.com/songquanpeng/one-api/relay/channel/ali"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/channel/runway"
	"github.com/songquanpeng/one-api/relay/channel/vertexai"
	relayhelper "github.com/songquanpeng/one-api/relay/helper"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

func DoVideoRequest(c *gin.Context, modelName string) *model.ErrorWithStatusCode {
	ctx := c.Request.Context()
	var videoRequest model.VideoRequest
	err := common.UnmarshalBodyReusable(c, &videoRequest)
	meta := util.GetRelayMeta(c)
	if err != nil {
		return openai.ErrorWrapper(err, "invalid_text_request", http.StatusBadRequest)
	}

	// 适配器路由：已迁移供应商由对应 VideoAdaptor 处理
	if adaptor := relayhelper.GetVideoAdaptor(modelName); adaptor != nil {
		return invokeVideoAdaptorRequest(c, ctx, adaptor, &videoRequest, meta)
	}

	if strings.HasPrefix(modelName, "video-01") ||
		strings.HasPrefix(modelName, "S2V-01") ||
		strings.HasPrefix(modelName, "T2V-01") ||
		strings.HasPrefix(modelName, "I2V-01") ||
		strings.HasPrefix(strings.ToLower(modelName), "minimax") {
		return handleMinimaxVideoRequest(c, ctx, videoRequest, meta)
	} else if modelName == "cogvideox" {
		return handleZhipuVideoRequest(c, ctx, videoRequest, meta)
	} else if modelName == "kling-identify-face" {
		DoIdentifyFace(c)
		return nil
	} else if modelName == "kling-advanced-lip-sync" {
		DoAdvancedLipSync(c)
		return nil
	} else if modelName == "gen3a_turbo" {
		return handleRunwayVideoRequest(c, ctx, videoRequest, meta)
	} else {
		return openai.ErrorWrapper(fmt.Errorf("unsupported model"), "unsupported_model", http.StatusBadRequest)
	}
}

func handleMinimaxVideoRequest(c *gin.Context, ctx context.Context, videoRequest model.VideoRequest, meta *util.RelayMeta) *model.ErrorWithStatusCode {

	baseUrl := meta.BaseURL
	fullRequestUrl := baseUrl + "/v1/video_generation"

	// 读取原始请求体
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return openai.ErrorWrapper(err, "read_request_body_failed", http.StatusBadRequest)
	}

	// 先解析为 map 以便处理 duration 的多种类型
	var requestMap map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &requestMap); err != nil {
		return openai.ErrorWrapper(err, "invalid_request_error", http.StatusBadRequest)
	}

	// 处理 duration 字段，兼容多种类型（int、float64、string）
	var durationInt int
	if durationValue, exists := requestMap["duration"]; exists && durationValue != nil {
		switch v := durationValue.(type) {
		case float64:
			durationInt = int(v)
		case string:
			parsed, parseErr := strconv.Atoi(v)
			if parseErr == nil {
				durationInt = parsed
			} else {
				durationInt = 6 // 解析失败使用默认值
			}
		case int:
			durationInt = v
		default:
			durationInt = 6 // 未知类型使用默认值
		}
	}

	// 如果没有传递或值为 0，设置默认值
	if durationInt == 0 {
		durationInt = 6
		requestMap["duration"] = 6
	} else {
		requestMap["duration"] = durationInt
	}

	// 重新序列化为 JSON
	modifiedBodyBytes, err := json.Marshal(requestMap)
	if err != nil {
		return openai.ErrorWrapper(err, "json_marshal_error", http.StatusInternalServerError)
	}

	// 解析请求体以获取 duration 和 resolution 参数
	var videoRequestMinimax model.VideoRequestMinimax
	if err := json.Unmarshal(modifiedBodyBytes, &videoRequestMinimax); err != nil {
		return openai.ErrorWrapper(err, "invalid_request_error", http.StatusBadRequest)
	}

	// 设置默认 resolution（如果未提供则使用 768P）
	if videoRequestMinimax.Resolution == "" {
		videoRequestMinimax.Resolution = "768P"
	}

	// 将 duration 和 resolution 存储到 context 中供后续计费使用
	c.Set("minimax_duration", videoRequestMinimax.Duration)
	c.Set("minimax_resolution", videoRequestMinimax.Resolution)

	// 请求参数已通过c.Set存储，无需额外日志

	// 重新序列化请求体
	jsonData, err := json.Marshal(videoRequestMinimax)
	if err != nil {
		return openai.ErrorWrapper(err, "json_marshal_error", http.StatusInternalServerError)
	}

	return sendRequestMinimaxAndHandleResponse(c, ctx, fullRequestUrl, jsonData, meta, videoRequest.Model)
}

func handleZhipuVideoRequest(c *gin.Context, ctx context.Context, videoRequest model.VideoRequest, meta *util.RelayMeta) *model.ErrorWithStatusCode {
	baseUrl := meta.BaseURL
	fullRequestUrl := baseUrl + "/api/paas/v4/videos/generations"

	videoRequestZhipu := model.VideoRequestZhipu{
		Model:    videoRequest.Model,
		Prompt:   videoRequest.Prompt,
		ImageURL: videoRequest.ImageURL,
	}

	jsonData, err := json.Marshal(videoRequestZhipu)
	if err != nil {
		return openai.ErrorWrapper(err, "json_marshal_error", http.StatusInternalServerError)
	}

	return sendRequestZhipuAndHandleResponse(c, ctx, fullRequestUrl, jsonData, meta, "cogvideox")
}
func handleRunwayVideoRequest(c *gin.Context, ctx context.Context, videoRequest model.VideoRequest, meta *util.RelayMeta) *model.ErrorWithStatusCode {
	baseUrl := meta.BaseURL
	var fullRequestUrl string
	if meta.ChannelType == 42 {
		fullRequestUrl = baseUrl + "/v1/image_to_video"
	} else {
		fullRequestUrl = baseUrl + "/runwayml/v1/image_to_video"
	}

	// 解析请求体
	var runwayRequest runway.VideoGenerationRequest
	if err := common.UnmarshalBodyReusable(c, &runwayRequest); err != nil {
		return openai.ErrorWrapper(err, "invalid_video_generation_request", http.StatusBadRequest)
	}

	// 设置默认时长
	if runwayRequest.Duration == 0 {
		runwayRequest.Duration = 10
	}

	// 设置 duration 到上下文
	c.Set("duration", strconv.Itoa(runwayRequest.Duration))

	// 序列化请求
	jsonData, err := json.Marshal(runwayRequest)
	if err != nil {
		return openai.ErrorWrapper(err, "failed to marshal request body", http.StatusInternalServerError)
	}

	return sendRequestRunwayAndHandleResponse(c, ctx, fullRequestUrl, jsonData, meta, "gen3a_turbo")
}

func sendRequestMinimaxAndHandleResponse(c *gin.Context, ctx context.Context, fullRequestUrl string, jsonData []byte, meta *util.RelayMeta, modelName string) *model.ErrorWithStatusCode {
	// 预扣费检查 - 预扣0.2，后续处理完多退少补
	quota := int64(0.2 * config.QuotaPerUnit)
	userQuota, err := dbmodel.CacheGetUserQuota(ctx, meta.UserId)
	if err != nil {
		return openai.ErrorWrapper(err, "get_user_quota_error", http.StatusInternalServerError)
	}
	if userQuota-quota < 0 {
		return openai.ErrorWrapper(fmt.Errorf("insufficient user balance"), "User balance is not enough", http.StatusBadRequest)
	}

	channel, err := dbmodel.GetChannelById(meta.ChannelId, true)
	if err != nil {
		return openai.ErrorWrapper(err, "get_channel_error", http.StatusInternalServerError)
	}

	req, err := http.NewRequest("POST", fullRequestUrl, bytes.NewBuffer(jsonData))
	if err != nil {
		return openai.ErrorWrapper(err, "create_request_error", http.StatusInternalServerError)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("authorization", "Bearer "+channel.Key)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return openai.ErrorWrapper(err, "request_error", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return openai.ErrorWrapper(err, "read_response_error", http.StatusInternalServerError)
	}

	var videoResponse model.VideoResponse
	err = json.Unmarshal(body, &videoResponse)
	if err != nil {
		return openai.ErrorWrapper(err, "response_parse_error", http.StatusInternalServerError)
	}

	videoResponse.StatusCode = resp.StatusCode
	return handleMinimaxVideoResponse(c, ctx, videoResponse, body, meta, modelName)

}

func sendRequestZhipuAndHandleResponse(c *gin.Context, ctx context.Context, fullRequestUrl string, jsonData []byte, meta *util.RelayMeta, modelName string) *model.ErrorWithStatusCode {
	// 预扣费检查 - 预扣0.2，后续处理完多退少补
	quota := int64(0.2 * config.QuotaPerUnit)
	userQuota, err := dbmodel.CacheGetUserQuota(ctx, meta.UserId)
	if err != nil {
		return openai.ErrorWrapper(err, "get_user_quota_error", http.StatusInternalServerError)
	}
	if userQuota-quota < 0 {
		return openai.ErrorWrapper(fmt.Errorf("insufficient user balance"), "User balance is not enough", http.StatusBadRequest)
	}

	channel, err := dbmodel.GetChannelById(meta.ChannelId, true)
	if err != nil {
		return openai.ErrorWrapper(err, "get_channel_error", http.StatusInternalServerError)
	}

	req, err := http.NewRequest("POST", fullRequestUrl, bytes.NewBuffer(jsonData))
	if err != nil {
		return openai.ErrorWrapper(err, "create_request_error", http.StatusInternalServerError)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("authorization", "Bearer "+channel.Key)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return openai.ErrorWrapper(err, "request_error", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return openai.ErrorWrapper(err, "read_response_error", http.StatusInternalServerError)
	}

	var videoResponse model.VideoResponse
	err = json.Unmarshal(body, &videoResponse)
	if err != nil {
		return openai.ErrorWrapper(err, "response_parse_error", http.StatusInternalServerError)
	}
	videoResponse.StatusCode = resp.StatusCode
	return handleMZhipuVideoResponse(c, ctx, videoResponse, body, meta, modelName)

}
func sendRequestRunwayAndHandleResponse(c *gin.Context, ctx context.Context, fullRequestUrl string, jsonData []byte, meta *util.RelayMeta, modelName string) *model.ErrorWithStatusCode {
	// 预扣费检查 - 预扣0.2，后续处理完多退少补
	quota := int64(0.2 * config.QuotaPerUnit)
	userQuota, err := dbmodel.CacheGetUserQuota(ctx, meta.UserId)
	if err != nil {
		return openai.ErrorWrapper(err, "get_user_quota_error", http.StatusInternalServerError)
	}
	if userQuota-quota < 0 {
		return openai.ErrorWrapper(fmt.Errorf("insufficient user balance"), "User balance is not enough", http.StatusBadRequest)
	}

	channel, err := dbmodel.GetChannelById(meta.ChannelId, true)
	if err != nil {
		return openai.ErrorWrapper(err, "get_channel_error", http.StatusInternalServerError)
	}

	req, err := http.NewRequest("POST", fullRequestUrl, bytes.NewBuffer(jsonData))
	if err != nil {
		return openai.ErrorWrapper(err, "create_request_error", http.StatusInternalServerError)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Runway-Version", "2024-11-06")
	req.Header.Set("authorization", "Bearer "+channel.Key)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return openai.ErrorWrapper(err, "request_error", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return openai.ErrorWrapper(err, "read_response_error", http.StatusInternalServerError)
	}

	var videoResponse runway.VideoResponse
	err = json.Unmarshal(body, &videoResponse)
	if err != nil {
		log.Printf("Unmarshal error: %v", err)
		return openai.ErrorWrapper(err, "response_parse_error", http.StatusInternalServerError)
	}

	videoResponse.StatusCode = resp.StatusCode
	return handleRunwayVideoResponse(c, ctx, videoResponse, body, meta, modelName)
}

func handleMinimaxVideoResponse(c *gin.Context, ctx context.Context, videoResponse model.VideoResponse, body []byte, meta *util.RelayMeta, modelName string) *model.ErrorWithStatusCode {
	switch videoResponse.BaseResp.StatusCode {
	case 0:
		// 先计算quota
		quota := calculateQuota(meta, modelName, "", "", c)

		// 从 context 中获取 duration 和 resolution
		var durationStr string
		var resolutionStr string
		if minimaxDuration, exists := c.Get("minimax_duration"); exists {
			if durationInt, ok := minimaxDuration.(int); ok {
				durationStr = fmt.Sprintf("%d", durationInt)
			}
		}
		if minimaxResolution, exists := c.Get("minimax_resolution"); exists {
			if resolution, ok := minimaxResolution.(string); ok {
				resolutionStr = resolution
			}
		}

		// 将 resolution 存储到 mode 参数中
		err := CreateVideoLog("minimax", videoResponse.TaskID, meta, resolutionStr, durationStr, "", "", quota, 0, resolutionStr, "")
		if err != nil {

		}
		// 创建 GeneralVideoResponse 结构体
		generalResponse := model.GeneralVideoResponse{
			TaskId:  videoResponse.TaskID,
			Message: videoResponse.BaseResp.StatusMsg,
		}

		switch videoResponse.BaseResp.StatusCode {
		case 0:
			generalResponse.TaskStatus = "succeed"
		default:
			generalResponse.TaskStatus = "failed"
		}
		// 将 GeneralVideoResponse 结构体转换为 JSON
		jsonResponse, err := json.Marshal(generalResponse)
		if err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("Error marshaling response: %s", err),
				"internal_error",
				http.StatusInternalServerError,
			)
		}

		// 发送 JSON 响应给客户端
		c.Data(http.StatusOK, "application/json", jsonResponse)
		return handleSuccessfulResponseWithQuota(c, ctx, meta, modelName, "", "", quota)
	case 1002, 1008:
		return openai.ErrorWrapper(
			fmt.Errorf("API error: %s", videoResponse.BaseResp.StatusMsg),
			"api_error",
			http.StatusTooManyRequests,
		)
	case 1004:
		return openai.ErrorWrapper(
			fmt.Errorf("API error: %s", videoResponse.BaseResp.StatusMsg),
			"api_error",
			http.StatusForbidden,
		)
	case 1013, 1026:
		return openai.ErrorWrapper(
			fmt.Errorf("API error: %s", videoResponse.BaseResp.StatusMsg),
			"api_error",
			http.StatusBadRequest,
		)
	default:
		return openai.ErrorWrapper(
			fmt.Errorf("Unknown API error: %s", videoResponse.BaseResp.StatusMsg),
			"api_error",
			http.StatusInternalServerError,
		)
	}
}

func handleMZhipuVideoResponse(c *gin.Context, ctx context.Context, videoResponse model.VideoResponse, body []byte, meta *util.RelayMeta, modelName string) *model.ErrorWithStatusCode {
	switch videoResponse.StatusCode {
	case 200:
		// 先计算quota
		quota := calculateQuota(meta, modelName, "", "", c)

		err := CreateVideoLog("zhipu", videoResponse.ID, meta, "", "", "", "", quota, 0, "", "")
		if err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("API error: %s", videoResponse.ZhipuError.Message),
				"api_error",
				http.StatusBadRequest,
			)
		}

		// 创建 GeneralVideoResponse 结构体
		generalResponse := model.GeneralVideoResponse{
			TaskId:  videoResponse.ID,
			Message: "",
		}

		// 修改 TaskStatus 处理逻辑
		switch videoResponse.TaskStatus {
		case "FAIL":
			generalResponse.TaskStatus = "failed"
		default:
			generalResponse.TaskStatus = "succeed"
		}

		// 将 GeneralVideoResponse 结构体转换为 JSON
		jsonResponse, err := json.Marshal(generalResponse)
		if err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("error marshaling response: %s", err),
				"internal_error",
				http.StatusInternalServerError,
			)
		}

		// 发送 JSON 响应给客户端
		c.Data(http.StatusOK, "application/json", jsonResponse)

		return handleSuccessfulResponseWithQuota(c, ctx, meta, modelName, "", "", quota)
	case 400:
		return openai.ErrorWrapper(
			fmt.Errorf("API error: %s", videoResponse.ZhipuError.Message),
			"api_error",
			http.StatusBadRequest,
		)
	case 429:
		return openai.ErrorWrapper(
			fmt.Errorf("API error: %s", videoResponse.ZhipuError.Message),
			"api_error",
			http.StatusTooManyRequests,
		)
	default:
		return openai.ErrorWrapper(
			fmt.Errorf("Unknown API error: %s", videoResponse.ZhipuError.Message),
			"api_error",
			http.StatusInternalServerError,
		)
	}
}
func handleRunwayVideoResponse(c *gin.Context, ctx context.Context, videoResponse runway.VideoResponse, body []byte, meta *util.RelayMeta, modelName string) *model.ErrorWithStatusCode {
	switch videoResponse.StatusCode {
	case 200:
		// 先计算quota
		quota := calculateQuota(meta, modelName, "", "", c)

		err := CreateVideoLog("runway", videoResponse.Id, meta, "", "", "", "", quota, 0, "", "")
		if err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("API error: %s", err.Error()),
				"api_error",
				http.StatusBadRequest,
			)
		}

		// 创建 GeneralVideoResponse 结构体
		generalResponse := model.GeneralVideoResponse{
			TaskId:     videoResponse.Id,
			Message:    "",
			TaskStatus: "succeed",
		}

		// 将 GeneralVideoResponse 结构体转换为 JSON
		jsonResponse, err := json.Marshal(generalResponse)
		if err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("error marshaling response: %s", err),
				"internal_error",
				http.StatusInternalServerError,
			)
		}

		// 发送 JSON 响应给客户端
		c.Data(http.StatusOK, "application/json", jsonResponse)

		return handleSuccessfulResponseWithQuota(c, ctx, meta, modelName, "", "", quota)
	case 400:
		return openai.ErrorWrapper(
			fmt.Errorf("API error: %s", videoResponse.Error),
			"api_error",
			http.StatusBadRequest,
		)
	case 429:
		return openai.ErrorWrapper(
			fmt.Errorf("API error: %s", videoResponse.Error),
			"api_error",
			http.StatusTooManyRequests,
		)
	default:
		return openai.ErrorWrapper(
			fmt.Errorf("Unknown API error: %s", videoResponse.Error),
			"api_error",
			http.StatusInternalServerError,
		)
	}
}

// 新增计算quota的函数
func calculateQuota(meta *util.RelayMeta, modelName string, mode string, duration string, c *gin.Context) int64 {
	var modelPrice float64
	defaultPrice, ok := common.DefaultModelPrice[modelName]
	if !ok {
		modelPrice = 0.1
	} else {
		modelPrice = defaultPrice
	}
	quota := int64(modelPrice * config.QuotaPerUnit)

	// 特殊处理 kling-v1 模型
	if modelName == "kling-v1" {
		var multiplier float64
		switch {
		case mode == "std" && duration == "5":
			multiplier = 1
		case mode == "std" && duration == "10":
			multiplier = 2
		case mode == "pro" && duration == "5":
			multiplier = 3.5
		case mode == "pro" && duration == "10":
			multiplier = 7
		default:
			multiplier = 1
		}
		quota = int64(float64(quota) * multiplier)
	}
	if modelName == "kling-v1-5" || modelName == "kling-v1-6" {
		var multiplier float64
		switch {
		case mode == "std" && duration == "5":
			multiplier = 1
		case mode == "std" && duration == "10":
			multiplier = 2
		case mode == "pro" && duration == "5":
			multiplier = 1.75
		case mode == "pro" && duration == "10":
			multiplier = 3.5
		default:
			multiplier = 1
		}
		quota = int64(float64(quota) * multiplier)
	}

	// 特殊处理 MiniMax-Hailuo 视频模型（基于 duration 和 resolution 计费）
	if modelName == "MiniMax-Hailuo-2.3-Fast" || modelName == "MiniMax-Hailuo-2.3" || modelName == "MiniMax-Hailuo-02" {
		// 从 context 中获取 duration 和 resolution
		minimaxDuration, hasDuration := c.Get("minimax_duration")
		minimaxResolution, hasResolution := c.Get("minimax_resolution")

		if hasDuration && hasResolution {
			// 安全的类型断言
			durationInt, ok1 := minimaxDuration.(int)
			resolutionStr, ok2 := minimaxResolution.(string)

			if !ok1 || !ok2 {
				// 类型断言失败，使用默认值
				log.Printf("[计费警告] duration 或 resolution 类型不匹配，使用默认计费")
				return quota
			}

			// 定义价格（人民币）
			var priceCNY float64

			// 根据模型、分辨率和时长设置价格（单位：人民币元）
			switch modelName {
			case "MiniMax-Hailuo-2.3-Fast":
				switch {
				case resolutionStr == "768P" && durationInt == 6:
					priceCNY = 1.35
				case resolutionStr == "768P" && durationInt == 10:
					priceCNY = 2.25
				case resolutionStr == "1080P" && durationInt == 6:
					priceCNY = 2.31
				default:
					// 未匹配到价格表，使用 768P 6秒作为默认
					log.Printf("[计费警告] MiniMax-Hailuo-2.3-Fast 未找到匹配价格: resolution=%s, duration=%d, 使用默认价格1.35元", resolutionStr, durationInt)
					priceCNY = 1.35
				}
			case "MiniMax-Hailuo-2.3":
				switch {
				case resolutionStr == "768P" && durationInt == 6:
					priceCNY = 2.0
				case resolutionStr == "768P" && durationInt == 10:
					priceCNY = 4.0
				case resolutionStr == "1080P" && durationInt == 6:
					priceCNY = 3.5
				default:
					// 未匹配到价格表，使用 768P 6秒作为默认
					log.Printf("[计费警告] MiniMax-Hailuo-2.3 未找到匹配价格: resolution=%s, duration=%d, 使用默认价格2.0元", resolutionStr, durationInt)
					priceCNY = 2.0
				}
			case "MiniMax-Hailuo-02":
				// MiniMax-Hailuo-02 支持多种分辨率
				switch {
				case resolutionStr == "512P" && durationInt == 6:
					priceCNY = 1.5 // 根据官方文档补充
				case resolutionStr == "512P" && durationInt == 10:
					priceCNY = 3.0 // 根据官方文档补充
				case resolutionStr == "768P" && durationInt == 6:
					priceCNY = 2.0
				case resolutionStr == "768P" && durationInt == 10:
					priceCNY = 4.0
				case resolutionStr == "1080P" && durationInt == 6:
					priceCNY = 3.5
				case resolutionStr == "1088P" && durationInt == 6:
					priceCNY = 3.5 // 根据官方文档补充
				default:
					// 未匹配到价格表，使用 768P 6秒作为默认
					log.Printf("[计费警告] MiniMax-Hailuo-02 未找到匹配价格: resolution=%s, duration=%d, 使用默认价格2.0元", resolutionStr, durationInt)
					priceCNY = 2.0
				}
			}

			// 将人民币转换为美元（使用固定汇率 7.2）
			priceUSD := priceCNY / 7.2
			quota = int64(priceUSD * config.QuotaPerUnit)

			// 计费信息已记录到数据库
		}
	}

	value, exists := c.Get("duration")
	if exists {
		runwayDuration := value.(string)
		if runwayDuration == "10" {
			quota = quota * 2
		}
	}

	if modelName == "v3.5" {
		durationInt := c.GetInt("Duration")
		modeStr := c.GetString("Mode")
		motionMode := c.GetString("MotionMode")
		var multiplier float64
		switch {
		case modeStr == "Turbo" && durationInt == 5 && motionMode == "Normal":
			multiplier = 1
		case modeStr == "Turbo" && durationInt == 5 && motionMode == "Performance":
			multiplier = 2
		case modeStr == "Turbo" && durationInt == 8 && motionMode == "Normal":
			multiplier = 2
		case modeStr == "540P" && durationInt == 5 && motionMode == "Normal":
			multiplier = 1
		case modeStr == "540P" && durationInt == 5 && motionMode == "Performance":
			multiplier = 2
		case modeStr == "540P" && durationInt == 8 && motionMode == "Normal":
			multiplier = 2
		case modeStr == "720P" && durationInt == 5 && motionMode == "Normal":
			multiplier = 1.33
		case modeStr == "720P" && durationInt == 5 && motionMode == "Performance":
			multiplier = 2.67
		case modeStr == "720P" && durationInt == 8 && motionMode == "Normal":
			multiplier = 2.67
		case modeStr == "1080P" && durationInt == 5 && motionMode == "Normal":
			multiplier = 2.67
		default:
			multiplier = 1
		}
		quota = int64(float64(45) * multiplier)
	}

	return quota
}

// 新增带quota参数的成功响应处理函数，支持可选的videoTaskId参数
func handleSuccessfulResponseWithQuota(c *gin.Context, ctx context.Context, meta *util.RelayMeta, modelName string, mode string, duration string, quota int64, videoTaskId ...string) *model.ErrorWithStatusCode {
	referer := c.Request.Header.Get("HTTP-Referer")
	title := c.Request.Header.Get("X-Title")

	err := dbmodel.PostConsumeTokenQuota(meta.TokenId, quota)
	if err != nil {
		logger.Error(ctx,
			"error consuming token remain quota: "+err.Error())
	}

	err = dbmodel.CacheUpdateUserQuota(ctx, meta.UserId)
	if err != nil {
		logger.Error(ctx,
			"error update user quota cache: "+err.Error())
	}

	if quota != 0 {
		var modelPrice float64
		defaultPrice, ok := common.DefaultModelPrice[modelName]
		if !ok {
			modelPrice = 0.1
		} else {
			modelPrice = defaultPrice
		}

		tokenName := c.GetString("token_name")
		xRequestID := c.GetString("X-Request-ID")
		logContent := fmt.Sprintf("Model fixed price $%.2f", modelPrice)

		// 如果提供了videoTaskId，使用RecordVideoConsumeLog，否则使用普通的RecordConsumeLogWithRequestID
		if len(videoTaskId) > 0 && videoTaskId[0] != "" {
			dbmodel.RecordVideoConsumeLog(ctx, meta.UserId, meta.ChannelId, 0, 0, modelName, tokenName, quota, logContent, 0, title, referer, videoTaskId[0])
		} else {
			dbmodel.RecordConsumeLogWithRequestID(ctx, meta.UserId, meta.ChannelId, 0, 0, modelName, tokenName, quota, logContent, 0, title, referer, false, 0.0, xRequestID)
		}

		dbmodel.UpdateUserUsedQuotaAndRequestCount(meta.UserId, quota)
		channelId := c.GetInt("channel_id")
		dbmodel.UpdateChannelUsedQuota(channelId, quota)
	}

	return nil
}

// invokeVideoAdaptorRequest 通过 VideoAdaptor 接口处理视频生成请求
func invokeVideoAdaptorRequest(c *gin.Context, ctx context.Context, adaptor relaychannel.VideoAdaptor, videoRequest *model.VideoRequest, meta *util.RelayMeta) *model.ErrorWithStatusCode {
	// 预扣费余额检查
	prePayment := adaptor.GetPrePaymentQuota()
	userQuota, err := dbmodel.CacheGetUserQuota(ctx, meta.UserId)
	if err != nil {
		return openai.ErrorWrapper(err, "get_user_quota_error", http.StatusInternalServerError)
	}
	if userQuota-prePayment < 0 {
		return openai.ErrorWrapper(fmt.Errorf("insufficient user balance"), "User balance is not enough", http.StatusBadRequest)
	}

	adaptor.Init(meta)
	taskResult, apiErr := adaptor.HandleVideoRequest(c, videoRequest, meta)
	if apiErr != nil {
		return apiErr
	}

	// 创建视频任务日志
	_ = CreateVideoLog(adaptor.GetProviderName(), taskResult.TaskId, meta,
		taskResult.Mode, taskResult.Duration, taskResult.VideoType,
		taskResult.VideoId, taskResult.Quota, taskResult.VideoDuration, taskResult.Resolution, taskResult.Sound)

	// 保存 provider 凭证（需在 CreateVideoLog 之后，确保记录已创建）
	if taskResult.Credentials != "" {
		if credErr := dbmodel.UpdateVideoCredentials(taskResult.TaskId, taskResult.Credentials); credErr != nil {
			log.Printf("[VideoAdaptor] Failed to save credentials for task %s: %v", taskResult.TaskId, credErr)
		}
	}

	// 响应客户端
	c.JSON(http.StatusOK, model.GeneralVideoResponse{
		TaskId:        taskResult.TaskId,
		TaskStatus:    taskResult.TaskStatus,
		Message:       taskResult.Message,
		VideoDuration: taskResult.VideoDuration,
	})

	return handleSuccessfulResponseWithQuota(c, ctx, meta,
		meta.ActualModelName, taskResult.Mode, taskResult.Duration,
		taskResult.Quota, taskResult.TaskId)
}

// invokeVideoAdaptorResult 通过 VideoAdaptor 接口查询视频任务结果
func invokeVideoAdaptorResult(c *gin.Context, adaptor relaychannel.VideoAdaptor, videoTask *dbmodel.Video, channel *dbmodel.Channel, cfg *dbmodel.ChannelConfig) *model.ErrorWithStatusCode {
	adaptor.Init(nil)
	result, apiErr := adaptor.HandleVideoResult(c, videoTask, channel, cfg)
	if apiErr != nil {
		return apiErr
	}

	taskId := videoTask.TaskId

	// 更新任务状态，检查是否需要退款
	// 只在失败时传递失败原因，避免将成功/处理中的 Message 写入 fail_reason 字段
	failReason := ""
	if result.TaskStatus == "failed" {
		failReason = result.Message
	}
	needRefund := UpdateVideoTaskStatus(taskId, result.TaskStatus, failReason)
	if needRefund {
		log.Printf("Task %s failed, compensating user", taskId)
		CompensateVideoTask(taskId)
	}

	// 保存视频 URL 到数据库
	if result.VideoResult != "" {
		if err := dbmodel.UpdateVideoStoreUrl(taskId, result.VideoResult); err != nil {
			log.Printf("Failed to update store_url for task %s: %v", taskId, err)
		}
	}

	// 将数据库中存储的 video_duration 填充到响应中
	if videoTask.VideoDuration > 0 {
		result.VideoDuration = videoTask.VideoDuration
	}

	c.JSON(http.StatusOK, result)
	return nil
}

func CreateVideoLog(provider string, taskId string, meta *util.RelayMeta, mode string, duration string, videoType string, videoId string, quota int64, videoDuration float64, resolution string, sound string) error {
	// 对于VertexAI，保存完整的JSON凭证
	var credentialsJSON string
	if provider == "vertexai" {
		// 获取当前使用的凭证
		channel, err := dbmodel.GetChannelById(meta.ChannelId, true)
		if err != nil {
			log.Printf("[VEO任务创建] 获取渠道失败 - 任务:%s, 渠道ID:%d, 错误:%v", taskId, meta.ChannelId, err)
		} else {
			credentials, err := vertexai.GetCredentialsFromConfig(meta.Config, channel)
			if err != nil {
				log.Printf("[VEO任务创建] 获取凭证失败 - 任务:%s, 错误:%v", taskId, err)
			} else {
				if credentialsBytes, err := json.Marshal(credentials); err == nil {
					credentialsJSON = string(credentialsBytes)
					log.Printf("[VEO任务创建] ✅ 成功保存凭证 - 任务:%s, 项目ID:%s, 服务账号:%s",
						taskId, credentials.ProjectID, credentials.ClientEmail)
				} else {
					log.Printf("[VEO任务创建] JSON序列化失败 - 任务:%s, 错误:%v", taskId, err)
				}
			}
		}

		// 如果没有获取到凭证，记录警告
		if credentialsJSON == "" {
			log.Printf("[VEO任务创建] ⚠️  未能保存凭证，查询时将使用当前渠道配置 - 任务:%s", taskId)
		}
	} else if provider == "xai" {
		credentialsJSON = meta.APIKey
	}

	// 根据模型名称确定最终的视频类型
	finalVideoType := videoType
	if videoType == "image-to-video" && strings.Contains(strings.ToLower(meta.OriginModelName), "t2v") {
		finalVideoType = "text-to-video"
	}

	// 处理 resolution 参数
	resolutionStr := resolution

	// 创建新的 Video 实例
	video := &dbmodel.Video{
		Prompt:        "prompt",
		CreatedAt:     time.Now().Unix(), // 使用当前时间戳
		TaskId:        taskId,
		Provider:      provider,
		Username:      dbmodel.GetUsernameById(meta.UserId),
		ChannelId:     meta.ChannelId,
		UserId:        meta.UserId,
		Mode:          mode, //keling
		Type:          finalVideoType,
		Model:         meta.OriginModelName,
		Duration:      duration,
		Resolution:    resolutionStr, // 保存分辨率
		VideoId:       videoId,
		Quota:         quota,
		Credentials:   credentialsJSON, // 保存完整的JSON凭证
		Status:        "processing",    // 初始状态设置为处理中
		VideoDuration: videoDuration,   // 输入视频时长
		Sound:         sound,           // 是否有声
	}

	// 调用 Insert 方法插入记录
	err := video.Insert()
	if err != nil {
		return fmt.Errorf("failed to insert video log: %v", err)
	}

	return nil
}

func mapTaskStatus(status string) string {
	switch status {
	case "PROCESSING":
		return "processing"
	case "SUCCESS":
		return "succeed"
	case "FAIL":
		return "failed"
	default:
		return "unknown"
	}
}

func mapTaskStatusMinimax(status string) string {
	switch status {
	case "Preparing":
		return "processing"
	case "Processing":
		return "processing"
	case "Success":
		return "succeed"
	case "Fail":
		return "failed"
	default:
		log.Printf("[MiniMax状态映射] 未知状态: %s", status)
		return "unknown"
	}
}

func mapTaskStatusRunway(status string) string {
	switch status {
	case "PENDING":
		return "processing"
	case "SUCCEEDED":
		return "succeed"
	default:
		return "unknown"
	}
}

func GetVideoResult(c *gin.Context, taskId string) *model.ErrorWithStatusCode {
	videoTask, err := dbmodel.GetVideoTaskById(taskId)
	if err != nil {
		return openai.ErrorWrapper(
			fmt.Errorf("failed to get video: %v", err),
			"database_error",
			http.StatusInternalServerError,
		)
	}

	channel, err := dbmodel.GetChannelById(videoTask.ChannelId, true)
	if err != nil {
		return openai.ErrorWrapper(
			fmt.Errorf("failed to get channel: %v", err),
			"database_error",
			http.StatusInternalServerError,
		)
	}
	cfg, err := channel.LoadConfig()
	if err != nil {
		return openai.ErrorWrapper(
			fmt.Errorf("failed to load channel config: %v", err),
			"config_error",
			http.StatusInternalServerError,
		)
	}

	// 适配器路由：已迁移供应商由对应 VideoAdaptor 处理
	if adaptor := relayhelper.GetVideoAdaptorByProvider(videoTask.Provider); adaptor != nil {
		return invokeVideoAdaptorResult(c, adaptor, videoTask, channel, &cfg)
	}

	var fullRequestUrl string
	switch videoTask.Provider {
	case "zhipu":
		fullRequestUrl = fmt.Sprintf("https://open.bigmodel.cn/api/paas/v4/async-result/%s", taskId)
	case "minimax":
		fullRequestUrl = fmt.Sprintf("%s/v1/query/video_generation?task_id=%s", *channel.BaseURL, taskId)
	case "runway":
		if channel.Type != 42 {
			fullRequestUrl = fmt.Sprintf("%s/runwayml/v1/tasks/%s", *channel.BaseURL, taskId)
		} else {
			fullRequestUrl = fmt.Sprintf("%s/v1/tasks/%s", *channel.BaseURL, taskId)
		}

	default:
		return openai.ErrorWrapper(
			fmt.Errorf("unsupported model type:"),
			"invalid_request_error",
			http.StatusBadRequest,
		)
	}
	// 创建新的请求
	var req *http.Request

	req, err = http.NewRequest("GET", fullRequestUrl, nil)
	if err != nil {
		return openai.ErrorWrapper(
			fmt.Errorf("failed to create request: %v", err),
			"api_error",
			http.StatusInternalServerError,
		)
	}
	if videoTask.Provider == "runway" && channel.Type == 42 {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Runway-Version", "2024-11-06")
		req.Header.Set("Authorization", "Bearer "+channel.Key)
	} else {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+channel.Key)
	}

	// 发送 HTTP 请求获取结果
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return openai.ErrorWrapper(
			fmt.Errorf("failed to fetch video result: %v", err),
			"api_error",
			http.StatusInternalServerError,
		)
	}
	defer resp.Body.Close()
	// log.Printf("video response body: %+v", resp)
	// 检查响应状态码
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return openai.ErrorWrapper(
			fmt.Errorf("API error: %s", string(body)),
			"api_error",
			resp.StatusCode,
		)
	}

	if videoTask.Provider == "zhipu" {
		// ✅ 修复：defer 必须在 ReadAll 之前
		defer func() {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
		}()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("failed to read response body: %v", err),
				"internal_error",
				http.StatusInternalServerError,
			)
		}

		// 解析JSON响应
		var zhipuResp model.FinalVideoResponse
		if err := json.Unmarshal(body, &zhipuResp); err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("failed to parse response JSON: %v", err),
				"json_parse_error",
				http.StatusInternalServerError,
			)
		}

		// 创建 GeneralVideoResponse 结构体
		generalResponse := model.GeneralFinalVideoResponse{
			TaskId:      taskId,
			TaskStatus:  mapTaskStatus(zhipuResp.TaskStatus), // 使用 mapTaskStatus 函数
			Message:     "",
			VideoResult: "",
			Duration:    videoTask.Duration,
		}

		// 如果任务成功且有视频结果，添加到响应中
		if zhipuResp.TaskStatus == "SUCCESS" && len(zhipuResp.VideoResults) > 0 {
			generalResponse.VideoResult = zhipuResp.VideoResults[0].URL
			// 同时设置 VideoResults
			generalResponse.VideoResults = []model.VideoResultItem{
				{Url: zhipuResp.VideoResults[0].URL},
			}

			// 将视频URL存储到数据库
			if generalResponse.VideoResult != "" {
				err := dbmodel.UpdateVideoStoreUrl(taskId, generalResponse.VideoResult)
				if err != nil {
					log.Printf("Failed to update store_url for task %s: %v", taskId, err)
				}
			}
		}

		// 更新任务状态并检查是否需要退款
		needRefund := UpdateVideoTaskStatus(taskId, generalResponse.TaskStatus, "")
		if needRefund {
			log.Printf("Task %s failed, compensating user", taskId)
			CompensateVideoTask(taskId)
		}

		// 将 GeneralVideoResponse 结构体转换为 JSON
		jsonResponse, err := json.Marshal(generalResponse)
		if err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("Error marshaling response: %s", err),
				"internal_error",
				http.StatusInternalServerError,
			)
		}

		// 直接使用上游返回的状态码
		c.Data(resp.StatusCode, "application/json", jsonResponse)
		return nil
	} else if videoTask.Provider == "minimax" {
		err := handleMinimaxResponse(c, channel, taskId)
		if err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("Error handling minimax response: %v", err),
				"internal_error",
				http.StatusInternalServerError,
			)
		}
	} else if videoTask.Provider == "runway" {
		// ✅ defer 位置正确（在 ReadAll 之前）
		defer func() {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
		}()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("failed to read response body: %v", err),
				"internal_error",
				http.StatusInternalServerError,
			)
		}

		// 解析JSON响应
		var runwayResp runway.VideoFinalResponse
		if err := json.Unmarshal(body, &runwayResp); err != nil {
			log.Printf("Failed to parse response: %v, body: %s", err, string(body))
			return openai.ErrorWrapper(
				fmt.Errorf("failed to parse response JSON: %v", err),
				"json_parse_error",
				http.StatusInternalServerError,
			)
		}

		// 创建 GeneralVideoResponse 结构体
		generalResponse := model.GeneralFinalVideoResponse{
			TaskId:      taskId,
			TaskStatus:  mapTaskStatusRunway(runwayResp.Status),
			Message:     "", // 添加错误信息
			VideoResult: "",
			Duration:    videoTask.Duration,
		}

		// 如果任务成功且有视频结果，添加到响应中
		if runwayResp.Status == "SUCCEEDED" && len(runwayResp.Output) > 0 {
			generalResponse.VideoResult = runwayResp.Output[0]
			// 同时设置 VideoResults
			generalResponse.VideoResults = []model.VideoResultItem{
				{Url: runwayResp.Output[0]},
			}

			// 将视频URL存储到数据库
			if generalResponse.VideoResult != "" {
				err := dbmodel.UpdateVideoStoreUrl(taskId, generalResponse.VideoResult)
				if err != nil {
					log.Printf("Failed to update store_url for task %s: %v", taskId, err)
				}
			}
		} else {
			log.Printf("Task not succeeded or no output. Status: %s, Output length: %d",
				runwayResp.Status, len(runwayResp.Output))
		}

		// 更新任务状态并检查是否需要退款
		failReason := ""
		if runwayResp.Status == "FAILED" {
			failReason = "Task failed"
		}
		needRefund := UpdateVideoTaskStatus(taskId, generalResponse.TaskStatus, failReason)
		if needRefund {
			log.Printf("Task %s failed, compensating user", taskId)
			CompensateVideoTask(taskId)
		}

		// 将 GeneralVideoResponse 结构体转换为 JSON
		jsonResponse, err := json.Marshal(generalResponse)
		if err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("error marshaling response: %s", err),
				"internal_error",
				http.StatusInternalServerError,
			)
		}

		// 直接使用上游返回的状态码
		c.Data(resp.StatusCode, "application/json", jsonResponse)
		return nil
	} else if videoTask.Provider == "ali" {
		defer resp.Body.Close()

		// 首先检查数据库中是否已有存储的URL
		if videoTask.StoreUrl != "" {
			log.Printf("Found existing store URL for Ali task %s: %s", taskId, videoTask.StoreUrl)

			// 解析StoreUrl，可能是JSON数组格式或单个URL
			var videoUrls []string
			if err := json.Unmarshal([]byte(videoTask.StoreUrl), &videoUrls); err != nil {
				// 如果不是JSON数组，就当作单个URL处理
				videoUrls = []string{videoTask.StoreUrl}
			}

			// 构建VideoResults
			videoResults := make([]model.VideoResultItem, len(videoUrls))
			for i, url := range videoUrls {
				videoResults[i] = model.VideoResultItem{Url: url}
			}

			generalResponse := model.GeneralFinalVideoResponse{
				TaskId:       taskId,
				VideoResult:  videoUrls[0], // 第一个URL作为主URL
				VideoId:      taskId,
				TaskStatus:   "succeed",
				Message:      "Video retrieved from cache",
				VideoResults: videoResults,
				Duration:     videoTask.Duration,
			}
			jsonResponse, err := json.Marshal(generalResponse)
			if err != nil {
				return openai.ErrorWrapper(fmt.Errorf("error marshaling response: %s", err), "internal_error", http.StatusInternalServerError)
			}
			c.Data(http.StatusOK, "application/json", jsonResponse)
			return nil
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("failed to read response body: %v", err),
				"internal_error",
				http.StatusInternalServerError,
			)
		}

		// 打印完整的阿里云响应体
		log.Printf("Ali video query response body: %s", string(body))

		// 解析JSON响应
		var aliResp ali.AliVideoQueryResponse
		if err := json.Unmarshal(body, &aliResp); err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("failed to parse response JSON: %v", err),
				"json_parse_error",
				http.StatusInternalServerError,
			)
		}

		// 创建 GeneralVideoResponse 结构体
		generalResponse := model.GeneralFinalVideoResponse{
			TaskId:      taskId,
			VideoId:     taskId,
			TaskStatus:  "processing", // 默认状态
			Message:     "",
			VideoResult: "",
			Duration:    videoTask.Duration,
		}

		// 处理响应
		if aliResp.Code != "" {
			// 查询API本身出错 - 直接返回阿里云的错误信息
			generalResponse.TaskStatus = "failed"
			if aliResp.Message != "" {
				generalResponse.Message = fmt.Sprintf("Aliyun API error: [%s] %s (request_id: %s)", aliResp.Code, aliResp.Message, aliResp.RequestID)
			} else {
				generalResponse.Message = fmt.Sprintf("Aliyun API error: [%s] (request_id: %s)", aliResp.Code, aliResp.RequestID)
			}
		} else if aliResp.Output != nil {
			// 根据任务状态处理
			switch aliResp.Output.TaskStatus {
			case "SUCCEEDED":
				generalResponse.TaskStatus = "succeed"
				generalResponse.Message = fmt.Sprintf("Video generation completed, request_id: %s", aliResp.RequestID)
				if aliResp.Output.VideoURL != "" {
					// 保存URL到数据库
					if updateErr := dbmodel.UpdateVideoStoreUrl(taskId, aliResp.Output.VideoURL); updateErr != nil {
						log.Printf("Failed to save Ali video URL for task %s: %v", taskId, updateErr)
					} else {
						log.Printf("Successfully saved Ali video URL for task %s: %s", taskId, aliResp.Output.VideoURL)
					}

					generalResponse.VideoResult = aliResp.Output.VideoURL
					generalResponse.VideoResults = []model.VideoResultItem{
						{Url: aliResp.Output.VideoURL},
					}
				}
			case "FAILED":
				generalResponse.TaskStatus = "failed"
				// 优先使用阿里云返回的详细错误信息（错误信息在output对象内部）
				if aliResp.Output.Code != "" && aliResp.Output.Message != "" {
					generalResponse.Message = fmt.Sprintf("video generation failed: [%s] %s (request_id: %s)", aliResp.Output.Code, aliResp.Output.Message, aliResp.RequestID)
				} else if aliResp.Output.Message != "" {
					generalResponse.Message = fmt.Sprintf("video generation failed: %s (request_id: %s)", aliResp.Output.Message, aliResp.RequestID)
				} else if aliResp.Code != "" && aliResp.Message != "" {
					// 兼容顶层错误信息
					generalResponse.Message = fmt.Sprintf("video generation failed: [%s] %s (request_id: %s)", aliResp.Code, aliResp.Message, aliResp.RequestID)
				} else if aliResp.Message != "" {
					generalResponse.Message = fmt.Sprintf("video generation failed: %s (request_id: %s)", aliResp.Message, aliResp.RequestID)
				} else {
					generalResponse.Message = fmt.Sprintf("video generation failed (request_id: %s)", aliResp.RequestID)
				}
			case "UNKNOWN":
				generalResponse.TaskStatus = "failed"
				// 优先使用output内的错误信息
				if aliResp.Output.Code != "" && aliResp.Output.Message != "" {
					generalResponse.Message = fmt.Sprintf("task expired or unknown: [%s] %s (request_id: %s)", aliResp.Output.Code, aliResp.Output.Message, aliResp.RequestID)
				} else if aliResp.Output.Message != "" {
					generalResponse.Message = fmt.Sprintf("task expired or unknown: %s (request_id: %s)", aliResp.Output.Message, aliResp.RequestID)
				} else if aliResp.Message != "" {
					generalResponse.Message = fmt.Sprintf("task expired or unknown: %s (request_id: %s)", aliResp.Message, aliResp.RequestID)
				} else {
					generalResponse.Message = fmt.Sprintf("task expired or unknown (request_id: %s)", aliResp.RequestID)
				}
			case "PROCESSING", "RUNNING":
				generalResponse.TaskStatus = "processing"
				generalResponse.Message = fmt.Sprintf("Video generation in progress, request_id: %s", aliResp.RequestID)
			default:
				generalResponse.TaskStatus = "processing"
				generalResponse.Message = fmt.Sprintf("Video generation in progress (status: %s), request_id: %s", aliResp.Output.TaskStatus, aliResp.RequestID)
			}
		} else {
			// 无输出，可能是API错误
			generalResponse.TaskStatus = "failed"
			if aliResp.Message != "" {
				generalResponse.Message = fmt.Sprintf("no response data received: %s (request_id: %s)", aliResp.Message, aliResp.RequestID)
			} else {
				generalResponse.Message = fmt.Sprintf("no response data received (request_id: %s)", aliResp.RequestID)
			}
		}

		// 更新数据库任务状态并在必要时处理退款
		failReason := ""
		if generalResponse.TaskStatus == "failed" {
			failReason = generalResponse.Message // 包含request_id的完整错误信息
		}
		needRefund := UpdateVideoTaskStatus(taskId, generalResponse.TaskStatus, failReason)
		if needRefund {
			log.Printf("Ali task %s failed, compensating user", taskId)
			CompensateVideoTask(taskId)
		}

		// 将 GeneralVideoResponse 结构体转换为 JSON
		jsonResponse, err := json.Marshal(generalResponse)
		if err != nil {
			return openai.ErrorWrapper(
				fmt.Errorf("error marshaling response: %s", err),
				"internal_error",
				http.StatusInternalServerError,
			)
		}

		// 返回响应
		c.Data(http.StatusOK, "application/json", jsonResponse)
		return nil
	}
	return nil
}

func handleMinimaxResponse(c *gin.Context, channel *dbmodel.Channel, taskId string) *model.ErrorWithStatusCode {
	// 查询数据库中的任务信息以获取Duration等字段
	videoTask, err := dbmodel.GetVideoTaskById(taskId)
	if err != nil {
		log.Printf("Failed to get video task for minimax: %v", err)
		// 继续处理，但duration将为空
	}

	// 第一次请求，获取初始状态
	url := fmt.Sprintf("%s/v1/query/video_generation?task_id=%s", *channel.BaseURL, taskId)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return openai.ErrorWrapper(fmt.Errorf("failed to create request: %v", err), "api_error", http.StatusInternalServerError)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+channel.Key)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return openai.ErrorWrapper(fmt.Errorf("failed to send request: %v", err), "api_error", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return openai.ErrorWrapper(fmt.Errorf("failed to read response body: %v", err), "internal_error", http.StatusInternalServerError)
	}

	// 打印海螺原始响应体
	log.Printf("[MiniMax原始响应] TaskId:%s, StatusCode:%d, Body:%s", taskId, resp.StatusCode, string(body))

	var minimaxResp model.FinalVideoResponse
	if err := json.Unmarshal(body, &minimaxResp); err != nil {
		return openai.ErrorWrapper(fmt.Errorf("failed to parse response JSON: %v", err), "json_parse_error", http.StatusInternalServerError)
	}

	duration := ""
	if videoTask != nil {
		duration = videoTask.Duration
	}

	generalResponse := model.GeneralFinalVideoResponse{
		TaskId:      taskId,
		TaskStatus:  mapTaskStatusMinimax(minimaxResp.Status),
		Message:     minimaxResp.BaseResp.StatusMsg,
		VideoResult: "",
		Duration:    duration,
	}

	// 如果 FileID 为空，直接返回当前状态
	if minimaxResp.FileID == "" {
		// 更新任务状态并检查是否需要退款
		failReason := ""
		if generalResponse.TaskStatus == "failed" {
			failReason = generalResponse.Message
			if failReason == "" {
				failReason = "Task failed"
			}
		}
		needRefund := UpdateVideoTaskStatus(taskId, generalResponse.TaskStatus, failReason)
		if needRefund {
			log.Printf("Task %s failed, compensating user", taskId)
			CompensateVideoTask(taskId)
		}

		jsonResponse, err := json.Marshal(generalResponse)
		if err != nil {
			return openai.ErrorWrapper(fmt.Errorf("Error marshaling response: %s", err), "internal_error", http.StatusInternalServerError)
		}
		c.Data(resp.StatusCode, "application/json", jsonResponse)
		return nil
	}

	// 如果 FileID 不为空，获取文件信息
	fileUrl := fmt.Sprintf("%s/v1/files/retrieve?file_id=%s", *channel.BaseURL, minimaxResp.FileID)
	fileReq, err := http.NewRequest("GET", fileUrl, nil)
	if err != nil {
		return openai.ErrorWrapper(fmt.Errorf("failed to create file request: %v", err), "api_error", http.StatusInternalServerError)
	}
	fileReq.Header.Set("Content-Type", "application/json")
	fileReq.Header.Set("Authorization", "Bearer "+channel.Key)

	fileResp, err := client.Do(fileReq)
	if err != nil {
		return openai.ErrorWrapper(fmt.Errorf("failed to send file request: %v", err), "api_error", http.StatusInternalServerError)
	}
	defer fileResp.Body.Close()

	fileBody, err := io.ReadAll(fileResp.Body)
	if err != nil {
		return openai.ErrorWrapper(fmt.Errorf("failed to read file response body: %v", err), "internal_error", http.StatusInternalServerError)
	}

	// 打印海螺文件信息原始响应体
	log.Printf("[MiniMax文件响应] TaskId:%s, FileID:%s, StatusCode:%d, Body:%s", taskId, minimaxResp.FileID, fileResp.StatusCode, string(fileBody))

	var fileResponse model.MinimaxFinalResponse
	if err := json.Unmarshal(fileBody, &fileResponse); err != nil {
		return openai.ErrorWrapper(fmt.Errorf("failed to parse file response JSON: %v", err), "json_parse_error", http.StatusInternalServerError)
	}

	generalResponse.VideoResult = fileResponse.File.DownloadURL
	// 同时设置 VideoResults
	generalResponse.VideoResults = []model.VideoResultItem{
		{Url: fileResponse.File.DownloadURL},
	}
	generalResponse.TaskStatus = "succeed" // 假设有 FileID 且能获取到下载 URL 就意味着成功

	// 将视频URL存储到数据库的StoreUrl字段
	if fileResponse.File.DownloadURL != "" {
		err := dbmodel.UpdateVideoStoreUrl(taskId, fileResponse.File.DownloadURL)
		if err != nil {
			log.Printf("Failed to update store_url for task %s: %v", taskId, err)
		}
	}

	// 更新任务状态并检查是否需要退款
	failReason := ""
	if generalResponse.TaskStatus == "failed" {
		failReason = generalResponse.Message
		if failReason == "" {
			failReason = "Task failed"
		}
	}
	needRefund := UpdateVideoTaskStatus(taskId, generalResponse.TaskStatus, failReason)
	if needRefund {
		log.Printf("Task %s failed, compensating user", taskId)
		CompensateVideoTask(taskId)
	}

	jsonResponse, err := json.Marshal(generalResponse)
	if err != nil {
		return openai.ErrorWrapper(fmt.Errorf("Error marshaling response: %s", err), "internal_error", http.StatusInternalServerError)
	}

	c.Data(fileResp.StatusCode, "application/json", jsonResponse)
	return nil
}

func UpdateVideoTaskStatus(taskid string, status string, failreason string) bool {
	videoTask, err := dbmodel.GetVideoTaskById(taskid)
	if err != nil {
		log.Printf("Failed to get video task for update: %v", err)
		return false
	}

	// 记录原始状态
	oldStatus := videoTask.Status

	// 检查状态是否真的发生了变化
	if oldStatus == status {
		log.Printf("Task %s status unchanged: %s", taskid, status)
		return false
	}

	// 更新字段
	videoTask.Status = status
	if failreason != "" {
		videoTask.FailReason = failreason
	}

	// 计算总耗时（秒）
	videoTask.TotalDuration = time.Now().Unix() - videoTask.CreatedAt

	// 尝试更新数据库
	err = videoTask.Update()
	if err != nil {
		log.Printf("Failed to update video task %s using model method: %v", taskid, err)

		// 如果Update失败，尝试直接使用SQL更新作为回退方案
		log.Printf("Attempting direct SQL update for task %s", taskid)
		updateFields := map[string]interface{}{
			"status":         status,
			"total_duration": time.Now().Unix() - videoTask.CreatedAt,
		}
		if failreason != "" {
			updateFields["fail_reason"] = failreason
		}

		result := dbmodel.DB.Model(&dbmodel.Video{}).
			Where("task_id = ?", taskid).
			Updates(updateFields)

		if result.Error != nil {
			log.Printf("Direct SQL update also failed for task %s: %v", taskid, result.Error)
			return false
		}

		if result.RowsAffected == 0 {
			log.Printf("No rows affected for task %s update - record may not exist", taskid)
			return false
		}

		log.Printf("Direct SQL update successful for task %s, affected rows: %d", taskid, result.RowsAffected)
	} else {
		log.Printf("Model update successful for task %s", taskid)
	}

	log.Printf("Task %s status updated from '%s' to '%s'", taskid, oldStatus, status)

	// 返回是否需要退款：只有当状态变为失败且之前不是失败状态时才退款
	// 空字符串被视为非失败状态，这是正确的，因为任务刚创建时就是这个状态
	needRefund := (oldStatus != "failed" && status == "failed")
	log.Printf("Task %s refund decision: oldStatus='%s', newStatus='%s', needRefund=%v", taskid, oldStatus, status, needRefund)

	return needRefund
}

func CompensateVideoTask(taskid string) {
	videoTask, err := dbmodel.GetVideoTaskById(taskid)
	if err != nil {
		log.Printf("Failed to get video task for compensation: %v", err)
		return
	}
	quota := videoTask.Quota
	log.Printf("Compensating user %d for failed task %s with quota %d", videoTask.UserId, taskid, quota)

	// 1. 补偿用户配额（增加余额、减少已使用配额和请求次数）
	err = dbmodel.CompensateVideoTaskQuota(videoTask.UserId, quota)
	if err != nil {
		log.Printf("Failed to compensate user quota for task %s: %v", taskid, err)
		return
	}
	log.Printf("Successfully compensated user %d quota for task %s", videoTask.UserId, taskid)

	// 2. 补偿渠道配额（减少渠道已使用配额）
	err = dbmodel.CompensateChannelQuota(videoTask.ChannelId, quota)
	if err != nil {
		log.Printf("Failed to compensate channel quota for task %s: %v", taskid, err)
	} else {
		log.Printf("Successfully compensated channel %d quota for task %s", videoTask.ChannelId, taskid)
	}

	log.Printf("Successfully completed compensation for task %s: user %d and channel %d restored quota %d", taskid, videoTask.UserId, videoTask.ChannelId, quota)
}
