package vertexai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"

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

func (a *VideoAdaptor) GetProviderName() string { return "vertexai" }
func (a *VideoAdaptor) GetChannelName() string  { return "Vertex AI Veo" }
func (a *VideoAdaptor) GetSupportedModels() []string {
	return []string{"veo-2.0-generate-001", "veo-3.0-generate-preview", "veo-3.0-fast-generate-preview"}
}
func (a *VideoAdaptor) GetPrePaymentQuota() int64 {
	return int64(6.0 * config.QuotaPerUnit)
}

func (a *VideoAdaptor) HandleVideoRequest(c *gin.Context, req *model.VideoRequest, meta *util.RelayMeta) (*relaychannel.VideoTaskResult, *model.ErrorWithStatusCode) {
	// 获取渠道信息
	channel, err := dbmodel.GetChannelById(meta.ChannelId, true)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "get_channel_error", http.StatusInternalServerError)
	}

	credentials, err := GetCredentialsFromConfig(meta.Config, channel)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "invalid_credentials", http.StatusInternalServerError)
	}

	projectID := credentials.ProjectID
	if projectID == "" {
		return nil, openaiAdaptor.ErrorWrapper(
			fmt.Errorf("unable to get Vertex AI project ID, please check JSON credentials in Key field"),
			"invalid_project_id", http.StatusBadRequest)
	}

	region := meta.Config.Region
	var fullRequestUrl string
	if region == "global" || region == "" {
		fullRequestUrl = fmt.Sprintf(
			"https://aiplatform.googleapis.com/v1/projects/%s/locations/global/publishers/google/models/%s:predictLongRunning",
			projectID, meta.OriginModelName)
	} else {
		fullRequestUrl = fmt.Sprintf(
			"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:predictLongRunning",
			region, projectID, region, meta.OriginModelName)
	}

	log.Printf("veo-full-request-url: %s", fullRequestUrl)

	// 解析请求体
	var reqBody map[string]interface{}
	if err := json.NewDecoder(c.Request.Body).Decode(&reqBody); err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "invalid_request_body", http.StatusBadRequest)
	}

	// 删除model参数
	delete(reqBody, "model")

	// 读取parameters字段
	params, _ := reqBody["parameters"].(map[string]interface{})
	if params == nil {
		params = make(map[string]interface{})
	}

	// 读取generateAudio（默认true）
	generateAudio := true
	if val, ok := params["generateAudio"].(bool); ok {
		generateAudio = val
	}

	// 读取durationSeconds（默认8）
	durationSeconds := 8
	if val, ok := params["durationSeconds"].(float64); ok {
		durationSeconds = int(val)
	}

	// 添加storageUri参数（从渠道配置中读取）
	if meta.Config.GoogleStorage != "" {
		params["storageUri"] = meta.Config.GoogleStorage
	}
	reqBody["parameters"] = params

	// 序列化请求体
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "json_marshal_error", http.StatusInternalServerError)
	}

	// 获取访问令牌
	adaptor := &Adaptor{
		AccountCredentials: *credentials,
	}
	accessToken, err := GetAccessToken(adaptor, meta)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "get_access_token_error", http.StatusInternalServerError)
	}

	// 发送请求
	httpReq, err := http.NewRequest("POST", fullRequestUrl, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "create_request_error", http.StatusInternalServerError)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "request_error", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(err, "read_response_error", http.StatusInternalServerError)
	}

	var veoResponse map[string]interface{}
	if err := json.Unmarshal(body, &veoResponse); err != nil {
		log.Printf("[VEO] Failed to parse response JSON: %v", err)
		return nil, openaiAdaptor.ErrorWrapper(err, "response_parse_error", http.StatusInternalServerError)
	}

	if resp.StatusCode == 200 {
		// 提取 taskId（取操作名称最后一部分）
		var taskId string
		if name, ok := veoResponse["name"].(string); ok {
			parts := strings.Split(name, "/")
			if len(parts) > 0 {
				taskId = parts[len(parts)-1]
			} else {
				taskId = name
			}
		}

		// 根据generateAudio设置videoMode
		var videoMode string
		if generateAudio {
			videoMode = "AudioVideo"
		} else {
			videoMode = "NoAudioVideo"
		}

		// 计算配额
		quota := common.CalculateVideoQuota(meta.OriginModelName, "", videoMode, strconv.Itoa(durationSeconds), "", "")

		return &relaychannel.VideoTaskResult{
			TaskId:     taskId,
			TaskStatus: "succeed",
			Duration:   strconv.Itoa(durationSeconds),
			Mode:       videoMode,
			Quota:      quota,
		}, nil
	}

	// 处理错误响应
	errorMsg := "Unknown error"
	if errObj, ok := veoResponse["error"].(map[string]interface{}); ok {
		if message, ok := errObj["message"].(string); ok {
			errorMsg = message
		}
	}
	return nil, openaiAdaptor.ErrorWrapper(
		fmt.Errorf("VEO API error: %s", errorMsg),
		"api_error", resp.StatusCode)
}

func (a *VideoAdaptor) HandleVideoResult(c *gin.Context, videoTask *dbmodel.Video, ch *dbmodel.Channel, cfg *dbmodel.ChannelConfig) (*model.GeneralFinalVideoResponse, *model.ErrorWithStatusCode) {
	taskId := videoTask.TaskId

	// 如果数据库中已有缓存的URL，直接返回
	if videoTask.StoreUrl != "" {
		log.Printf("Found existing store URL for task %s: %s", taskId, videoTask.StoreUrl)

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
			VideoResult:  videoUrls[0],
			VideoId:      taskId,
			TaskStatus:   "succeed",
			Message:      "Video retrieved from cache",
			VideoResults: videoResults,
			Duration:     videoTask.Duration,
		}, nil
	}

	// 加载凭证
	var credentials *Credentials
	if videoTask.Credentials != "" {
		credentials = &Credentials{}
		if err := json.Unmarshal([]byte(videoTask.Credentials), credentials); err != nil {
			log.Printf("[VEO查询] ❌ 解析保存的凭证失败 - 任务:%s, 错误:%v", taskId, err)
			return nil, openaiAdaptor.ErrorWrapper(
				fmt.Errorf("failed to parse saved Vertex AI credentials: %v", err),
				"invalid_saved_credentials", http.StatusInternalServerError)
		}
		log.Printf("[VEO查询] ✅ 使用保存的凭证 - 任务:%s, 项目ID:%s", taskId, credentials.ProjectID)
	} else {
		log.Printf("[VEO查询] ⚠️  任务未保存凭证，回退到当前渠道配置 - 任务:%s", taskId)
		var err error
		credentials, err = GetCredentialsFromConfig(*cfg, ch)
		if err != nil {
			return nil, openaiAdaptor.ErrorWrapper(
				fmt.Errorf("failed to get Vertex AI credentials: %v", err),
				"invalid_credentials", http.StatusInternalServerError)
		}
	}

	projectId := credentials.ProjectID
	if projectId == "" {
		return nil, openaiAdaptor.ErrorWrapper(
			fmt.Errorf("unable to get Vertex AI project ID, please check credential configuration"),
			"invalid_project_id", http.StatusInternalServerError)
	}

	region := cfg.Region
	if region == "" {
		region = "global"
	}
	modelId := videoTask.Model

	// 构建 fetchPredictOperation URL
	var baseURL string
	if region == "global" {
		baseURL = "https://aiplatform.googleapis.com"
	} else {
		baseURL = fmt.Sprintf("https://%s-aiplatform.googleapis.com", region)
	}
	fullRequestUrl := fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:fetchPredictOperation",
		baseURL, projectId, region, modelId)

	// 构建完整的操作名称
	fullOperationName := fmt.Sprintf("projects/%s/locations/%s/publishers/google/models/%s/operations/%s",
		projectId, region, modelId, taskId)

	// 获取访问令牌
	adaptor := &Adaptor{
		AccountCredentials: *credentials,
	}
	tempMeta := &util.RelayMeta{
		ChannelId: ch.Id,
		Config: dbmodel.ChannelConfig{
			Region:            cfg.Region,
			VertexAIProjectID: credentials.ProjectID,
		},
		ActualAPIKey: func() string {
			if credBytes, err := json.Marshal(credentials); err == nil {
				return string(credBytes)
			}
			return ""
		}(),
		IsMultiKey: false,
	}

	accessToken, err := GetAccessToken(adaptor, tempMeta)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(
			fmt.Errorf("failed to get VertexAI access token: %v", err),
			"auth_error", http.StatusInternalServerError)
	}

	// POST 请求
	requestBody := map[string]string{"operationName": fullOperationName}
	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(
			fmt.Errorf("failed to marshal request body: %v", err),
			"marshal_error", http.StatusInternalServerError)
	}

	httpReq, err := http.NewRequest("POST", fullRequestUrl, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(
			fmt.Errorf("failed to create request: %v", err),
			"api_error", http.StatusInternalServerError)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+accessToken)

	httpClient := &http.Client{}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(
			fmt.Errorf("failed to do request: %v", err),
			"request_error", http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, openaiAdaptor.ErrorWrapper(
			fmt.Errorf("failed to read response body: %v", err),
			"internal_error", http.StatusInternalServerError)
	}

	var veoResp map[string]interface{}
	if err := json.Unmarshal(body, &veoResp); err != nil {
		log.Printf("Failed to parse Vertex AI response as JSON. Body: %s", string(body))
		return nil, openaiAdaptor.ErrorWrapper(
			fmt.Errorf("failed to parse response JSON: %v", err),
			"json_parse_error", http.StatusInternalServerError)
	}

	log.Printf("=== [VEO查询] 完整响应体 for task %s ===", taskId)
	responseBodyStr := string(body)
	if len(responseBodyStr) > 2000 {
		log.Printf("原始响应体 (truncated): %s...%s",
			responseBodyStr[:1000], responseBodyStr[len(responseBodyStr)-1000:])
		log.Printf("响应体长度: %d characters", len(responseBodyStr))
	} else {
		log.Printf("原始响应体: %s", responseBodyStr)
	}

	log.Printf("=== [VEO查询] 响应体结构分析 ===")
	printVeoJSONStructure(veoResp, "", 4)
	log.Printf("=== [VEO查询] 响应体分析结束 ===")

	generalResponse := &model.GeneralFinalVideoResponse{
		TaskId:     taskId,
		VideoId:    taskId,
		TaskStatus: "processing",
		Message:    "Operation in progress",
		Duration:   videoTask.Duration,
	}

	if done, ok := veoResp["done"].(bool); ok && done {
		if errorInfo, ok := veoResp["error"].(map[string]interface{}); ok {
			generalResponse.TaskStatus = "failed"
			if message, ok := errorInfo["message"].(string); ok {
				generalResponse.Message = message
			} else {
				generalResponse.Message = "Operation failed with an unknown error."
			}
		} else if response, ok := veoResp["response"].(map[string]interface{}); ok {
			if raiFilteredCount, hasFiltered := response["raiMediaFilteredCount"]; hasFiltered {
				if filteredCount, ok := raiFilteredCount.(float64); ok && filteredCount > 0 {
					generalResponse.TaskStatus = "failed"
					var filterReasons []string
					if reasons, hasReasons := response["raiMediaFilteredReasons"].([]interface{}); hasReasons {
						for _, reason := range reasons {
							if reasonStr, ok := reason.(string); ok {
								filterReasons = append(filterReasons, reasonStr)
							}
						}
					}
					if len(filterReasons) > 0 {
						generalResponse.Message = strings.Join(filterReasons, "; ")
					} else {
						generalResponse.Message = fmt.Sprintf("Content filtered (count: %.0f)", filteredCount)
					}
				} else {
					videoURIs := extractVeoVideoURIs(response)
					if len(videoURIs) > 0 {
						processedURIs := processVeoVideos(c, videoURIs, videoTask.UserId)
						generalResponse.TaskStatus = "succeed"
						generalResponse.Message = "Video generated successfully."
						generalResponse.VideoResult = buildStoreUrl(processedURIs)
						generalResponse.VideoResults = make([]model.VideoResultItem, len(processedURIs))
						for i, uri := range processedURIs {
							generalResponse.VideoResults[i] = model.VideoResultItem{Url: uri}
						}
					} else {
						generalResponse.TaskStatus = "failed"
						generalResponse.Message = "Operation completed, but no video result was found."
					}
				}
			} else {
				videoURIs := extractVeoVideoURIs(response)
				if len(videoURIs) > 0 {
					processedURIs := processVeoVideos(c, videoURIs, videoTask.UserId)
					generalResponse.TaskStatus = "succeed"
					generalResponse.Message = "Video generated successfully."
					generalResponse.VideoResult = buildStoreUrl(processedURIs)
					generalResponse.VideoResults = make([]model.VideoResultItem, len(processedURIs))
					for i, uri := range processedURIs {
						generalResponse.VideoResults[i] = model.VideoResultItem{Url: uri}
					}
				} else {
					generalResponse.TaskStatus = "failed"
					generalResponse.Message = "Operation completed, but no video result was found."
				}
			}
		} else {
			generalResponse.TaskStatus = "failed"
			generalResponse.Message = "Operation completed with an invalid response format."
		}
	}

	return generalResponse, nil
}

// buildStoreUrl 将视频URL列表编码为适合存入数据库的格式
// 单个URL直接存储，多个URL存储为JSON数组
func buildStoreUrl(uris []string) string {
	if len(uris) == 0 {
		return ""
	}
	if len(uris) == 1 {
		return uris[0]
	}
	b, _ := json.Marshal(uris)
	return string(b)
}

// extractVeoVideoURIs 从 Vertex AI Veo 操作响应中提取所有视频URI或base64数据
func extractVeoVideoURIs(response map[string]interface{}) []string {
	var videoURIs []string

	log.Printf("[VEO视频提取] 开始解析响应中的视频URI")
	log.Printf("[VEO视频提取] 响应中的顶级字段: %+v", func() []string {
		keys := make([]string, 0, len(response))
		for k := range response {
			keys = append(keys, k)
		}
		return keys
	}())

	// 检查 fetchPredictOperation 格式 (`videos` 字段)
	if videos, ok := response["videos"].([]interface{}); ok && len(videos) > 0 {
		log.Printf("[VEO视频提取] 找到videos字段，包含 %d 个视频", len(videos))
		for i, videoInterface := range videos {
			if video, ok := videoInterface.(map[string]interface{}); ok {
				log.Printf("[VEO视频提取] 视频 %d 的字段: %+v", i, func() []string {
					keys := make([]string, 0, len(video))
					for k := range video {
						keys = append(keys, k)
					}
					return keys
				}())

				if gcsUri, ok := video["gcsUri"].(string); ok && gcsUri != "" {
					log.Printf("[VEO视频提取] ✅ 找到GCS URI: %s", gcsUri)
					httpsUrl := convertGCStoHTTPS(gcsUri)
					videoURIs = append(videoURIs, httpsUrl)
					continue
				}
				if bytesBase64, ok := video["bytesBase64Encoded"].(string); ok && bytesBase64 != "" {
					log.Printf("[VEO视频提取] ✅ 找到base64数据，长度: %d", len(bytesBase64))
					videoURIs = append(videoURIs, "data:video/mp4;base64,"+bytesBase64)
				}
			} else {
				log.Printf("[VEO视频提取] ⚠️  视频 %d 不是map格式: %T", i, videoInterface)
			}
		}
	} else {
		log.Printf("[VEO视频提取] ❌ 未找到videos字段或为空")
	}

	// 检查标准长轮询操作格式 (`generatedSamples` 字段)
	if generatedSamples, ok := response["generatedSamples"].([]interface{}); ok && len(generatedSamples) > 0 {
		log.Printf("[VEO视频提取] 找到generatedSamples字段，包含 %d 个样本", len(generatedSamples))
		for i, sampleInterface := range generatedSamples {
			if sample, ok := sampleInterface.(map[string]interface{}); ok {
				if video, ok := sample["video"].(map[string]interface{}); ok {
					log.Printf("[VEO视频提取] 样本 %d 的video字段: %+v", i, func() []string {
						keys := make([]string, 0, len(video))
						for k := range video {
							keys = append(keys, k)
						}
						return keys
					}())

					if uri, ok := video["uri"].(string); ok && uri != "" {
						log.Printf("[VEO视频提取] ✅ 找到URI: %s", uri)
						httpsUrl := convertGCStoHTTPS(uri)
						videoURIs = append(videoURIs, httpsUrl)
						continue
					}
					if bytesBase64, ok := video["bytesBase64Encoded"].(string); ok && bytesBase64 != "" {
						log.Printf("[VEO视频提取] ✅ 找到base64数据，长度: %d", len(bytesBase64))
						videoURIs = append(videoURIs, "data:video/mp4;base64,"+bytesBase64)
					}
				} else {
					log.Printf("[VEO视频提取] ⚠️  样本 %d 中未找到video字段", i)
				}
			} else {
				log.Printf("[VEO视频提取] ⚠️  样本 %d 不是map格式: %T", i, sampleInterface)
			}
		}
	} else {
		log.Printf("[VEO视频提取] ❌ 未找到generatedSamples字段或为空")
	}

	log.Printf("[VEO视频提取] 最终提取到 %d 个视频URI", len(videoURIs))
	return videoURIs
}

// convertGCStoHTTPS 将 gs:// 格式的 URI 转换为 https://storage.googleapis.com/ 格式
func convertGCStoHTTPS(gcsUri string) string {
	if strings.HasPrefix(gcsUri, "gs://") {
		httpsUrl := strings.Replace(gcsUri, "gs://", "https://storage.googleapis.com/", 1)
		log.Printf("[VEO URL转换] GCS URI: %s -> HTTPS URL: %s", gcsUri, httpsUrl)
		return httpsUrl
	}
	return gcsUri
}

// printVeoJSONStructure 打印JSON结构，但不显示具体内容（避免base64数据过长）
func printVeoJSONStructure(data interface{}, prefix string, maxDepth int) {
	if maxDepth <= 0 {
		return
	}

	switch v := data.(type) {
	case map[string]interface{}:
		log.Printf("%s{", prefix)
		for key, value := range v {
			switch v := value.(type) {
			case string:
				if len(v) > 100 {
					log.Printf("%s  \"%s\": \"<string length: %d>\"", prefix, key, len(v))
				} else {
					log.Printf("%s  \"%s\": \"%s\"", prefix, key, v)
				}
			case bool:
				log.Printf("%s  \"%s\": %v", prefix, key, value)
			case float64:
				log.Printf("%s  \"%s\": %v", prefix, key, value)
			case []interface{}:
				log.Printf("%s  \"%s\": [", prefix, key)
				if len(value.([]interface{})) > 0 {
					printVeoJSONStructure(value.([]interface{})[0], prefix+"    ", maxDepth-1)
					if len(value.([]interface{})) > 1 {
						log.Printf("%s    ... (%d more items)", prefix, len(value.([]interface{}))-1)
					}
				}
				log.Printf("%s  ]", prefix)
			case map[string]interface{}:
				log.Printf("%s  \"%s\":", prefix, key)
				printVeoJSONStructure(value, prefix+"    ", maxDepth-1)
			case nil:
				log.Printf("%s  \"%s\": null", prefix, key)
			default:
				log.Printf("%s  \"%s\": <%T>", prefix, key, value)
			}
		}
		log.Printf("%s}", prefix)
	case []interface{}:
		log.Printf("%s[", prefix)
		if len(v) > 0 {
			printVeoJSONStructure(v[0], prefix+"  ", maxDepth-1)
			if len(v) > 1 {
				log.Printf("%s  ... (%d more items)", prefix, len(v)-1)
			}
		}
		log.Printf("%s]", prefix)
	default:
		log.Printf("%s<%T>", prefix, v)
	}
}

// processVeoVideos 并发处理视频URI：base64数据上传到R2，其他URI直接使用
func processVeoVideos(c *gin.Context, videoURIs []string, userId int) []string {
	type uploadResult struct {
		index int
		url   string
	}

	responseFormat := c.GetString("response_format")
	results := make([]uploadResult, len(videoURIs))
	var wg sync.WaitGroup

	mu := sync.Mutex{}
	for i, uri := range videoURIs {
		wg.Add(1)
		go func(index int, videoURI string) {
			defer wg.Done()
			var finalURL string
			if responseFormat == "url" && strings.HasPrefix(videoURI, "data:video/mp4;base64,") {
				base64Data := strings.TrimPrefix(videoURI, "data:video/mp4;base64,")
				uploaded, uploadErr := relaychannel.UploadVideoBase64ToR2(base64Data, userId, "mp4")
				if uploadErr != nil {
					log.Printf("[VEO] Failed to upload video %d to R2: %v", index, uploadErr)
					finalURL = videoURI // 上传失败保留原始base64
				} else {
					finalURL = uploaded
				}
			} else {
				finalURL = videoURI
			}
			mu.Lock()
			results[index] = uploadResult{index: index, url: finalURL}
			mu.Unlock()
		}(i, uri)
	}
	wg.Wait()

	processedURIs := make([]string, len(videoURIs))
	for _, r := range results {
		processedURIs[r.index] = r.url
	}
	return processedURIs
}
