package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	dbmodel "github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/channel/xai"
	"github.com/songquanpeng/one-api/relay/util"
)

// DirectRelayXaiVideoGeneration handles POST /xai/v1/videos/generations
func DirectRelayXaiVideoGeneration(c *gin.Context, meta *util.RelayMeta) {
	directRelayXaiVideo(c, meta, "generations")
}

// DirectRelayXaiVideoEdit handles POST /xai/v1/videos/edits
func DirectRelayXaiVideoEdit(c *gin.Context, meta *util.RelayMeta) {
	directRelayXaiVideo(c, meta, "edits")
}

// DirectRelayXaiVideoExtension handles POST /xai/v1/videos/extensions
func DirectRelayXaiVideoExtension(c *gin.Context, meta *util.RelayMeta) {
	directRelayXaiVideo(c, meta, "extensions")
}

func directRelayXaiVideo(c *gin.Context, meta *util.RelayMeta, endpoint string) {
	ctx := c.Request.Context()

	requestBody, readErr := common.GetRequestBody(c)
	if readErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body: " + readErr.Error()})
		return
	}

	params := xai.ParseNativeVideoParams(requestBody)
	quota := xai.CalculateNativeVideoQuota(endpoint, params)

	userQuota, err := dbmodel.CacheGetUserQuota(ctx, meta.UserId)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get user quota: " + err.Error()})
		return
	}
	if userQuota-quota < 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": "insufficient user balance"})
		return
	}

	baseURL := meta.BaseURL
	if baseURL == "" {
		baseURL = "https://api.x.ai"
	}

	logger.Infof(ctx, "[xAI Video] %s request - url=%s/v1/videos/%s, quota=%d", endpoint, baseURL, endpoint, quota)

	resp, err := xai.SendNativeVideoRequest(baseURL, meta.APIKey, endpoint, requestBody)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	logger.Infof(ctx, "[xAI Video] %s response - status=%d", endpoint, resp.StatusCode)

	if (resp.StatusCode == 200 || resp.StatusCode == 202) && resp.RequestId != "" {
		handleXaiVideoBilling(c, meta, quota, resp.RequestId)

		duration := xai.NativeDurationStr(endpoint, params)
		resolution := params.Resolution
		if resolution == "" {
			resolution = "480p"
		}
		if logErr := CreateVideoLog("xai", resp.RequestId, meta, endpoint, duration, "grok-video", resp.RequestId, quota, 0, resolution, ""); logErr != nil {
			logger.Errorf(ctx, "[xAI Video] 创建视频日志失败: %v", logErr)
		}
	}

	writeUpstreamResponse(c, resp.StatusCode, resp.Header, resp.Body)
}

// GetXaiVideoResult handles GET /xai/v1/videos/:requestId
func GetXaiVideoResult(c *gin.Context, requestId string) {
	ctx := c.Request.Context()
	logger.Debugf(ctx, "[xAI Video] GetResult - requestId=%s", requestId)

	task, err := dbmodel.GetVideoTaskById(requestId)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "video task not found: " + err.Error()})
		return
	}

	if task.Result != "" && (task.Status == "succeed" || task.Status == "failed" || task.Status == "expired") {
		logger.Debugf(ctx, "[xAI Video] GetResult from db - requestId=%s, status=%s", requestId, task.Status)
		c.Data(http.StatusOK, "application/json", []byte(task.Result))
		return
	}

	channel, err := dbmodel.GetChannelById(task.ChannelId, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get channel info: " + err.Error()})
		return
	}

	apiKey := xai.ResolveAPIKey(task, channel)
	baseURL := xai.ResolveBaseURL(channel)

	resp, err := xai.FetchNativeVideoResult(baseURL, apiKey, requestId)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	logger.Debugf(ctx, "[xAI Video] GetResult response - requestId=%s, status=%d", requestId, resp.StatusCode)

	if resp.StatusCode == 200 || resp.StatusCode == 202 {
		xai.UpdateNativeVideoTaskStatus(requestId, resp.Body, task)
	}

	writeUpstreamResponse(c, resp.StatusCode, resp.Header, resp.Body)
}

// writeUpstreamResponse writes the upstream response back to the client transparently.
func writeUpstreamResponse(c *gin.Context, statusCode int, header http.Header, body []byte) {
	for key, values := range header {
		if strings.EqualFold(key, "content-length") {
			continue
		}
		for _, value := range values {
			c.Writer.Header().Add(key, value)
		}
	}
	c.Data(statusCode, header.Get("Content-Type"), body)
}

func handleXaiVideoBilling(c *gin.Context, meta *util.RelayMeta, quota int64, taskId string) {
	referer := c.Request.Header.Get("HTTP-Referer")
	title := c.Request.Header.Get("X-Title")

	if err := dbmodel.PostConsumeTokenQuota(meta.TokenId, quota); err != nil {
		logger.Errorf(c.Request.Context(), "[xAI Video] 扣除token配额失败: %v", err)
		return
	}
	_ = dbmodel.CacheUpdateUserQuota(context.Background(), meta.UserId)

	if quota != 0 {
		tokenName := c.GetString("token_name")
		logContent := fmt.Sprintf("xAI Video Generation model: %s, total cost: $%.6f",
			meta.BillingModelName(), float64(quota)/config.QuotaPerUnit)
		dbmodel.RecordVideoConsumeLog(context.Background(), meta.UserId, meta.ChannelId,
			0, 0, meta.OriginModelName, tokenName, quota, logContent, 0, title, referer, taskId)
		dbmodel.UpdateUserUsedQuotaAndRequestCount(meta.UserId, quota)
		dbmodel.UpdateChannelUsedQuota(meta.ChannelId, quota)
	}
}
