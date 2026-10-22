package controller

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	relaymodel "github.com/songquanpeng/one-api/relay/model"

	dbmodel "github.com/songquanpeng/one-api/model"
)

func appendUniqueChannelID(channelIDs []int, channelID int) []int {
	if channelID <= 0 {
		return channelIDs
	}
	for _, existingChannelID := range channelIDs {
		if existingChannelID == channelID {
			return channelIDs
		}
	}
	return append(channelIDs, channelID)
}

// selectRetryChannel 选择重试渠道，始终从最高优先级开始（skipPriorityLevels=0），
// 依靠 CacheGetRandomSatisfiedChannel 内部 fallback 自然降级。
// 若当前轮次所有渠道已耗尽，则重置 failedChannelIds 从头循环。
func selectRetryChannel(ctx context.Context, group string, model string, failedChannelIds *[]int) (*dbmodel.Channel, error) {
	channel, _, err := dbmodel.CacheGetRandomSatisfiedChannel(
		ctx, group, model, 0, "", *failedChannelIds,
	)
	if err != nil {
		// provider 组内耗尽时明确失败，不重置失败列表或进入其他 provider。
		if dbmodel.HasRetryBoundary(ctx) {
			return nil, err
		}
		// 所有优先级均已耗尽，重置后从最高优先级重新开始
		*failedChannelIds = nil
		channel, _, err = dbmodel.CacheGetRandomSatisfiedChannel(
			ctx, group, model, 0, "", nil,
		)
	}
	return channel, err
}

// responsesRetryDelay 保留上游的等待要求，超过本次允许等待的上限时停止，不能提前重试。
func responsesRetryDelay(retryAfter, retryAfterMS string, attempt int, now time.Time) (time.Duration, error) {
	delay := time.Duration(1<<min(max(attempt, 1), 4)) * time.Second
	const limit = 30 * time.Second
	if ms, err := strconv.ParseInt(strings.TrimSpace(retryAfterMS), 10, 64); err == nil && ms >= 0 {
		if ms > int64(limit/time.Millisecond) {
			return 0, fmt.Errorf("upstream retry-after-ms exceeds the 30 second retry wait limit")
		}
		return max(time.Millisecond, time.Duration(ms)*time.Millisecond), nil
	}
	if seconds, err := strconv.ParseInt(strings.TrimSpace(retryAfter), 10, 64); err == nil && seconds >= 0 {
		if seconds > int64(limit/time.Second) {
			return 0, fmt.Errorf("upstream Retry-After exceeds the 30 second retry wait limit")
		}
		delay = time.Duration(seconds) * time.Second
	} else if until, err := http.ParseTime(retryAfter); err == nil {
		delay = until.Sub(now)
	}
	if delay > limit {
		return 0, fmt.Errorf("upstream Retry-After exceeds the 30 second retry wait limit")
	}
	return max(time.Millisecond, delay), nil
}

// selectResponsesRetryChannel 对状态绑定的限流请求退避后重试原资源，不放宽历史来源边界。
func selectResponsesRetryChannel(c *gin.Context, group, model string, failedIDs *[]int, status, attempt int) (*dbmodel.Channel, string, error) {
	ctx := c.Request.Context()
	if err := ctx.Err(); err != nil {
		return nil, "client_cancelled", err
	}
	if dbmodel.ResponseStateChannel(ctx) <= 0 || status != http.StatusTooManyRequests {
		channel, err := selectRetryChannel(ctx, group, model, failedIDs)
		return channel, "", err
	}
	delay, err := responsesRetryDelay(c.GetString("responses_retry_after"), c.GetString("responses_retry_after_ms"), attempt, time.Now())
	if err != nil {
		return nil, "retry_after_exceeds_limit", err
	}
	logger.Infof(ctx, "responses_retry_diag event=retry_wait next_attempt=%d channel=%d delay_ms=%d reason=state_bound_rate_limit", attempt+1, dbmodel.ResponseStateChannel(ctx), delay.Milliseconds())
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, "client_cancelled", ctx.Err()
	case <-timer.C:
	}
	if err := ctx.Err(); err != nil {
		return nil, "client_cancelled", err
	}
	// 不传失败排除列表，但仍由原选渠器检查分组、模型、状态和 provider / channel 边界。
	channel, _, err := dbmodel.CacheGetRandomSatisfiedChannel(ctx, group, model, 0, "", nil)
	return channel, "", err
}

func getLastRetryFallbackChannel(channelID int) *dbmodel.Channel {
	if channelID <= 0 {
		return nil
	}
	if channel, err := dbmodel.CacheGetChannel(channelID); err == nil {
		return channel
	}
	if channel, err := dbmodel.GetChannelById(channelID, true); err == nil {
		return channel
	}
	return nil
}

// responsesRetryStopReason 区分协议边界与普通上游错误，便于解释单次失败日志。
func responsesRetryStopReason(c *gin.Context, relayError *relaymodel.ErrorWithStatusCode, retriesRemaining int) (reason string) {
	// 临时诊断：记录实际走到的分支，不改变重试策略。
	defer func() {
		action, diagnosticReason := "retry", reason
		if diagnosticReason != "" {
			action = "stop"
		} else if retriesRemaining <= 0 {
			action, diagnosticReason = "stop", "retry_limit_reached"
		}
		_, forced := c.Get("specific_channel_id")
		ctx := c.Request.Context()
		history, _ := c.Get("admin_channel_history")
		channelHistory, _ := history.([]int)
		logger.Infof(ctx, "responses_retry_diag event=decision attempt=%d channel=%d status=%d upstream_http_status=%d retries_remaining=%d configured_retries=%d provider=%q state_channel=%d specific_channel=%t state_record_failed=%t systemone_no_retry=%t response_written=%t response_bytes=%d first_write=%q last_upstream_event=%q non_prelude_events=%d text_deltas=%d client_cancelled=%t action=%s reason=%q",
			len(channelHistory), c.GetInt("channel_id"), relayError.StatusCode,
			c.GetInt("responses_diag_http_status"), retriesRemaining, config.RetryTimes, dbmodel.RetryProvider(ctx), dbmodel.ResponseStateChannel(ctx),
			forced, c.GetBool("responses_state_record_failed"), c.GetBool("systemone_no_retry"), c.Writer.Written(), c.Writer.Size(),
			c.GetString("responses_diag_first_write"), c.GetString("responses_diag_last_event"), c.GetInt("responses_diag_non_prelude_events"), c.GetInt("responses_diag_text_deltas"),
			ctx.Err() != nil, action, diagnosticReason)
	}()
	if c.Request.Context().Err() != nil {
		return "client_cancelled"
	}
	if c.Writer.Written() {
		return "response_already_started"
	}
	if c.GetBool("responses_state_record_failed") {
		return "response_state_record_failed"
	}
	if _, forced := c.Get("specific_channel_id"); forced {
		return "specific_channel_requested"
	}
	if !shouldRetry(c, relayError.StatusCode, relayError.Error.Message) {
		return "non_retryable_error"
	}
	return ""
}
