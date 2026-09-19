package controller

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/constant"
	relaymodel "github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

type transcriptionDurationResponse struct {
	Type     string          `json:"type"`
	Error    json.RawMessage `json:"error"`
	Duration *float64        `json:"duration"`
	Usage    *struct {
		Type    string   `json:"type"`
		Seconds *float64 `json:"seconds"`
	} `json:"usage"`
}

func (r transcriptionDurationResponse) billedSeconds() *float64 {
	seconds := r.Duration
	if r.Usage != nil && r.Usage.Type == "duration" && r.Usage.Seconds != nil {
		seconds = r.Usage.Seconds
	}
	if len(r.Error) > 0 && string(r.Error) != "null" {
		return nil
	}
	if seconds == nil || *seconds < 0 || math.IsNaN(*seconds) || math.IsInf(*seconds, 0) {
		return nil
	}
	return seconds
}

func audioDurationQuota(seconds, price, quotaPerUnit, groupRatio float64) (int64, error) {
	for _, value := range []float64{seconds, price, quotaPerUnit, groupRatio} {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, fmt.Errorf("invalid audio duration billing value")
		}
	}
	if quotaPerUnit == 0 {
		return 0, fmt.Errorf("quota per unit must be positive")
	}
	// Treat configured decimal prices as decimal values, avoiding binary float
	// drift across half-quota boundaries (e.g. 9 seconds costs 337.5 quota).
	cost := new(big.Rat).SetInt64(1)
	for _, value := range []float64{seconds, price, quotaPerUnit, groupRatio} {
		decimal, _ := new(big.Rat).SetString(strconv.FormatFloat(value, 'f', -1, 64))
		cost.Mul(cost, decimal)
	}
	cost.Quo(cost, big.NewRat(60, 1))
	cost.Add(cost, big.NewRat(1, 2))
	quota := new(big.Int).Quo(cost.Num(), cost.Denom())
	if !quota.IsInt64() {
		return 0, fmt.Errorf("audio duration quota overflow")
	}
	return quota.Int64(), nil
}

// forwardDurationTranscription preserves JSON/SSE responses and reads only
// terminal, cumulative duration usage. Missing usage never becomes token usage.
func forwardDurationTranscription(c *gin.Context, resp *http.Response) (*float64, error) {
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		var result transcriptionDurationResponse
		if json.Unmarshal(body, &result) != nil {
			result = transcriptionDurationResponse{}
		}
		if len(result.Error) > 0 && string(result.Error) != "null" {
			return nil, fmt.Errorf("upstream returned a transcription error")
		}
		for key, values := range resp.Header {
			c.Writer.Header()[key] = values
		}
		c.Status(resp.StatusCode)
		_, err = c.Writer.Write(body)
		// The upstream completed the request even if the client has disconnected.
		if err != nil {
			logger.Warn(c.Request.Context(), "transcription response delivery failed: "+err.Error())
		}
		return result.billedSeconds(), nil
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Status(resp.StatusCode)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	var seconds *float64
	var eventData, eventType string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			eventData += strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ") + "\n"
		} else if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if line == "" {
			var event transcriptionDurationResponse
			if json.Unmarshal([]byte(eventData), &event) == nil && (event.Type == "transcript.text.done" || eventType == "transcript.text.done") {
				if reported := event.billedSeconds(); reported != nil {
					seconds = reported
				}
			}
			eventData, eventType = "", ""
		}
		if _, err := c.Writer.WriteString(line + "\n"); err != nil {
			break
		}
		if line == "" {
			c.Writer.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Warn(c.Request.Context(), "transcription stream interrupted: "+err.Error())
	}
	return seconds, nil
}

func relayDurationTranscription(c *gin.Context, relayMode int, modelName string, price float64) *relaymodel.ErrorWithStatusCode {
	ctx := c.Request.Context()
	start := time.Now()
	userID, tokenID, channelID := c.GetInt("id"), c.GetInt("token_id"), c.GetInt("channel_id")
	// Freeze all tariff inputs before sending the request.
	groupRatio := util.GetBillingGroupRatio(c, c.GetString("group"), modelName)
	quotaPerUnit := config.QuotaPerUnit
	reserve, err := audioDurationQuota(60, price, quotaPerUnit, groupRatio)
	if err != nil {
		return openai.ErrorWrapper(err, "invalid_duration_price", http.StatusBadRequest)
	}
	form, err := c.MultipartForm()
	if err != nil {
		return openai.ErrorWrapper(err, "invalid_multipart_form", http.StatusBadRequest)
	}
	if len(form.File["file"]) != 1 {
		return openai.ErrorWrapper(fmt.Errorf("exactly one audio file is required"), "invalid_audio_file", http.StatusBadRequest)
	}
	format := c.DefaultPostForm("response_format", "json")
	if len(form.Value["response_format"]) > 1 {
		return openai.ErrorWrapper(fmt.Errorf("only one response_format is allowed"), "invalid_audio_response_format", http.StatusBadRequest)
	}
	if format != "json" && format != "verbose_json" && format != "diarized_json" {
		return openai.ErrorWrapper(fmt.Errorf("duration billing requires json, verbose_json, or diarized_json response format"), "unsupported_audio_billing_format", http.StatusBadRequest)
	}
	upstreamModel := modelName
	if mapping := c.GetString("model_mapping"); mapping != "" {
		var models map[string]string
		if err := json.Unmarshal([]byte(mapping), &models); err != nil {
			return openai.ErrorWrapper(err, "invalid_model_mapping", http.StatusInternalServerError)
		}
		if models[modelName] != "" {
			upstreamModel = models[modelName]
		}
	}
	channelType := c.GetInt("channel")
	baseURL := c.GetString("base_url")
	if baseURL == "" {
		baseURL = common.ChannelBaseURLs[channelType]
	}
	url := util.GetFullRequestURL(baseURL, c.Request.URL.String(), channelType)
	if channelType == common.ChannelTypeAzure {
		action := "transcriptions"
		if relayMode == constant.RelayModeAudioTranslation {
			action = "translations"
		}
		url = fmt.Sprintf("%s/openai/deployments/%s/audio/%s?api-version=%s", strings.TrimRight(baseURL, "/"), upstreamModel, action, util.GetAzureAPIVersion(c))
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fileHeader := form.File["file"][0]
	file, err := fileHeader.Open()
	if err != nil {
		return openai.ErrorWrapper(err, "open_audio_failed", http.StatusBadRequest)
	}
	part, err := writer.CreateFormFile("file", fileHeader.Filename)
	if err == nil {
		_, err = io.Copy(part, file)
	}
	file.Close()
	if err != nil {
		return openai.ErrorWrapper(err, "copy_audio_failed", http.StatusInternalServerError)
	}
	for name, values := range form.Value {
		if name == "model" {
			continue
		}
		for _, value := range values {
			if err := writer.WriteField(name, value); err != nil {
				return openai.ErrorWrapper(err, "write_audio_field_failed", http.StatusInternalServerError)
			}
		}
	}
	if err := writer.WriteField("model", upstreamModel); err != nil {
		return openai.ErrorWrapper(err, "write_audio_model_failed", http.StatusInternalServerError)
	}
	if err := writer.Close(); err != nil {
		return openai.ErrorWrapper(err, "close_audio_form_failed", http.StatusInternalServerError)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return openai.ErrorWrapper(err, "new_audio_request_failed", http.StatusInternalServerError)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", c.GetHeader("Accept"))
	if channelType == common.ChannelTypeAzure {
		req.Header.Set("api-key", strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	} else {
		req.Header.Set("Authorization", c.GetHeader("Authorization"))
	}
	if err := model.AdjustAudioQuota(userID, tokenID, reserve, true); err != nil {
		return openai.ErrorWrapper(err, "insufficient_audio_quota", http.StatusForbidden)
	}
	settled := false
	defer func() {
		if !settled {
			if err := model.AdjustAudioQuota(userID, tokenID, -reserve, false); err != nil {
				logger.Error(ctx, "audio quota refund failed: "+err.Error())
			}
		}
		if err := model.CacheUpdateUserQuota2(userID); err != nil {
			logger.Error(ctx, "audio quota cache refresh failed: "+err.Error())
		}
	}()
	if err := model.CacheUpdateUserQuota2(userID); err != nil {
		logger.Warn(ctx, "audio reservation cache refresh failed: "+err.Error())
	}
	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		return openai.ErrorWrapper(err, "audio_request_failed", http.StatusBadGateway)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return util.RelayErrorHandler(resp)
	}
	seconds, err := forwardDurationTranscription(c, resp)
	if err != nil {
		return openai.ErrorWrapper(err, "audio_response_failed", http.StatusBadGateway)
	}
	var quota int64
	source := "missing"
	if seconds != nil {
		quota, err = audioDurationQuota(*seconds, price, quotaPerUnit, groupRatio)
		if err != nil {
			logger.Error(ctx, err.Error())
			return nil
		}
		source = "upstream"
	}
	if err := model.AdjustAudioQuota(userID, tokenID, quota-reserve, false); err != nil {
		logger.Error(ctx, "audio quota settlement failed: "+err.Error())
		return nil
	}
	settled = true
	other, _ := json.Marshal(struct {
		Mode       string   `json:"billing_mode"`
		Price      float64  `json:"duration_price_per_minute"`
		Seconds    *float64 `json:"audio_duration_seconds,omitempty"`
		Source     string   `json:"transcription_usage_source"`
		GroupRatio float64  `json:"group_ratio"`
	}{"duration", price, seconds, source, groupRatio})
	if seconds == nil {
		logger.Warn(ctx, "transcription usage unavailable; reserved quota refunded")
	}
	model.UpdateUserUsedQuotaAndRequestCount(userID, quota)
	model.UpdateChannelUsedQuota(channelID, quota)
	model.RecordConsumeLogWithOtherAndRequestID(ctx, userID, channelID, 0, 0, modelName, c.GetString("token_name"), quota,
		"audio duration billing", time.Since(start).Seconds(), c.GetHeader("X-Title"), c.GetHeader("HTTP-Referer"),
		strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"), 0, string(other), c.GetString("X-Request-ID"), 0, "")
	return nil
}
