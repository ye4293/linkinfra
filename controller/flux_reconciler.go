package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/channel/flux"
	"github.com/songquanpeng/one-api/relay/util"
)

const (
	fluxReconcileInterval = 30 * time.Second
	fluxReconcileBatch    = 50
	// 超过 15 分钟仍未终态，直接判定失败（BFL 结果 URL 有效期约 10 分钟，
	// 15min 阈值给慢任务留余量，但避免在 URL 早已失效后继续无意义查询）
	fluxReconcileExpireSecs = 15 * 60
	// 同时并发查询上游的 goroutine 上限，防止大批量任务打爆 BFL/CPU
	fluxQueryConcurrency = 50
)

// reconcilerMu 防止两次 tick 并发执行（上次未完成时跳过）
var reconcilerMu sync.Mutex

// fluxQuerySem 全局信号量，限制同时执行上游查询的 goroutine 数
var fluxQuerySem = make(chan struct{}, fluxQueryConcurrency)

// isFluxReconcilerEnabled 复用 ENABLE_VIDEO_TASK_POLLER 开关（与 ali/xai/doubao 视频
// poller 共用），保持运维侧只需管理一个变量。命名虽叫 "VIDEO"，但实际控制所有后台对账任务。
func isFluxReconcilerEnabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("ENABLE_VIDEO_TASK_POLLER")))
	return v == "true" || v == "1"
}

// StartFluxReconciler 启动后台 Flux/Replicate 任务对账 goroutine
func StartFluxReconciler(ctx context.Context) {
	if !isFluxReconcilerEnabled() {
		logger.Info(ctx, "[flux-reconciler] disabled by ENABLE_VIDEO_TASK_POLLER env, not starting")
		return
	}

	ticker := time.NewTicker(fluxReconcileInterval)
	defer ticker.Stop()

	logger.Info(ctx, "[flux-reconciler] started, interval=30s")

	// 启动时立即跑一次
	runFluxReconcile(ctx)

	for {
		select {
		case <-ctx.Done():
			logger.Info(ctx, "[flux-reconciler] stopped")
			return
		case <-ticker.C:
			runFluxReconcile(ctx)
		}
	}
}

func runFluxReconcile(ctx context.Context) {
	if !reconcilerMu.TryLock() {
		logger.Infof(ctx, "[flux-reconciler] 上次扫描尚未完成，跳过本轮")
		return
	}
	defer reconcilerMu.Unlock()

	now := time.Now().Unix()
	statuses := []string{flux.TaskStatusProcessing, flux.TaskStatusSubmitted}

	// ① 超过 15 分钟仍未终态 → 直接标失败，不再查上游
	expireBefore := now - fluxReconcileExpireSecs
	expired, err := model.ExpireStuckFluxImages(statuses, expireBefore, "task timed out")
	if err != nil {
		logger.Errorf(ctx, "[flux-reconciler] 批量过期失败: %v", err)
	} else if expired > 0 {
		logger.Infof(ctx, "[flux-reconciler] 批量过期 %d 条超时任务", expired)
	}

	// ② 15min 以内的记录 → 查上游尝试对账
	olderThan := now
	newerThan := now - fluxReconcileExpireSecs
	images, err := model.GetStuckFluxImages(statuses, olderThan, newerThan, fluxReconcileBatch)
	if err != nil {
		logger.Errorf(ctx, "[flux-reconciler] 查询卡死任务失败: %v", err)
		return
	}
	if len(images) == 0 {
		return
	}

	logger.Infof(ctx, "[flux-reconciler] 发现 %d 条卡死任务，开始对账（并发上限=%d）", len(images), fluxQueryConcurrency)
	for _, img := range images {
		img := img
		go func() {
			fluxQuerySem <- struct{}{}        // 占用一个并发槽，满额时阻塞等待
			defer func() { <-fluxQuerySem }() // 完成后释放
			reconcileFluxImage(ctx, img)
		}()
	}
}

func reconcileFluxImage(ctx context.Context, image *model.Image) {
	if image.TaskId == "" {
		return
	}

	channel, err := model.GetChannelById(image.ChannelId, true)
	if err != nil || channel.BaseURL == nil || *channel.BaseURL == "" {
		logger.Errorf(ctx, "[flux-reconciler] 获取渠道失败: image_id=%d, channel_id=%d, err=%v",
			image.Id, image.ChannelId, err)
		return
	}

	baseURL := *channel.BaseURL
	apiKey := channel.ResolveKeyByIndex(image.KeyIndex)

	if strings.Contains(baseURL, "replicate.com") {
		reconcileReplicateImage(ctx, image, baseURL, apiKey)
	} else {
		reconcileBFLImage(ctx, image, baseURL, apiKey)
	}
}

// ─── BFL ────────────────────────────────────────────────────────────────────

func reconcileBFLImage(ctx context.Context, image *model.Image, baseURL, apiKey string) {
	// 单次查询；失败/未就绪交给 30s 后的下一轮重试，避免在单轮内长时间阻塞
	poll, err := fetchBFLResult(ctx, image.TaskId, baseURL, apiKey)
	if err != nil {
		logger.Warnf(ctx, "[flux-reconciler] BFL 查询失败（30s 后下一轮重试）: task_id=%s, err=%v",
			image.TaskId, err)
		return
	}
	if !flux.IsUpstreamReady(poll.Status) {
		logger.Debugf(ctx, "[flux-reconciler] BFL 未就绪，等待下一轮: task_id=%s, status=%s",
			image.TaskId, poll.Status)
		return
	}
	if poll.Result == nil || poll.Result.Sample == "" {
		logger.Errorf(ctx, "[flux-reconciler] BFL Ready 但 sample 为空: task_id=%s", image.TaskId)
		return
	}
	applyFluxBFLSuccess(ctx, image, *poll)
}

// fetchBFLResult 向 BFL 发起单次 GET 查询，返回解析后的轮询响应
func fetchBFLResult(ctx context.Context, taskID, baseURL, apiKey string) (*flux.FluxPollingResponse, error) {
	queryURL := fmt.Sprintf("%s/v1/get_result?id=%s", baseURL, taskID)
	req, err := http.NewRequestWithContext(ctx, "GET", queryURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-key", apiKey)

	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	logger.Infof(ctx, "[flux-reconciler] BFL 查询: task_id=%s, status=%d, body=%s",
		taskID, resp.StatusCode, string(body))

	var poll flux.FluxPollingResponse
	if err := json.Unmarshal(body, &poll); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return &poll, nil
}

func applyFluxBFLSuccess(ctx context.Context, image *model.Image, poll flux.FluxPollingResponse) {
	resultBytes, _ := json.Marshal(poll)
	image.Status = flux.TaskStatusSucceed
	image.StoreUrl = poll.Result.Sample
	image.Result = string(resultBytes)

	applied, dbErr := image.UpdateIfNotTerminal()
	if dbErr != nil {
		logger.Errorf(ctx, "[flux-reconciler] BFL 更新成功记录失败: task_id=%s, err=%v", image.TaskId, dbErr)
		return
	}
	if !applied {
		logger.Infof(ctx, "[flux-reconciler] BFL 已被其他路径处理: task_id=%s", image.TaskId)
		return
	}
	logger.Infof(ctx, "[flux-reconciler] BFL 对账成功: task_id=%s, user_id=%d, quota=%d (创建时已扣费)",
		image.TaskId, image.UserId, image.Quota)
}

func applyFluxBFLFailed(ctx context.Context, image *model.Image, poll flux.FluxPollingResponse) {
	image.Status = flux.TaskStatusFailed
	image.FailReason = poll.Error
	if image.FailReason == "" {
		image.FailReason = fmt.Sprintf("BFL status: %s", poll.Status)
	}
	applied, dbErr := image.UpdateIfNotTerminal()
	if dbErr != nil {
		logger.Errorf(ctx, "[flux-reconciler] BFL 更新失败记录失败: task_id=%s, err=%v", image.TaskId, dbErr)
		return
	}
	if !applied {
		logger.Infof(ctx, "[flux-reconciler] BFL 失败标记跳过（已为终态）: task_id=%s", image.TaskId)
		return
	}
	logger.Infof(ctx, "[flux-reconciler] BFL 任务标记失败: task_id=%s, reason=%s", image.TaskId, image.FailReason)
}

// ─── Replicate ───────────────────────────────────────────────────────────────

func reconcileReplicateImage(ctx context.Context, image *model.Image, baseURL, apiKey string) {
	queryURL := fmt.Sprintf("%s/v1/predictions/%s", baseURL, image.TaskId)
	req, err := http.NewRequestWithContext(ctx, "GET", queryURL, nil)
	if err != nil {
		logger.Errorf(ctx, "[flux-reconciler] Replicate 创建请求失败: task_id=%s, err=%v", image.TaskId, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		logger.Errorf(ctx, "[flux-reconciler] Replicate 请求失败: task_id=%s, err=%v", image.TaskId, err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	logger.Infof(ctx, "[flux-reconciler] Replicate 查询结果: task_id=%s, status=%d, body=%s",
		image.TaskId, resp.StatusCode, string(body))

	var repl flux.ReplicateResponse
	if err := json.Unmarshal(body, &repl); err != nil {
		logger.Errorf(ctx, "[flux-reconciler] Replicate 解析响应失败: task_id=%s, err=%v", image.TaskId, err)
		return
	}

	switch repl.Status {
	case "succeeded":
		imageURL := string(repl.Output)
		if imageURL == "" {
			logger.Errorf(ctx, "[flux-reconciler] Replicate succeeded 但 output 为空: task_id=%s", image.TaskId)
			return
		}
		applyFluxReplicateSuccess(ctx, image, repl, imageURL)
	case "failed", "canceled":
		applyFluxReplicateFailed(ctx, image, repl)
	default:
		logger.Debugf(ctx, "[flux-reconciler] Replicate 未终态，跳过: task_id=%s, status=%s", image.TaskId, repl.Status)
	}
}

func applyFluxReplicateSuccess(ctx context.Context, image *model.Image, repl flux.ReplicateResponse, imageURL string) {
	queryResult := map[string]any{
		"id":     repl.ID,
		"status": "Ready",
		"result": map[string]any{"sample": imageURL},
	}
	resultBytes, _ := json.Marshal(queryResult)

	image.Status = flux.TaskStatusSucceed
	image.StoreUrl = imageURL
	image.Result = string(resultBytes)

	applied, dbErr := image.UpdateIfNotTerminal()
	if dbErr != nil {
		logger.Errorf(ctx, "[flux-reconciler] Replicate 更新成功记录失败: task_id=%s, err=%v", image.TaskId, dbErr)
		return
	}
	if !applied {
		logger.Infof(ctx, "[flux-reconciler] Replicate 已被其他路径处理: task_id=%s", image.TaskId)
		return
	}
	logger.Infof(ctx, "[flux-reconciler] Replicate 对账成功: task_id=%s, user_id=%d, quota=%d (创建时已扣费)",
		image.TaskId, image.UserId, image.Quota)
}

func applyFluxReplicateFailed(ctx context.Context, image *model.Image, repl flux.ReplicateResponse) {
	image.Status = flux.TaskStatusFailed
	image.FailReason = fmt.Sprintf("%v", repl.Error)
	if image.FailReason == "<nil>" || image.FailReason == "" {
		image.FailReason = fmt.Sprintf("Replicate task %s", repl.Status)
	}
	applied, dbErr := image.UpdateIfNotTerminal()
	if dbErr != nil {
		logger.Errorf(ctx, "[flux-reconciler] Replicate 更新失败记录失败: task_id=%s, err=%v", image.TaskId, dbErr)
		return
	}
	if !applied {
		logger.Infof(ctx, "[flux-reconciler] Replicate 失败标记跳过（已为终态）: task_id=%s", image.TaskId)
		return
	}
	logger.Infof(ctx, "[flux-reconciler] Replicate 任务标记失败: task_id=%s, reason=%s", image.TaskId, image.FailReason)
}
