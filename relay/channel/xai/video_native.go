package xai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	dbmodel "github.com/songquanpeng/one-api/model"
)

// NativeVideoParams is the parsed request body for xAI native video API.
type NativeVideoParams struct {
	Duration   int    `json:"duration"`
	Resolution string `json:"resolution"`
	Video      *struct {
		URL string `json:"url"`
	} `json:"video,omitempty"`
	Image *struct {
		URL string `json:"url"`
	} `json:"image,omitempty"`
}

// NativeVideoResponse wraps the upstream HTTP result for a POST request.
type NativeVideoResponse struct {
	StatusCode  int
	Body        []byte
	Header      http.Header
	RequestId   string // parsed from response JSON
	ParsedResp  GrokVideoResponse
}

// NativeVideoResultResponse wraps the upstream HTTP result for a GET request.
type NativeVideoResultResponse struct {
	StatusCode int
	Body       []byte
	Header     http.Header
}

// ParseNativeVideoParams parses the request body into NativeVideoParams.
func ParseNativeVideoParams(body []byte) *NativeVideoParams {
	var p NativeVideoParams
	_ = json.Unmarshal(body, &p)
	return &p
}

// CalculateNativeVideoQuota computes the pre-charge quota for a given endpoint.
func CalculateNativeVideoQuota(endpoint string, params *NativeVideoParams) int64 {
	outputPrice := 0.05
	if params.Resolution == "720p" {
		outputPrice = 0.07
	}

	switch endpoint {
	case "generations":
		duration := params.Duration
		if duration <= 0 {
			duration = 8
		}
		if duration > 15 {
			duration = 15
		}
		total := float64(duration) * outputPrice
		if params.Image != nil && params.Image.URL != "" {
			total += 0.002
		}
		return int64(total * config.QuotaPerUnit)

	case "edits":
		return int64(0.20 * config.QuotaPerUnit)

	case "extensions":
		duration := params.Duration
		if duration <= 0 {
			duration = 6
		}
		if duration > 10 {
			duration = 10
		}
		total := float64(duration) * outputPrice
		return int64(total * config.QuotaPerUnit)

	default:
		return int64(0.20 * config.QuotaPerUnit)
	}
}

// NativeDurationStr returns the duration string for video logging.
func NativeDurationStr(endpoint string, params *NativeVideoParams) string {
	switch endpoint {
	case "generations":
		d := params.Duration
		if d <= 0 {
			d = 8
		}
		return strconv.Itoa(d)
	case "extensions":
		d := params.Duration
		if d <= 0 {
			d = 6
		}
		return strconv.Itoa(d)
	default:
		return ""
	}
}

// SendNativeVideoRequest forwards a POST request to xAI video API.
func SendNativeVideoRequest(baseURL, apiKey, endpoint string, body []byte) (*NativeVideoResponse, error) {
	baseURL = strings.TrimSuffix(baseURL, "/")
	fullURL := fmt.Sprintf("%s/v1/videos/%s", baseURL, endpoint)

	httpReq, err := http.NewRequest(http.MethodPost, fullURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read upstream response: %w", err)
	}

	result := &NativeVideoResponse{
		StatusCode: resp.StatusCode,
		Body:       respBody,
		Header:     resp.Header,
	}

	if resp.StatusCode == 200 || resp.StatusCode == 202 {
		var grokResp GrokVideoResponse
		if json.Unmarshal(respBody, &grokResp) == nil {
			result.RequestId = grokResp.RequestId
			result.ParsedResp = grokResp
		}
	}

	return result, nil
}

// FetchNativeVideoResult queries the status of a video generation task.
func FetchNativeVideoResult(baseURL, apiKey, requestId string) (*NativeVideoResultResponse, error) {
	baseURL = strings.TrimSuffix(baseURL, "/")
	fullURL := fmt.Sprintf("%s/v1/videos/%s", baseURL, requestId)

	httpReq, err := http.NewRequest(http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read upstream response: %w", err)
	}

	return &NativeVideoResultResponse{
		StatusCode: resp.StatusCode,
		Body:       respBody,
		Header:     resp.Header,
	}, nil
}

// ResolveAPIKey picks the best API key from task credentials or channel key.
func ResolveAPIKey(task *dbmodel.Video, channel *dbmodel.Channel) string {
	if task.Credentials != "" {
		return task.Credentials
	}
	for _, k := range strings.Split(channel.Key, "\n") {
		k = strings.TrimSpace(k)
		if k != "" {
			return k
		}
	}
	return ""
}

// ResolveBaseURL returns the effective base URL.
func ResolveBaseURL(channel *dbmodel.Channel) string {
	u := channel.GetBaseURL()
	if u == "" {
		return "https://api.x.ai"
	}
	return u
}

// UpdateNativeVideoTaskStatus parses the GET response and updates the DB record.
// Uses Redis distributed lock per task to prevent duplicate quota operations in multi-instance deployments.
func UpdateNativeVideoTaskStatus(requestId string, body []byte, task *dbmodel.Video) {
	var result GrokVideoResult
	if err := json.Unmarshal(body, &result); err != nil {
		return
	}

	newStatus := mapXaiVideoStatus(result.Status)
	isTerminal := newStatus == "succeed" || newStatus == "failed" || newStatus == "expired"
	oldIsTerminal := task.Status == "succeed" || task.Status == "failed" || task.Status == "expired"
    // 如果任务已经结束，则直接返回
	if oldIsTerminal {
		return
	}
	if isTerminal && !oldIsTerminal {
		lockKey := fmt.Sprintf("lock:xai_video_task:%s", requestId)
		token := common.RedisLockAcquire(lockKey, 60*time.Second)
		if token == "" {
			logger.SysLog(fmt.Sprintf("[xAI Video] 状态更新锁竞争失败 - taskId=%s", requestId))
			return
		}
		defer common.RedisLockRelease(lockKey, token)

		fresh, err := dbmodel.GetVideoTaskById(requestId)
		if err != nil {
			logger.SysLog(fmt.Sprintf("[xAI Video] 重新读取任务失败 - taskId=%s, err=%v", requestId, err))
			return
		}
		if fresh.Status == "succeed" || fresh.Status == "failed" || fresh.Status == "expired" {
			return
		}
		task = fresh
	}

	task.Status = newStatus

	switch result.Status {
	case "done":
		if result.Video != nil && result.Video.URL != "" {
			task.StoreUrl = result.Video.URL
			if result.Video.Duration > 0 {
				task.Duration = strconv.Itoa(result.Video.Duration)
			}
		}
	case "failed":
		if result.GetError() != "" {
			task.FailReason = result.GetError()
		}
	}

	task.Result = string(body)
	task.TotalDuration = time.Now().Unix() - task.CreatedAt

	isNewTerminal := !oldIsTerminal && isTerminal

	if isNewTerminal && task.Status == "succeed" && task.Quota > 0 {
		adjustNativeVideoQuota(requestId, task, &result)
	}

	if err := task.Update(); err != nil {
		logger.SysLog(fmt.Sprintf("[xAI Video] 保存RESULT数据失败 - taskId=%s, err=%v", requestId, err))
	}

	if isNewTerminal && task.Status == "failed" && task.Quota > 0 {
		logger.SysLog(fmt.Sprintf("[xAI Video] 任务失败退款 - taskId=%s, quota=%d", requestId, task.Quota))
		CompensateNativeVideoTask(requestId)
	}
}

func mapXaiVideoStatus(xaiStatus string) string {
	switch xaiStatus {
	case "done":
		return "succeed"
	case "failed":
		return "failed"
	case "expired":
		return "expired"
	default:
		return "processing"
	}
}

// adjustNativeVideoQuota recalculates the actual cost on success and refunds or charges the difference.
func adjustNativeVideoQuota(requestId string, task *dbmodel.Video, result *GrokVideoResult) {
	actualQuota := calculateActualXaiVideoQuota(task, result)
	if actualQuota <= 0 {
		return
	}

	diff := task.Quota - actualQuota
	if diff == 0 {
		return
	}

	if diff > 0 {
		logger.SysLog(fmt.Sprintf("[xAI Video] 多退 - taskId=%s, pre=%d, actual=%d, refund=%d",
			requestId, task.Quota, actualQuota, diff))
		if err := dbmodel.IncreaseUserQuota(task.UserId, diff); err != nil {
			logger.SysLog(fmt.Sprintf("[xAI Video] 多退用户配额失败 - taskId=%s, err=%v", requestId, err))
			return
		}
		_ = dbmodel.CompensateChannelQuota(task.ChannelId, diff)
	} else {
		charge := -diff
		logger.SysLog(fmt.Sprintf("[xAI Video] 少补 - taskId=%s, pre=%d, actual=%d, charge=%d",
			requestId, task.Quota, actualQuota, charge))
		if err := dbmodel.DecreaseUserQuota(task.UserId, charge); err != nil {
			logger.SysLog(fmt.Sprintf("[xAI Video] 少补用户配额失败 - taskId=%s, err=%v", requestId, err))
			return
		}
		dbmodel.UpdateChannelUsedQuota(task.ChannelId, charge)
	}

	task.Quota = actualQuota
}

// calculateActualXaiVideoQuota computes the real quota from upstream usage.cost_in_usd_ticks.
// xAI defines: 1 USD = 10,000,000,000 ticks.
func calculateActualXaiVideoQuota(task *dbmodel.Video, result *GrokVideoResult) int64 {
	if result.Usage == nil || result.Usage.CostInUsdTicks <= 0 {
		return task.Quota
	}
	const ticksPerUSD = 10_000_000_000.0
	actualQuota := int64(float64(result.Usage.CostInUsdTicks) / ticksPerUSD * config.QuotaPerUnit)
	if actualQuota <= 0 {
		return task.Quota
	}
	return actualQuota
}

// CompensateNativeVideoTask refunds quota for a failed video task.
func CompensateNativeVideoTask(taskId string) {
	task, err := dbmodel.GetVideoTaskById(taskId)
	if err != nil || task.Quota <= 0 {
		return
	}

	if err := dbmodel.CompensateVideoTaskQuota(task.UserId, task.Quota); err != nil {
		logger.SysLog(fmt.Sprintf("[xAI Video] 补偿用户配额失败 - taskId=%s, err=%v", taskId, err))
		return
	}
	_ = dbmodel.CompensateChannelQuota(task.ChannelId, task.Quota)
}
