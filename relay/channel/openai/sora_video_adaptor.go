package openai

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	dbmodel "github.com/songquanpeng/one-api/model"
	relaychannel "github.com/songquanpeng/one-api/relay/channel"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

// VideoAdaptor implements the VideoAdaptor interface for Sora (OpenAI).
type VideoAdaptor struct {
	relaychannel.BaseVideoAdaptor
}

func (a *VideoAdaptor) GetProviderName() string { return "sora" }
func (a *VideoAdaptor) GetChannelName() string  { return "OpenAI Sora" }
func (a *VideoAdaptor) GetSupportedModels() []string {
	return []string{"sora-2", "sora-2-pro", "sora-2-remix", "sora-2-pro-remix"}
}

// GetPrePaymentQuota returns the pre-payment quota: 4s * $0.10/s = $0.40 USD.
func (a *VideoAdaptor) GetPrePaymentQuota() int64 {
	return int64(0.4 * config.QuotaPerUnit)
}

func (a *VideoAdaptor) HandleVideoRequest(c *gin.Context, req *model.VideoRequest, meta *util.RelayMeta) (*relaychannel.VideoTaskResult, *model.ErrorWithStatusCode) {
	modelName := meta.ActualModelName

	// Remix path
	if strings.Contains(modelName, "remix") {
		return a.handleRemixRequest(c, meta)
	}

	// Form-data vs JSON path
	contentType := c.GetHeader("Content-Type")
	if strings.Contains(contentType, "multipart/form-data") {
		return a.handleFormDataRequest(c, meta)
	}
	return a.handleJSONRequest(c, meta)
}

// handleFormDataRequest handles native multipart form-data passthrough.
func (a *VideoAdaptor) handleFormDataRequest(c *gin.Context, meta *util.RelayMeta) (*relaychannel.VideoTaskResult, *model.ErrorWithStatusCode) {
	if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
		return nil, ErrorWrapper(err, "parse_multipart_form_failed", http.StatusBadRequest)
	}

	modelName := c.Request.FormValue("model")
	if modelName == "" {
		modelName = meta.ActualModelName
	}
	secondsStr := c.Request.FormValue("seconds")
	if secondsStr == "" {
		secondsStr = "4"
	}
	size := c.Request.FormValue("size")
	if size == "" {
		size = "720x1280"
	}

	log.Printf("sora-video-request (form-data): model=%s, seconds=%s, size=%s", modelName, secondsStr, size)

	return a.sendFormDataRequest(c, meta, modelName, secondsStr, size)
}

// handleJSONRequest handles JSON requests by converting to multipart form-data.
func (a *VideoAdaptor) handleJSONRequest(c *gin.Context, meta *util.RelayMeta) (*relaychannel.VideoTaskResult, *model.ErrorWithStatusCode) {
	var soraReq SoraVideoRequest
	if err := common.UnmarshalBodyReusable(c, &soraReq); err != nil {
		return nil, ErrorWrapper(err, "parse_json_request_failed", http.StatusBadRequest)
	}

	if soraReq.Model == "" {
		soraReq.Model = meta.ActualModelName
	}
	if soraReq.Seconds == "" {
		soraReq.Seconds = "4"
	}
	if soraReq.Size == "" {
		soraReq.Size = "720x1280"
	}

	log.Printf("sora-video-request (JSON): model=%s, seconds=%s, size=%s, has_input_reference=%v",
		soraReq.Model, soraReq.Seconds, soraReq.Size, soraReq.InputReference != "")

	return a.sendJSONRequest(c, meta, &soraReq)
}

// handleRemixRequest handles Sora Remix requests.
func (a *VideoAdaptor) handleRemixRequest(c *gin.Context, meta *util.RelayMeta) (*relaychannel.VideoTaskResult, *model.ErrorWithStatusCode) {
	var remixReq SoraRemixRequest
	if err := common.UnmarshalBodyReusable(c, &remixReq); err != nil {
		return nil, ErrorWrapper(err, "parse_remix_request_failed", http.StatusBadRequest)
	}

	log.Printf("sora-remix-request: model=%s, video_id=%s, prompt=%s", remixReq.Model, remixReq.VideoID, remixReq.Prompt)

	// Look up the original video task to get the original channel info.
	videoTask, err := dbmodel.GetVideoTaskByVideoId(remixReq.VideoID)
	if err != nil {
		return nil, ErrorWrapper(
			fmt.Errorf("video_id not found: %s", remixReq.VideoID),
			"video_not_found",
			http.StatusNotFound,
		)
	}

	originalChannel, err := dbmodel.GetChannelById(videoTask.ChannelId, true)
	if err != nil {
		return nil, ErrorWrapper(err, "get_original_channel_error", http.StatusInternalServerError)
	}

	log.Printf("sora-remix: using original channel_id=%d, channel_name=%s", videoTask.ChannelId, originalChannel.Name)

	baseUrl := *originalChannel.BaseURL
	if baseUrl == "" {
		baseUrl = "https://api.openai.com"
	}

	var fullRequestUrl string
	if originalChannel.Type == common.ChannelTypeAzure {
		fullRequestUrl = fmt.Sprintf("%s/openai/v1/videos/%s/remix", baseUrl, remixReq.VideoID)
	} else {
		fullRequestUrl = fmt.Sprintf("%s/v1/videos/%s/remix", baseUrl, remixReq.VideoID)
	}

	requestBody := map[string]string{"prompt": remixReq.Prompt}
	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, ErrorWrapper(err, "marshal_request_failed", http.StatusInternalServerError)
	}

	log.Printf("sora-remix: sending to OpenAI - URL: %s, body: %s (model param removed)", fullRequestUrl, string(jsonData))

	req, err := http.NewRequest("POST", fullRequestUrl, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, ErrorWrapper(err, "create_request_error", http.StatusInternalServerError)
	}

	req.Header.Set("Content-Type", "application/json")
	if originalChannel.Type == common.ChannelTypeAzure {
		req.Header.Set("Api-key", originalChannel.Key)
	} else {
		req.Header.Set("Authorization", "Bearer "+originalChannel.Key)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, ErrorWrapper(err, "request_error", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, ErrorWrapper(err, "read_response_error", http.StatusInternalServerError)
	}

	if config.DebugEnabled {
		log.Printf("[DEBUG] Sora remix response: status=%d, body=%s", resp.StatusCode, string(respBody))
	}

	var soraResponse SoraVideoResponse
	if err := json.Unmarshal(respBody, &soraResponse); err != nil {
		return nil, ErrorWrapper(err, "parse_remix_response_failed", http.StatusInternalServerError)
	}
	soraResponse.StatusCode = resp.StatusCode

	// Extract params for billing from response.
	modelName := soraResponse.Model
	if modelName == "" {
		modelName = "sora-2"
	}
	secondsStr := soraResponse.Seconds
	if secondsStr == "" {
		secondsStr = "4"
	}
	size := soraResponse.Size
	if size == "" {
		size = "720x1280"
	}

	quota := calcSoraQuota(modelName, secondsStr, size)

	// Check user balance.
	userQuota, err := dbmodel.CacheGetUserQuota(c.Request.Context(), meta.UserId)
	if err != nil {
		return nil, ErrorWrapper(err, "get_user_quota_error", http.StatusInternalServerError)
	}
	if userQuota-quota < 0 {
		return nil, ErrorWrapper(fmt.Errorf("insufficient user balance"), "User balance is not enough", http.StatusBadRequest)
	}

	if soraResponse.Error != nil {
		return nil, &model.ErrorWithStatusCode{
			Error: model.Error{
				Message: soraResponse.Error.Message,
				Type:    soraResponse.Error.Type,
				Code:    soraResponse.Error.Code,
			},
			StatusCode: soraResponse.StatusCode,
		}
	}
	if soraResponse.StatusCode != 200 {
		errMsg := fmt.Sprintf("Request failed with status code: %d", soraResponse.StatusCode)
		return nil, &model.ErrorWithStatusCode{
			Error: model.Error{
				Message: errMsg,
				Type:    "api_error",
			},
			StatusCode: soraResponse.StatusCode,
		}
	}

	return &relaychannel.VideoTaskResult{
		TaskId:     soraResponse.ID,
		TaskStatus: "succeed",
		Message:    fmt.Sprintf("Video remix request submitted successfully, task_id: %s, remixed_from: %s", soraResponse.ID, remixReq.VideoID),
		Mode:       size,
		Duration:   secondsStr,
		VideoType:  "remix",
		VideoId:    remixReq.VideoID,
		Quota:      quota,
	}, nil
}

// sendFormDataRequest rebuilds and sends a multipart form-data request to the Sora API.
func (a *VideoAdaptor) sendFormDataRequest(c *gin.Context, meta *util.RelayMeta, modelName string, secondsStr string, size string) (*relaychannel.VideoTaskResult, *model.ErrorWithStatusCode) {
	channel, err := dbmodel.GetChannelById(meta.ChannelId, true)
	if err != nil {
		return nil, ErrorWrapper(err, "get_channel_error", http.StatusInternalServerError)
	}

	quota := calcSoraQuota(modelName, secondsStr, size)

	userQuota, err := dbmodel.CacheGetUserQuota(c.Request.Context(), meta.UserId)
	if err != nil {
		return nil, ErrorWrapper(err, "get_user_quota_error", http.StatusInternalServerError)
	}
	if userQuota-quota < 0 {
		return nil, ErrorWrapper(fmt.Errorf("insufficient user balance"), "User balance is not enough", http.StatusBadRequest)
	}

	baseUrl := meta.BaseURL
	var fullRequestUrl string
	if meta.ChannelType == common.ChannelTypeAzure {
		fullRequestUrl = fmt.Sprintf("%s/openai/v1/videos", baseUrl)
	} else {
		fullRequestUrl = fmt.Sprintf("%s/v1/videos", baseUrl)
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	for key, values := range c.Request.PostForm {
		for _, value := range values {
			writer.WriteField(key, value)
		}
	}

	if c.Request.MultipartForm != nil && c.Request.MultipartForm.File != nil {
		for fieldName, files := range c.Request.MultipartForm.File {
			for _, fileHeader := range files {
				file, err := fileHeader.Open()
				if err != nil {
					return nil, ErrorWrapper(err, "open_uploaded_file_failed", http.StatusBadRequest)
				}
				defer file.Close()

				part, err := writer.CreateFormFile(fieldName, fileHeader.Filename)
				if err != nil {
					return nil, ErrorWrapper(err, "create_form_file_failed", http.StatusInternalServerError)
				}
				io.Copy(part, file)
			}
		}
	}
	writer.Close()

	req, err := http.NewRequest("POST", fullRequestUrl, body)
	if err != nil {
		return nil, ErrorWrapper(err, "create_request_error", http.StatusInternalServerError)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	if meta.ChannelType == common.ChannelTypeAzure {
		req.Header.Set("Api-key", channel.Key)
	} else {
		req.Header.Set("Authorization", "Bearer "+channel.Key)
	}

	httpClient := &http.Client{}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, ErrorWrapper(err, "request_error", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, ErrorWrapper(err, "read_response_error", http.StatusInternalServerError)
	}

	if config.DebugEnabled {
		log.Printf("[DEBUG] Sora video response (form-data): status=%d, body=%s", resp.StatusCode, string(respBody))
	}

	var soraResponse SoraVideoResponse
	if err := json.Unmarshal(respBody, &soraResponse); err != nil {
		return nil, ErrorWrapper(err, "parse_sora_video_response_failed", http.StatusInternalServerError)
	}
	soraResponse.StatusCode = resp.StatusCode

	return parseSoraVideoResponse(soraResponse, respBody, modelName, quota, secondsStr, size)
}

// sendJSONRequest converts a JSON request to multipart form-data and sends it.
func (a *VideoAdaptor) sendJSONRequest(c *gin.Context, meta *util.RelayMeta, soraReq *SoraVideoRequest) (*relaychannel.VideoTaskResult, *model.ErrorWithStatusCode) {
	channel, err := dbmodel.GetChannelById(meta.ChannelId, true)
	if err != nil {
		return nil, ErrorWrapper(err, "get_channel_error", http.StatusInternalServerError)
	}

	quota := calcSoraQuota(soraReq.Model, soraReq.Seconds, soraReq.Size)

	userQuota, err := dbmodel.CacheGetUserQuota(c.Request.Context(), meta.UserId)
	if err != nil {
		return nil, ErrorWrapper(err, "get_user_quota_error", http.StatusInternalServerError)
	}
	if userQuota-quota < 0 {
		return nil, ErrorWrapper(fmt.Errorf("insufficient user balance"), "User balance is not enough", http.StatusBadRequest)
	}

	baseUrl := meta.BaseURL
	var fullRequestUrl string
	if meta.ChannelType == common.ChannelTypeAzure {
		fullRequestUrl = fmt.Sprintf("%s/openai/v1/videos", baseUrl)
	} else {
		fullRequestUrl = fmt.Sprintf("%s/v1/videos", baseUrl)
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	writer.WriteField("model", soraReq.Model)
	writer.WriteField("prompt", soraReq.Prompt)
	if soraReq.Size != "" {
		writer.WriteField("size", soraReq.Size)
	}
	if soraReq.Seconds != "" {
		writer.WriteField("seconds", soraReq.Seconds)
	}
	if soraReq.AspectRatio != "" {
		writer.WriteField("aspect_ratio", soraReq.AspectRatio)
	}
	if soraReq.Loop {
		writer.WriteField("loop", "true")
	}

	if soraReq.InputReference != "" {
		if err := soraHandleInputReference(writer, soraReq.InputReference); err != nil {
			return nil, ErrorWrapper(err, "handle_input_reference_failed", http.StatusBadRequest)
		}
	}

	writer.Close()

	req, err := http.NewRequest("POST", fullRequestUrl, body)
	if err != nil {
		return nil, ErrorWrapper(err, "create_request_error", http.StatusInternalServerError)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	if meta.ChannelType == common.ChannelTypeAzure {
		req.Header.Set("Api-key", channel.Key)
	} else {
		req.Header.Set("Authorization", "Bearer "+channel.Key)
	}

	httpClient := &http.Client{}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, ErrorWrapper(err, "request_error", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, ErrorWrapper(err, "read_response_error", http.StatusInternalServerError)
	}

	if config.DebugEnabled {
		log.Printf("[DEBUG] Sora video response (JSON->form): status=%d, body=%s", resp.StatusCode, string(respBody))
	}

	var soraResponse SoraVideoResponse
	if err := json.Unmarshal(respBody, &soraResponse); err != nil {
		return nil, ErrorWrapper(err, "parse_sora_video_response_failed", http.StatusInternalServerError)
	}
	soraResponse.StatusCode = resp.StatusCode

	return parseSoraVideoResponse(soraResponse, respBody, soraReq.Model, quota, soraReq.Seconds, soraReq.Size)
}

// parseSoraVideoResponse converts a SoraVideoResponse into a VideoTaskResult.
func parseSoraVideoResponse(soraResponse SoraVideoResponse, body []byte, modelName string, quota int64, secondsStr string, size string) (*relaychannel.VideoTaskResult, *model.ErrorWithStatusCode) {
	if soraResponse.Error != nil {
		return nil, &model.ErrorWithStatusCode{
			Error: model.Error{
				Message: soraResponse.Error.Message,
				Type:    soraResponse.Error.Type,
				Code:    soraResponse.Error.Code,
			},
			StatusCode: soraResponse.StatusCode,
		}
	}
	if soraResponse.StatusCode == 200 {
		return &relaychannel.VideoTaskResult{
			TaskId:     soraResponse.ID,
			TaskStatus: "succeed",
			Message:    fmt.Sprintf("Video generation request submitted successfully, task_id: %s", soraResponse.ID),
			Mode:       size,
			Duration:   secondsStr,
			VideoType:  "text-to-video",
			VideoId:    "",
			Quota:      quota,
		}, nil
	}

	// Non-200 status
	errMsg := fmt.Sprintf("Request failed with status code: %d", soraResponse.StatusCode)
	log.Printf("Sora video request failed: status=%d, body=%s", soraResponse.StatusCode, string(body))
	return nil, &model.ErrorWithStatusCode{
		Error: model.Error{
			Message: errMsg,
			Type:    "api_error",
		},
		StatusCode: soraResponse.StatusCode,
	}
}

// HandleVideoResult queries the Sora API for the current task status.
func (a *VideoAdaptor) HandleVideoResult(c *gin.Context, videoTask *dbmodel.Video, ch *dbmodel.Channel, cfg *dbmodel.ChannelConfig) (*model.GeneralFinalVideoResponse, *model.ErrorWithStatusCode) {
	taskId := videoTask.TaskId

	// Return from cache if available.
	if videoTask.StoreUrl != "" {
		log.Printf("Found existing store URL for Sora task %s: %s", taskId, videoTask.StoreUrl)

		var videoUrls []string
		if err := json.Unmarshal([]byte(videoTask.StoreUrl), &videoUrls); err != nil {
			videoUrls = []string{videoTask.StoreUrl}
		}

		videoResults := make([]model.VideoResultItem, len(videoUrls))
		for i, url := range videoUrls {
			videoResults[i] = model.VideoResultItem{Url: url}
		}

		return &model.GeneralFinalVideoResponse{
			TaskId:       taskId,
			VideoResult:  videoUrls[0],
			VideoId:      taskId,
			TaskStatus:   "succeed",
			Message:      "Video retrieved from cache",
			VideoResults: videoResults,
			Duration:     videoTask.Duration,
		}, nil
	}

	// Build status query URL.
	baseUrl := *ch.BaseURL
	if baseUrl == "" {
		baseUrl = "https://api.openai.com"
	}
	var fullRequestUrl string
	if ch.Type == common.ChannelTypeAzure {
		fullRequestUrl = fmt.Sprintf("%s/openai/v1/videos/%s", baseUrl, taskId)
	} else {
		fullRequestUrl = fmt.Sprintf("%s/v1/videos/%s", baseUrl, taskId)
	}

	req, err := http.NewRequest("GET", fullRequestUrl, nil)
	if err != nil {
		return nil, ErrorWrapper(fmt.Errorf("failed to create request: %v", err), "api_error", http.StatusInternalServerError)
	}

	req.Header.Set("Content-Type", "application/json")
	if ch.Type == common.ChannelTypeAzure {
		req.Header.Set("Api-key", ch.Key)
	} else {
		req.Header.Set("Authorization", "Bearer "+ch.Key)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, ErrorWrapper(fmt.Errorf("failed to fetch video result: %v", err), "api_error", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, ErrorWrapper(fmt.Errorf("API error: %s", string(body)), "api_error", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, ErrorWrapper(fmt.Errorf("failed to read response body: %v", err), "internal_error", http.StatusInternalServerError)
	}

	log.Printf("Sora video query response body: %s", string(body))

	var soraResp SoraVideoResponse
	if err := json.Unmarshal(body, &soraResp); err != nil {
		return nil, ErrorWrapper(fmt.Errorf("failed to parse Sora response JSON: %v", err), "json_parse_error", http.StatusInternalServerError)
	}

	generalResponse := &model.GeneralFinalVideoResponse{
		TaskId:     taskId,
		VideoId:    taskId,
		TaskStatus: "processing",
		Message:    "Video is still processing",
		Duration:   videoTask.Duration,
	}

	switch soraResp.Status {
	case "completed":
		log.Printf("Sora video completed, downloading: task_id=%s", taskId)
		videoUrl, downloadErr := downloadAndUploadSoraVideoInternal(ch, taskId, videoTask.UserId)
		if downloadErr != nil {
			generalResponse.TaskStatus = "processing"
			generalResponse.Message = fmt.Sprintf("Video completed but download failed, please retry: %v", downloadErr)
			log.Printf("Failed to download Sora video for task %s: %v", taskId, downloadErr)
		} else {
			generalResponse.TaskStatus = "succeed"
			generalResponse.Message = "Video generation completed and uploaded to R2"
			generalResponse.VideoResult = videoUrl
			generalResponse.VideoResults = []model.VideoResultItem{{Url: videoUrl}}
		}

	case "failed":
		generalResponse.TaskStatus = "failed"
		if soraResp.Error != nil {
			generalResponse.Message = fmt.Sprintf("Video generation failed: %s", soraResp.Error.Message)
		} else {
			generalResponse.Message = "Video generation failed"
		}

	case "queued", "processing":
		generalResponse.TaskStatus = "processing"
		if soraResp.Progress > 0 {
			generalResponse.Message = fmt.Sprintf("Video generation in progress (%d%%)", soraResp.Progress)
		} else {
			generalResponse.Message = "Video generation in progress"
		}

	default:
		generalResponse.TaskStatus = "processing"
		generalResponse.Message = fmt.Sprintf("Video status: %s", soraResp.Status)
	}

	return generalResponse, nil
}

// downloadAndUploadSoraVideoInternal downloads a Sora video and uploads it to R2.
func downloadAndUploadSoraVideoInternal(channel *dbmodel.Channel, videoId string, userId int) (string, error) {
	baseUrl := *channel.BaseURL
	if baseUrl == "" {
		baseUrl = "https://api.openai.com"
	}
	var downloadUrl string
	if channel.Type == common.ChannelTypeAzure {
		downloadUrl = fmt.Sprintf("%s/openai/v1/videos/%s/content", baseUrl, videoId)
	} else {
		downloadUrl = fmt.Sprintf("%s/v1/videos/%s/content", baseUrl, videoId)
	}

	log.Printf("Downloading Sora video: %s", downloadUrl)

	client := &http.Client{
		Timeout: 5 * time.Minute,
	}

	maxRetries := 5
	var resp *http.Response
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			waitSeconds := time.Duration(attempt*3) * time.Second
			log.Printf("Sora video content not ready yet, retrying in %v (attempt %d/%d): %s", waitSeconds, attempt, maxRetries, videoId)
			time.Sleep(waitSeconds)
		}

		req, err := http.NewRequest("GET", downloadUrl, nil)
		if err != nil {
			return "", fmt.Errorf("failed to create download request: %w", err)
		}

		if channel.Type == common.ChannelTypeAzure {
			req.Header.Set("api-key", channel.Key)
		} else {
			req.Header.Set("Authorization", "Bearer "+channel.Key)
		}

		resp, lastErr = client.Do(req)
		if lastErr != nil {
			lastErr = fmt.Errorf("failed to download video: %w", lastErr)
			continue
		}

		if resp.StatusCode == 404 {
			resp.Body.Close()
			lastErr = fmt.Errorf("video not ready yet (404)")
			continue
		}

		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return "", fmt.Errorf("download failed with status %d: %s", resp.StatusCode, string(body))
		}

		lastErr = nil
		break
	}

	if lastErr != nil {
		return "", fmt.Errorf("download failed after %d retries: %w", maxRetries, lastErr)
	}
	defer resp.Body.Close()

	videoData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read video data: %w", err)
	}

	log.Printf("Downloaded Sora video: %d bytes", len(videoData))

	base64Data := base64.StdEncoding.EncodeToString(videoData)

	videoUrl, err := relaychannel.UploadVideoBase64ToR2(base64Data, userId, "mp4")
	if err != nil {
		return "", fmt.Errorf("failed to upload to R2: %w", err)
	}

	log.Printf("Successfully uploaded Sora video to R2: %s", videoUrl)
	return videoUrl, nil
}

// calcSoraQuota calculates the quota for a Sora video request.
func calcSoraQuota(modelName string, secondsStr string, size string) int64 {
	var pricePerSecond float64
	isHighRes := size == "1024x1792" || size == "1792x1024"

	if modelName == "sora-2" {
		pricePerSecond = 0.10
	} else if modelName == "sora-2-pro" {
		if isHighRes {
			pricePerSecond = 0.50
		} else {
			pricePerSecond = 0.30
		}
	} else {
		pricePerSecond = 0.10
	}

	seconds, err := strconv.Atoi(secondsStr)
	if err != nil || seconds == 0 {
		seconds = 4
		log.Printf("Invalid seconds value '%s', using default 4", secondsStr)
	}

	totalPriceUSD := float64(seconds) * pricePerSecond
	quota := int64(totalPriceUSD * config.QuotaPerUnit)

	log.Printf("Sora video pricing: model=%s, seconds=%s (%d), size=%s, pricePerSecond=%.2f, totalUSD=%.6f, quota=%d",
		modelName, secondsStr, seconds, size, pricePerSecond, totalPriceUSD, quota)

	return quota
}

// soraHandleInputReference handles input_reference in URL/base64/data-URL formats.
func soraHandleInputReference(writer *multipart.Writer, inputReference string) error {
	if strings.HasPrefix(inputReference, "http://") || strings.HasPrefix(inputReference, "https://") {
		return soraHandleInputReferenceURL(writer, inputReference)
	} else if strings.HasPrefix(inputReference, "data:") {
		return soraHandleInputReferenceDataURL(writer, inputReference)
	}
	return soraHandleInputReferenceBase64(writer, inputReference)
}

func soraHandleInputReferenceURL(writer *multipart.Writer, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download input_reference from URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("failed to download input_reference: HTTP %d", resp.StatusCode)
	}

	fileData, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read file data: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")

	filename := ""
	if strings.Contains(contentType, "image/jpeg") || strings.Contains(contentType, "image/jpg") {
		filename = "input_reference.jpg"
	} else if strings.Contains(contentType, "image/png") {
		filename = "input_reference.png"
	} else if strings.Contains(contentType, "image/webp") {
		filename = "input_reference.webp"
	} else if strings.Contains(contentType, "image/gif") {
		filename = "input_reference.gif"
	}

	if filename == "" {
		urlLower := strings.ToLower(url)
		if strings.HasSuffix(urlLower, ".jpg") || strings.HasSuffix(urlLower, ".jpeg") {
			filename = "input_reference.jpg"
		} else if strings.HasSuffix(urlLower, ".png") {
			filename = "input_reference.png"
		} else if strings.HasSuffix(urlLower, ".webp") {
			filename = "input_reference.webp"
		} else if strings.HasSuffix(urlLower, ".gif") {
			filename = "input_reference.gif"
		} else if strings.Contains(urlLower, ".jpg?") || strings.Contains(urlLower, ".jpeg?") {
			filename = "input_reference.jpg"
		} else if strings.Contains(urlLower, ".png?") {
			filename = "input_reference.png"
		} else if strings.Contains(urlLower, ".webp?") {
			filename = "input_reference.webp"
		} else {
			filename = soraDetectImageFilename(fileData)
		}
	}

	log.Printf("Input reference URL: %s, Content-Type: %s, detected filename: %s", url, contentType, filename)

	mimeType := "image/jpeg"
	if strings.HasSuffix(filename, ".png") {
		mimeType = "image/png"
	} else if strings.HasSuffix(filename, ".webp") {
		mimeType = "image/webp"
	} else if strings.HasSuffix(filename, ".gif") {
		mimeType = "image/gif"
	}

	h := make(map[string][]string)
	h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="input_reference"; filename="%s"`, filename)}
	h["Content-Type"] = []string{mimeType}

	part, err := writer.CreatePart(h)
	if err != nil {
		return fmt.Errorf("failed to create form part: %w", err)
	}

	_, err = part.Write(fileData)
	if err != nil {
		return fmt.Errorf("failed to write file data: %w", err)
	}

	log.Printf("Input reference URL uploaded: %s, MIME: %s, filename: %s, size: %d bytes", url, mimeType, filename, len(fileData))
	return nil
}

func soraHandleInputReferenceDataURL(writer *multipart.Writer, dataURL string) error {
	parts := strings.SplitN(dataURL, ",", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid data URL format")
	}

	header := parts[0]
	data := parts[1]

	fileData, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return fmt.Errorf("failed to decode base64 from data URL: %w", err)
	}

	filename := "input_reference.jpg"
	mimeType := "image/jpeg"

	if strings.Contains(header, "image/png") {
		filename = "input_reference.png"
		mimeType = "image/png"
	} else if strings.Contains(header, "image/jpeg") || strings.Contains(header, "image/jpg") {
		filename = "input_reference.jpg"
		mimeType = "image/jpeg"
	} else if strings.Contains(header, "image/gif") {
		filename = "input_reference.gif"
		mimeType = "image/gif"
	} else if strings.Contains(header, "image/webp") {
		filename = "input_reference.webp"
		mimeType = "image/webp"
	}

	h := make(map[string][]string)
	h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="input_reference"; filename="%s"`, filename)}
	h["Content-Type"] = []string{mimeType}

	part, err := writer.CreatePart(h)
	if err != nil {
		return fmt.Errorf("failed to create form part: %w", err)
	}

	_, err = part.Write(fileData)
	if err != nil {
		return fmt.Errorf("failed to write file data: %w", err)
	}

	log.Printf("Input reference data URL processed: filename=%s, MIME=%s, size=%d bytes", filename, mimeType, len(fileData))
	return nil
}

func soraHandleInputReferenceBase64(writer *multipart.Writer, base64Data string) error {
	fileData, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return fmt.Errorf("failed to decode base64: %w", err)
	}

	filename := soraDetectImageFilename(fileData)

	mimeType := "image/jpeg"
	if strings.HasSuffix(filename, ".png") {
		mimeType = "image/png"
	} else if strings.HasSuffix(filename, ".webp") {
		mimeType = "image/webp"
	} else if strings.HasSuffix(filename, ".gif") {
		mimeType = "image/gif"
	}

	h := make(map[string][]string)
	h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="input_reference"; filename="%s"`, filename)}
	h["Content-Type"] = []string{mimeType}

	part, err := writer.CreatePart(h)
	if err != nil {
		return fmt.Errorf("failed to create form part: %w", err)
	}

	_, err = part.Write(fileData)
	if err != nil {
		return fmt.Errorf("failed to write file data: %w", err)
	}

	log.Printf("Input reference base64 processed: filename=%s, MIME=%s, size=%d bytes", filename, mimeType, len(fileData))
	return nil
}

// soraDetectImageFilename detects image type from file header and returns a suitable filename.
func soraDetectImageFilename(data []byte) string {
	if len(data) < 12 {
		return "input_reference.jpg"
	}

	if len(data) >= 2 && data[0] == 0xFF && data[1] == 0xD8 {
		return "input_reference.jpg"
	} else if len(data) >= 8 && data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 {
		return "input_reference.png"
	} else if len(data) >= 12 && string(data[8:12]) == "WEBP" {
		return "input_reference.webp"
	} else if len(data) >= 6 && string(data[0:3]) == "GIF" {
		return "input_reference.gif"
	}

	return "input_reference.jpg"
}
