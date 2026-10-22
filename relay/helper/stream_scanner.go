package helper

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"

	"github.com/bytedance/gopkg/util/gopool"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/relay/util"
)

const (
	InitialScannerBufferSize = 64 << 10 // 64KB (64*1024)
	MaxScannerBufferSize     = 50 << 20 // 50MB - large enough for base64 encoded images from Gemini
	DefaultPingInterval      = 10 * time.Second
	DefaultStreamingTimeout  = 300 * time.Second
)

func StreamScannerHandler(c *gin.Context, resp *http.Response, info *util.RelayMeta, dataHandler func(data string) bool) {

	if resp == nil || dataHandler == nil {
		return
	}
	println("ping interval seconds:")

	// resp.Body 由下方清理 defer 中关闭（在等待 goroutine 之前），此处不再重复注册

	streamingTimeout := time.Duration(config.StreamingTimeout) * time.Second
	if streamingTimeout <= 0 {
		streamingTimeout = DefaultStreamingTimeout
	}

	var (
		stopChan    = make(chan bool, 3)  // 增加缓冲区避免阻塞
		scannerDone = make(chan struct{}) // 独立广播完成，不能让 ping 消费掉主请求的退出通知
		scanner     = bufio.NewScanner(resp.Body)
		ticker      = time.NewTicker(streamingTimeout)
		pingTicker  *time.Ticker
		writeMutex  sync.Mutex     // Mutex to protect concurrent writes
		wg          sync.WaitGroup // 用于等待所有 goroutine 退出
	)

	pingEnabled := config.PingIntervalEnabled && !info.DisablePing
	pingInterval := time.Duration(config.PingIntervalSeconds) * time.Second
	if pingInterval <= 0 {
		pingInterval = DefaultPingInterval
	}

	if pingEnabled {
		pingTicker = time.NewTicker(pingInterval)
	}

	if config.DebugEnabled {
		// print timeout and ping interval for debugging
		//println("relay timeout seconds:", common.RelayTimeout)
		println("streaming timeout seconds:", int64(streamingTimeout.Seconds()))
		println("ping interval seconds:", int64(pingInterval.Seconds()))
	}

	// 改进资源清理，确保所有 goroutine 正确退出
	defer func() {
		// 通知所有 goroutine 停止
		common.SafeSendBool(stopChan, true)

		ticker.Stop()
		if pingTicker != nil {
			pingTicker.Stop()
		}

		// 先关闭上游连接，让阻塞在 scanner.Scan() 的 goroutine 立即返回，
		// 否则 scanner.Scan() 是阻塞 I/O，stopChan 无法打断它。
		if resp.Body != nil {
			resp.Body.Close()
		}

		// 等待所有 goroutine 退出，最多等待5秒
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			logger.Error(c.Request.Context(), "timeout waiting for goroutines to exit")
		}

		close(stopChan)
	}()

	scanner.Buffer(make([]byte, InitialScannerBufferSize), MaxScannerBufferSize)
	scanner.Split(bufio.ScanLines)
	common.SetEventStreamHeaders(c)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctx = context.WithValue(ctx, "stop_chan", stopChan)

	// Handle ping data sending with improved error handling
	if pingEnabled && pingTicker != nil {
		wg.Add(1)
		gopool.Go(func() {
			defer func() {
				defer wg.Done()
				if r := recover(); r != nil {
					logger.Error(c, fmt.Sprintf("ping goroutine panic: %v", r))
					common.SafeSendBool(stopChan, true)
				}
				if config.DebugEnabled {
					println("ping goroutine exited")
				}
			}()

			// 添加超时保护，防止 goroutine 无限运行
			maxPingDuration := 30 * time.Minute // 最大 ping 持续时间
			pingTimeout := time.NewTimer(maxPingDuration)
			defer pingTimeout.Stop()

			for {
				select {
				case <-pingTicker.C:
					// 使用超时机制防止写操作阻塞
					done := make(chan error, 1)
					go func() {
						writeMutex.Lock()
						defer writeMutex.Unlock()
						if info.DeferPingUntilResponse && !c.Writer.Written() {
							done <- nil
							return
						}
						done <- PingData(c)
					}()

					select {
					case err := <-done:
						if err != nil {
							logger.Error(c, "ping data error: "+err.Error())
							return
						}
						if config.DebugEnabled {
							logger.Info(c, "ping data sent")
						}
					case <-time.After(10 * time.Second):
						logger.Error(c, "ping data send timeout")
						return
					case <-ctx.Done():
						return
					case <-stopChan:
						return
					}
				case <-ctx.Done():
					return
				case <-stopChan:
					return
				case <-c.Request.Context().Done():
					// 监听客户端断开连接
					return
				case <-pingTimeout.C:
					logger.Error(c, "ping goroutine max duration reached")
					return
				}
			}
		})
	}

	// Scanner goroutine with improved error handling
	wg.Add(1)
	common.RelayCtxGo(ctx, func() {
		defer func() {
			defer wg.Done()
			defer close(scannerDone)
			if r := recover(); r != nil {
				logger.Error(c, fmt.Sprintf("scanner goroutine panic: %v", r))
			}
			common.SafeSendBool(stopChan, true)
			if config.DebugEnabled {
				println("scanner goroutine exited")
			}
		}()

		for scanner.Scan() {
			// 检查是否需要停止
			select {
			case <-stopChan:
				return
			case <-ctx.Done():
				return
			case <-c.Request.Context().Done():
				return
			default:
			}

			ticker.Reset(streamingTimeout)
			data := scanner.Text()
			if config.DebugEnabled {
				println(data)
			}

			if len(data) < 6 {
				continue
			}
			if data[:5] != "data:" && data[:6] != "[DONE]" {
				continue
			}
			data = data[5:]
			data = strings.TrimLeft(data, " ")
			data = strings.TrimSuffix(data, "\r")
			if !strings.HasPrefix(data, "[DONE]") {
				info.SetFirstResponseTime()

				// 使用超时机制防止写操作阻塞
				done := make(chan bool, 1)
				go func() {
					writeMutex.Lock()
					defer writeMutex.Unlock()
					done <- dataHandler(data)
				}()

				select {
				case success := <-done:
					if !success {
						return
					}
				case <-time.After(10 * time.Second):
					logger.Error(c, "data handler timeout")
					return
				case <-ctx.Done():
					return
				case <-stopChan:
					return
				}
			} else {
				// done, 处理完成标志，直接退出停止读取剩余数据防止出错
				logger.Info(c, "received [DONE], stopping scanner")
				return
			}
		}

		if err := scanner.Err(); err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "use of closed network connection") {
				logger.Error(c, "scanner error: "+err.Error())
			}
		}
	})

	// 主循环等待完成或超时
	select {
	case <-scannerDone:
		logger.Info(c, "streaming finished")
	case <-ticker.C:
		// 超时处理逻辑
		logger.Error(c, "streaming timeout")
	case <-stopChan:
		// 正常结束
		logger.Info(c, "streaming finished")
	case <-c.Request.Context().Done():
		// 客户端断开连接
		logger.Info(c, "client disconnected")
	}
}
