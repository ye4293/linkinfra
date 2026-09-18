package doubao

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	dbmodel "github.com/songquanpeng/one-api/model"
	relaychannel "github.com/songquanpeng/one-api/relay/channel"
	openaiAdaptor "github.com/songquanpeng/one-api/relay/channel/openai"
	"github.com/songquanpeng/one-api/relay/model"
	"github.com/songquanpeng/one-api/relay/util"
)

type VideoAdaptor struct {
	relaychannel.BaseVideoAdaptor
}

func (a *VideoAdaptor) GetProviderName() string { return "doubao" }
func (a *VideoAdaptor) GetChannelName() string  { return "豆包" }
func (a *VideoAdaptor) GetSupportedModels() []string {
	return []string{"doubao-seedance-1-0-lite", "doubao-seedance-1-0-pro", "doubao-seaweed"}
}

func (a *VideoAdaptor) HandleVideoRequest(c *gin.Context, req *model.VideoRequest, meta *util.RelayMeta) (*relaychannel.VideoTaskResult, *model.ErrorWithStatusCode) {
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "read_request_body_failed", http.StatusBadRequest)
	}

	var doubaoRequest DoubaoVideoRequest
	if err := json.Unmarshal(bodyBytes, &doubaoRequest); err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "parse_doubao_request_failed", http.StatusBadRequest)
	}
	log.Printf("doubao-request-data: %+v", doubaoRequest)

	if doubaoRequest.Model == "" {
		return nil, openaiAdaptor.ErrorWrapper(fmt.Errorf("model is required"), "invalid_request_error", http.StatusBadRequest)
	}
	if len(doubaoRequest.Content) == 0 {
		return nil, openaiAdaptor.ErrorWrapper(fmt.Errorf("content is required"), "invalid_request_error", http.StatusBadRequest)
	}

	ch, err := dbmodel.GetChannelById(meta.ChannelId, true)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "get_channel_error", http.StatusInternalServerError)
	}

	baseUrl := meta.BaseURL
	if baseUrl == "" {
		baseUrl = *ch.BaseURL
	}
	fullRequestUrl := baseUrl + "/api/v3/contents/generations/tasks"
	log.Printf("doubao fullRequestUrl: %s", fullRequestUrl)

	jsonData, err := json.Marshal(doubaoRequest)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "marshal_request_failed", http.StatusInternalServerError)
	}

	// Pre-payment: convert 1.4 CNY to USD
	prePayCNY := 1.4
	prePayUSD, exchangeErr := convertCNYToUSD(prePayCNY)
	if exchangeErr != nil {
		log.Printf("Failed to get exchange rate for Doubao pre-payment: %v, using fallback rate 7.2", exchangeErr)
		prePayUSD = prePayCNY / 7.2
	}
	quota := int64(prePayUSD * config.QuotaPerUnit)
	log.Printf("Doubao pre-payment: cny=%.2f, usd=%.6f, quota=%d", prePayCNY, prePayUSD, quota)

	httpReq, err := http.NewRequest(http.MethodPost, fullRequestUrl, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "create_request_error", http.StatusInternalServerError)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+ch.Key)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "request_error", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "read_response_error", http.StatusInternalServerError)
	}

	log.Printf("doubao-full-response-body: %s", string(body))
	log.Printf("doubao-response-status-code: %d", resp.StatusCode)

	var doubaoResponse DoubaoVideoResponse
	if err := json.Unmarshal(body, &doubaoResponse); err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "parse_response_error", http.StatusInternalServerError)
	}
	doubaoResponse.StatusCode = resp.StatusCode

	if doubaoResponse.StatusCode != 200 {
		errorMsg := "豆包API错误"
		if doubaoResponse.Error != nil && doubaoResponse.Error.Message != "" {
			errorMsg = doubaoResponse.Error.Message
		}
		return nil, openaiAdaptor.ErrorWrapper(fmt.Errorf("%s", errorMsg), "api_error", doubaoResponse.StatusCode)
	}

	return &relaychannel.VideoTaskResult{
		TaskId:     doubaoResponse.ID,
		TaskStatus: "succeed",
		Quota:      quota,
	}, nil
}

func (a *VideoAdaptor) HandleVideoResult(c *gin.Context, videoTask *dbmodel.Video, ch *dbmodel.Channel, cfg *dbmodel.ChannelConfig) (*model.GeneralFinalVideoResponse, *model.ErrorWithStatusCode) {
	taskId := videoTask.TaskId
	url := fmt.Sprintf("%s/api/v3/contents/generations/tasks/%s", *ch.BaseURL, taskId)

	_, body, err := relaychannel.SendVideoResultQuery(url, map[string]string{
		"Authorization": "Bearer " + ch.Key,
	})
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "request_error", http.StatusInternalServerError)
	}

	var doubaoResp DoubaoVideoResult
	if err := json.Unmarshal(body, &doubaoResp); err != nil {
		log.Printf("Failed to parse doubao response: %v, body: %s", err, string(body))
		return nil, openaiAdaptor.ErrorWrapper(err, "json_parse_error", http.StatusInternalServerError)
	}

	generalResponse := &model.GeneralFinalVideoResponse{
		TaskId:   doubaoResp.ID,
		VideoId:  doubaoResp.ID,
		Duration: videoTask.Duration,
	}

	switch doubaoResp.Status {
	case "queued", "running":
		generalResponse.TaskStatus = "processing"
	case "succeeded":
		generalResponse.TaskStatus = "succeeded"
		if doubaoResp.Content != nil && doubaoResp.Content.VideoURL != "" {
			generalResponse.VideoResult = doubaoResp.Content.VideoURL
			generalResponse.VideoResults = []model.VideoResultItem{{Url: doubaoResp.Content.VideoURL}}
		}
		// Adjust quota based on actual token usage
		if doubaoResp.Usage != nil && doubaoResp.Usage.TotalTokens > 0 {
			actualQuota := int64(float64(calculateQuotaForDoubao(doubaoResp.Model, int64(doubaoResp.Usage.TotalTokens))) * common.GetModelDiscount(videoTask.Model))
			preQuota := videoTask.Quota
			quotaDiff := int64(actualQuota - preQuota)
			if quotaDiff != 0 {
				if quotaErr := dbmodel.PostConsumeTokenQuota(c.GetInt("token_id"), quotaDiff); quotaErr != nil {
					log.Printf("Error consuming token quota diff: %v", quotaErr)
				}
				ctx := c.Request.Context()
				if cacheErr := dbmodel.CacheUpdateUserQuota(ctx, videoTask.UserId); cacheErr != nil {
					log.Printf("Error update user quota cache: %v", cacheErr)
				}
				dbmodel.UpdateUserUsedQuotaAndRequestCount(videoTask.UserId, quotaDiff)
				dbmodel.UpdateChannelUsedQuota(videoTask.ChannelId, quotaDiff)
			}
			if updateErr := dbmodel.UpdateLogQuotaAndTokens(doubaoResp.ID, int64(actualQuota), doubaoResp.Usage.TotalTokens); updateErr != nil {
				log.Printf("Failed to update log quota and tokens for task %s: %v", doubaoResp.ID, updateErr)
			} else {
				log.Printf("Successfully updated log for task %s: quota=%d, completion_tokens=%d", doubaoResp.ID, actualQuota, doubaoResp.Usage.TotalTokens)
			}
		}
	case "failed":
		generalResponse.TaskStatus = "failed"
		if doubaoResp.Error != nil {
			generalResponse.Message = doubaoResp.Error.Message
		}
	default:
		generalResponse.TaskStatus = "unknown"
	}

	return generalResponse, nil
}

// calculateQuotaForDoubao computes quota from token usage and model pricing (CNY-based)
func calculateQuotaForDoubao(modelName string, tokens int64) int64 {
	var basePriceCNY float64
	switch {
	case strings.Contains(modelName, "doubao-seedance-1-0-lite"):
		basePriceCNY = 10 / 1000000.0
	case strings.Contains(modelName, "doubao-seedance-1-0-pro"):
		basePriceCNY = 15 / 1000000.0
	case strings.Contains(modelName, "doubao-seaweed"):
		basePriceCNY = 30 / 1000000.0
	default:
		basePriceCNY = 50 / 1000000.0
	}
	cnyAmount := basePriceCNY * float64(tokens)
	usdAmount, exchangeErr := convertCNYToUSD(cnyAmount)
	if exchangeErr != nil {
		log.Printf("Failed to get exchange rate for Doubao pricing: %v, using fallback rate 7.2", exchangeErr)
		usdAmount = cnyAmount / 7.2
	}
	quota := int64(usdAmount * config.QuotaPerUnit)
	log.Printf("Doubao pricing calculation: model=%s, tokens=%d, cny=%.6f, usd=%.6f, quota=%d",
		modelName, tokens, cnyAmount, usdAmount, quota)
	return quota
}

// --- Exchange rate helpers (moved from video.go) ---

type exchangeRateManager struct {
	mu            sync.Mutex
	cnyToUSDRate  float64
	lastUpdate    time.Time
	cacheDuration time.Duration
}

var defaultExchangeManager = &exchangeRateManager{
	cacheDuration: 10 * time.Minute,
}

func convertCNYToUSD(cnyAmount float64) (float64, error) {
	rate, err := defaultExchangeManager.getCNYToUSDRate()
	if err != nil {
		return 0, err
	}
	usdAmount := cnyAmount * rate
	log.Printf("Converted %.6f CNY to %.6f USD (rate: %.6f)", cnyAmount, usdAmount, rate)
	return usdAmount, nil
}

func (e *exchangeRateManager) getCNYToUSDRate() (float64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if time.Since(e.lastUpdate) < e.cacheDuration && e.cnyToUSDRate > 0 {
		return e.cnyToUSDRate, nil
	}
	rate, err := fetchRateFromExchangeRateAPI()
	if err != nil {
		log.Printf("ExchangeRate-API failed: %v, using fallback rate 0.14", err)
		rate = 0.14
	}
	e.cnyToUSDRate = rate
	e.lastUpdate = time.Now()
	log.Printf("Updated exchange rate: %.6f CNY to USD", rate)
	return rate, nil
}

type exchangeRateResponse struct {
	ConversionRates map[string]float64 `json:"conversion_rates"`
}

func fetchRateFromExchangeRateAPI() (float64, error) {
	url := "https://api.exchangerate-api.com/v4/latest/CNY"
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch from ExchangeRate-API: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("ExchangeRate-API returned status code: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("failed to read response body: %v", err)
	}
	var data exchangeRateResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return 0, fmt.Errorf("failed to parse JSON response: %v", err)
	}
	usdRate, exists := data.ConversionRates["USD"]
	if !exists {
		return 0, fmt.Errorf("USD rate not found in response")
	}
	return usdRate, nil
}
