package channel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/util"
)

const defaultPingInterval = 10 * time.Second

func SetupCommonRequestHeader(c *gin.Context, req *http.Request, meta *util.RelayMeta) {
	req.Header.Set("Content-Type", c.Request.Header.Get("Content-Type"))
	req.Header.Set("Accept", c.Request.Header.Get("Accept"))
	if meta.IsStream && c.Request.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "text/event-stream")
	}
}

func DoRequestHelper(a Adaptor, c *gin.Context, meta *util.RelayMeta, requestBody io.Reader) (*http.Response, error) {
	fullRequestURL, err := a.GetRequestURL(meta)
	if err != nil {
		return nil, fmt.Errorf("get request url failed: %w", err)
	}
	// 不绑定客户端 context，避免客户端断开连接时取消正在进行的上游请求
	// 超时由 HTTPClient.Timeout（RELAY_TIMEOUT）统一控制
	req, err := http.NewRequest(c.Request.Method, fullRequestURL, requestBody)
	if err != nil {
		return nil, fmt.Errorf("new request failed: %w", err)
	}
	// Responses 流取消后不会继续交付结果，等待响应头阶段也应及时停止上游。
	if meta.IsStream && meta.Mode == constant.RelayModeOpenaiResponse {
		req = req.WithContext(c.Request.Context())
	}
	err = a.SetupRequestHeader(c, req, meta)
	if err != nil {
		return nil, fmt.Errorf("setup request header failed: %w", err)
	}
	// 应用渠道自定义请求头覆盖（优先级最高，覆盖用户传递和默认的header）
	ApplyHeadersOverride(req, meta)

	resp, err := DoRequest(c, req, meta)
	if err != nil {
		return nil, fmt.Errorf("do request failed: %w", err)
	}
	return resp, nil
}

// ApplyHeadersOverride 应用渠道自定义请求头覆盖
// 支持变量替换: {api_key} 会被替换为实际的 API Key
// 渠道配置的 header 会覆盖用户传递的和默认的 header（优先级最高）
func ApplyHeadersOverride(req *http.Request, meta *util.RelayMeta) {
	if len(meta.HeadersOverride) == 0 {
		return
	}

	for key, value := range meta.HeadersOverride {
		// 支持变量替换
		processedValue := value
		if strings.Contains(processedValue, "{api_key}") {
			// 优先使用 ActualAPIKey，如果为空则使用 APIKey
			apiKey := meta.ActualAPIKey
			if apiKey == "" {
				apiKey = meta.APIKey
			}
			processedValue = strings.ReplaceAll(processedValue, "{api_key}", apiKey)
		}
		// 设置请求头（覆盖已有的）
		req.Header.Set(key, processedValue)
	}
}

func DoRequest(c *gin.Context, req *http.Request, meta *util.RelayMeta) (*http.Response, error) {
	// 确保请求体被关闭
	defer func() {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		if c.Request.Body != nil {
			_ = c.Request.Body.Close()
		}
	}()

	// 流式请求：在等待上游响应期间发送 ping 保活，防止中间代理层断开连接
	var stopPinger func()
	if meta != nil && meta.IsStream && config.PingIntervalEnabled && !meta.DisablePing {
		common.SetEventStreamHeaders(c)
		pingInterval := time.Duration(config.PingIntervalSeconds) * time.Second
		if pingInterval <= 0 {
			pingInterval = defaultPingInterval
		}
		stopPinger = startPingKeepAlive(c, pingInterval)
		defer func() {
			if stopPinger != nil {
				stopPinger()
			}
		}()
	}

	resp, err := util.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("resp is nil")
	}
	return resp, nil
}

// startPingKeepAlive 启动一个 goroutine 定期发送 SSE ping 注释保活。
// 返回的 stop 函数会取消 goroutine 并同步等待其退出，确保调用方拿到控制权时不再有并发写入。
func startPingKeepAlive(c *gin.Context, pingInterval time.Duration) (stop func()) {
	pingerCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	gopool.Go(func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				logger.Debug(c.Request.Context(), fmt.Sprintf("SSE pre-request ping goroutine panic recovered: %v", r))
			}
		}()

		if pingInterval <= 0 {
			pingInterval = defaultPingInterval
		}

		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()

		maxPingDuration := 120 * time.Minute
		pingTimeout := time.NewTimer(maxPingDuration)
		defer pingTimeout.Stop()

		for {
			select {
			case <-ticker.C:
				if c.Writer == nil {
					return
				}
				_, err := c.Writer.Write([]byte("event: ping\ndata: {\"type\": \"ping\"}\n\n"))
				if err != nil {
					return
				}
				if flusher, ok := c.Writer.(http.Flusher); ok {
					flusher.Flush()
				}
			case <-pingerCtx.Done():
				return
			case <-c.Request.Context().Done():
				return
			case <-pingTimeout.C:
				return
			}
		}
	})

	return func() {
		cancel()
		<-done
	}
}
