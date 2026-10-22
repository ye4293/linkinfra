package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/util"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 通过完整鉴权、选渠和 RelayResponse 验证失败时不扣费、不重放。
func TestResponsesFailureLifecycle(t *testing.T) {
	for _, mode := range []string{"numeric", "created429", "empty", "ping_error", "ping_eof", "http429", "first_event429", "provider_filtered"} {
		t.Run(mode, func(t *testing.T) {
			pingSeen := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "http429" || mode == "provider_filtered" {
					// 即使开启 ping，等待 HTTP 头阶段也必须保留 HTTP 错误状态。
					time.Sleep(1200 * time.Millisecond)
					w.WriteHeader(429)
					_, _ = io.WriteString(w, `{"error":{"code":"insufficient_quota","message":"Quota exceeded"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				if mode == "first_event429" {
					_, _ = io.WriteString(w, "data: {\"type\":\"error\",\"code\":429,\"message\":\"Requests have exceeded the throughput limit\"}\n\n")
					return
				}
				if strings.HasPrefix(mode, "ping_") {
					select {
					case <-pingSeen:
					case <-time.After(5 * time.Second):
						return
					}
				} else if mode == "created429" {
					_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{}}\n\n")
				} else {
					_, _ = io.WriteString(w, "data: {\"type\":\"response.in_progress\",\"response\":{}}\n\n")
				}
				switch mode {
				case "numeric", "created429":
					_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":429,\"message\":\"Too many requests\"}}}\n\n")
				case "empty":
					_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{}}}\n\n")
				case "ping_error":
					_, _ = io.WriteString(w, "data: {\"type\":\"error\",\"code\":\"insufficient_quota\",\"message\":\"Quota exceeded\"}\n\n")
				}
			}))
			t.Cleanup(upstream.Close)
			fixture := newResponsesLifecycle(t, upstream.URL, "upstream-test-key", pingSeen)
			if mode == "provider_filtered" {
				require.NoError(t, fixture.db.Model(&model.Channel{}).Where("id = ?", 2).Update("config", `{"provider":"another-provider"}`).Error)
			}
			res, err := fixture.gateway.Client().Post(fixture.gateway.URL+"/v1/responses", "application/json", nil)
			require.NoError(t, err)
			res.Body.Close()
			require.Equal(t, 401, res.StatusCode)
			require.Zero(t, fixture.calls.Load())
			req, err := http.NewRequest("POST", fixture.gateway.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.1","input":"Reply OK","stream":true}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer sk-responseslifecycle")
			req.Header.Set("Content-Type", "application/json")
			res, err = fixture.gateway.Client().Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			require.NoError(t, err)
			if mode == "http429" || mode == "first_event429" || mode == "provider_filtered" {
				require.Equal(t, 429, res.StatusCode, string(body))
				require.NotContains(t, string(body), "event: ping")
				expectedAttempts := 2
				if mode == "provider_filtered" {
					expectedAttempts = 1
				}
				fixture.assertFailure(t, expectedAttempts)
			} else {
				require.Equal(t, 200, res.StatusCode)
				require.Equal(t, 1, strings.Count(string(body), "event: response.failed"), string(body))
				if strings.HasPrefix(mode, "ping_") {
					require.Contains(t, string(body), "event: ping")
					require.Less(t, strings.Index(string(body), "event: ping"), strings.Index(string(body), "event: response.failed"))
				}
				fixture.assertFailure(t, 1)
			}
			diagnostics := fixture.diagnostics.String()
			require.Contains(t, diagnostics, "event=request_started")
			require.Contains(t, diagnostics, "event=request_failed")
			require.Contains(t, diagnostics, "event=decision")
			require.Contains(t, diagnostics, "text_deltas=0")
			if mode == "provider_filtered" {
				require.Contains(t, diagnostics, "action=retry")
				require.Contains(t, diagnostics, "before_boundary=1 after_boundary=0")
				require.Contains(t, diagnostics, "provider_match:false state_match:true")
				require.Contains(t, diagnostics, "event=selection_failed")
				require.NotContains(t, diagnostics, "event=channel_selected")
			} else if mode == "http429" || mode == "first_event429" {
				require.Contains(t, diagnostics, "action=retry")
				require.Contains(t, diagnostics, "event=channel_selected attempt=2")
				require.Contains(t, diagnostics, "before_boundary=1 after_boundary=1")
				require.Contains(t, diagnostics, "reason=provider_channels_exhausted")
				require.NotContains(t, diagnostics, "event=first_write")
			} else {
				require.Contains(t, diagnostics, `reason=\"response_already_started\"`)
				require.Contains(t, diagnostics, "retries_remaining=2")
				require.Contains(t, diagnostics, "non_prelude_events=0")
				require.NotContains(t, diagnostics, "event=selection_started")
				source := `source=upstream event_type=\"response.in_progress\"`
				if mode == "created429" {
					source = `source=upstream event_type=\"response.created\"`
				} else if strings.HasPrefix(mode, "ping_") {
					source = "source=gateway event_type=ping"
				}
				require.Contains(t, diagnostics, source)
			}
			for _, secret := range []string{"upstream-test-key", "sk-responseslifecycle", "Reply OK", "Quota exceeded", "Too many requests"} {
				require.NotContains(t, diagnostics, secret)
			}
		})
	}
}

type responsesLifecycle struct {
	db          *gorm.DB
	gateway     *httptest.Server
	calls       atomic.Int32
	wire        bytes.Buffer
	mu          sync.Mutex
	holdForPing bool
	ping        chan struct{}
	diagnostics responsesDiagnosticLog
}

// 多个流协程共用日志输出，捕获时也必须同步；仅保留本次新增的诊断行。
type responsesDiagnosticLog struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *responsesDiagnosticLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if bytes.Contains(p, []byte("responses_retry_diag")) {
		_, _ = l.buffer.Write(p)
	}
	return len(p), nil
}

func (l *responsesDiagnosticLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.String()
}

type responsesCaptureWriter struct {
	gin.ResponseWriter
	fixture *responsesLifecycle
	ping    chan struct{}
	once    sync.Once
}

func (w *responsesCaptureWriter) Write(b []byte) (int, error) {
	w.fixture.mu.Lock()
	w.fixture.wire.Write(b)
	w.fixture.mu.Unlock()
	n, err := w.ResponseWriter.Write(b)
	if bytes.Contains(b, []byte("event: ping")) {
		w.once.Do(func() { close(w.ping) })
	}
	return n, err
}

type responsesCountingTransport struct {
	base    http.RoundTripper
	fixture *responsesLifecycle
}

func (tr responsesCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr.fixture.calls.Add(1)
	resp, err := tr.base.RoundTrip(req)
	if err == nil && resp.StatusCode == 200 && tr.fixture.holdForPing {
		resp.Body = &responsesWaitForPingBody{ReadCloser: resp.Body, ping: tr.fixture.ping}
	}
	return resp, err
}

// 仅用于真实联调：保持上游内容原样，等实际 ping 发出后才开始读取响应体。
type responsesWaitForPingBody struct {
	io.ReadCloser
	ping  <-chan struct{}
	ready bool
}

func (b *responsesWaitForPingBody) Read(p []byte) (int, error) {
	if !b.ready {
		select {
		case <-b.ping:
			b.ready = true
		case <-time.After(5 * time.Second):
			return 0, fmt.Errorf("test gateway did not send ping")
		}
	}
	return b.ReadCloser.Read(p)
}

func newResponsesLifecycle(t *testing.T, baseURL, key string, ping chan struct{}) *responsesLifecycle {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Log{}))
	oldDB, oldLog, oldClient := model.DB, model.LOG_DB, util.HTTPClient
	oldRedis, oldBatch, oldMemory, oldLogs := common.RedisEnabled, config.BatchUpdateEnabled, config.MemoryCacheEnabled, config.LogConsumeEnabled
	oldRetry, oldDisable, oldPing, oldSeconds := config.RetryTimes, config.AutomaticDisableChannelEnabled, config.PingIntervalEnabled, config.PingIntervalSeconds
	oldGroups := common.GroupRatio
	t.Cleanup(func() {
		model.DB, model.LOG_DB, util.HTTPClient = oldDB, oldLog, oldClient
		common.RedisEnabled, config.BatchUpdateEnabled, config.MemoryCacheEnabled, config.LogConsumeEnabled = oldRedis, oldBatch, oldMemory, oldLogs
		config.RetryTimes, config.AutomaticDisableChannelEnabled, config.PingIntervalEnabled, config.PingIntervalSeconds = oldRetry, oldDisable, oldPing, oldSeconds
		common.GroupRatio = oldGroups
		sqlDB.Close()
	})
	model.DB, model.LOG_DB = db, db
	common.RedisEnabled, config.BatchUpdateEnabled, config.MemoryCacheEnabled, config.LogConsumeEnabled = false, false, false, true
	config.RetryTimes, config.AutomaticDisableChannelEnabled, config.PingIntervalEnabled, config.PingIntervalSeconds = 2, false, true, 1
	common.GroupRatio = map[string]float64{"responses-lifecycle": 1}
	user := model.User{Id: 1, Username: "responses-lifecycle", Group: "responses-lifecycle", Status: common.UserStatusEnabled, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "responseslifecycle", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000000}
	require.NoError(t, db.Create(&token).Error)
	// 提供同 Provider 的备用渠道，确保流开始后不重试不是因为无候选渠道。
	for index := 0; index < 2; index++ {
		priority := int64(2 - index)
		ch := model.Channel{Type: common.ChannelTypeOpenAI, Key: key, Name: "Responses lifecycle", Models: "gpt-5.1", Group: user.Group, BaseURL: &baseURL, Priority: &priority, Config: `{"provider":"responses-lifecycle"}`}
		require.NoError(t, ch.Insert())
	}
	fixture := &responsesLifecycle{db: db, ping: ping}
	previousWriter := gin.DefaultWriter
	gin.DefaultWriter = &fixture.diagnostics
	t.Cleanup(func() { gin.DefaultWriter = previousWriter })
	util.HTTPClient = &http.Client{Transport: responsesCountingTransport{base: http.DefaultTransport, fixture: fixture}, Timeout: 30 * time.Second}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Writer = &responsesCaptureWriter{ResponseWriter: c.Writer, fixture: fixture, ping: ping}
		c.Next()
	})
	SetRelayRouter(r)
	fixture.gateway = httptest.NewServer(r)
	t.Cleanup(fixture.gateway.Close)
	return fixture
}

func (f *responsesLifecycle) assertFailure(t *testing.T, expectedCalls int) {
	t.Helper()
	require.EqualValues(t, expectedCalls, f.calls.Load(), "upstream retries must respect whether streaming has started")
	var user model.User
	var token model.Token
	require.NoError(t, f.db.First(&user, 1).Error)
	require.NoError(t, f.db.First(&token).Error)
	require.EqualValues(t, 1000000, user.Quota)
	require.EqualValues(t, 1000000, token.RemainQuota)
	require.Zero(t, user.UsedQuota)
	var logs []model.Log
	require.NoError(t, f.db.Find(&logs).Error)
	require.Len(t, logs, 1)
	require.Equal(t, model.LogTypeError, logs[0].Type)
	require.Zero(t, logs[0].Quota)
	parts := strings.SplitN(logs[0].Other, "retryHistory:", 2)
	require.Len(t, parts, 2)
	var attempts []util.RetryAttempt
	require.NoError(t, json.NewDecoder(strings.NewReader(parts[1])).Decode(&attempts))
	require.Len(t, attempts, expectedCalls)
	for i, attempt := range attempts {
		require.Equal(t, i+1, attempt.Attempt)
		require.NotEmpty(t, attempt.Error)
		require.GreaterOrEqual(t, attempt.Status, 400)
		if i > 0 {
			require.NotEqual(t, attempts[i-1].ChannelId, attempt.ChannelId)
		}
	}
	require.NotEmpty(t, attempts[len(attempts)-1].StopReason)

	t.Logf("upstream_requests=%d user_quota=%d token_quota=%d error_logs=%d consume_logs=0", f.calls.Load(), user.Quota, token.RemainQuota, len(logs))
}

// 必须显式提供无余额的测试 key 及 Codex 可执行文件；普通回归不访问真实上游。
func TestResponsesCodexLiveQuotaFailure(t *testing.T) {
	key, codexBin := os.Getenv("RESPONSES_LIVE_API_KEY"), os.Getenv("RESPONSES_CODEX_BIN")
	if key == "" || codexBin == "" {
		t.Skip("RESPONSES_LIVE_API_KEY and RESPONSES_CODEX_BIN are required")
	}
	for _, withPing := range []bool{false, true} {
		t.Run(fmt.Sprintf("ping_%t", withPing), func(t *testing.T) {
			runResponsesCodexLiveQuotaFailure(t, key, codexBin, withPing)
		})
	}
}

func runResponsesCodexLiveQuotaFailure(t *testing.T, key, codexBin string, withPing bool) {
	fixture := newResponsesLifecycle(t, "https://api.openai.com", key, make(chan struct{}))
	fixture.holdForPing = withPing
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexBin, "exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check", "--color", "never", "--sandbox", "read-only", "--model", "gpt-5.1",
		"--disable", "daemon_auto_start", "--disable", "shell_snapshot", "--enable", "skip_host_skill_discovery",
		"-c", `model_provider="responses-test"`,
		"-c", `model_providers.responses-test.name="Responses local test"`,
		"-c", fmt.Sprintf(`model_providers.responses-test.base_url="%s/v1"`, fixture.gateway.URL),
		"-c", `model_providers.responses-test.env_key="RESPONSES_GATEWAY_TOKEN"`,
		"-c", `model_providers.responses-test.wire_api="responses"`,
		"Reply with OK only. Do not use tools.")
	cmd.Dir = t.TempDir()
	for _, entry := range os.Environ() {
		name := strings.ToUpper(strings.SplitN(entry, "=", 2)[0])
		if name != "RESPONSES_LIVE_API_KEY" && name != "NO_PROXY" && name != "CODEX_DAEMON_SHUTDOWN_SOCKET" && name != "CODEX_THREAD_ID" && name != "CODEX_SESSION_ID" {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "RESPONSES_GATEWAY_TOKEN=sk-responseslifecycle", "NO_PROXY=localhost,127.0.0.1,::1")
	output, err := cmd.CombinedOutput()
	safeOutput := strings.ReplaceAll(string(output), key, "[REDACTED]")
	t.Log(safeOutput)
	require.NoError(t, ctx.Err(), "Codex must finish before the timeout")
	require.Error(t, err, "the explicitly supplied no-credit key must fail")
	require.Contains(t, safeOutput, "Quota exceeded")
	require.NotContains(t, safeOutput, "Reconnecting")
	require.NotContains(t, safeOutput, "stream closed before response.completed")
	fixture.assertFailure(t, 1)
	fixture.mu.Lock()
	wire := fixture.wire.String()
	fixture.mu.Unlock()
	require.Equal(t, 1, strings.Count(wire, "event: response.failed"))
	if withPing {
		require.Contains(t, wire, "event: ping")
		require.Less(t, strings.Index(wire, "event: ping"), strings.Index(wire, "event: response.failed"))
	}
	// 只记录事件类型，不记录模型请求、凭据或 response 元数据。
	scanner := bufio.NewScanner(strings.NewReader(wire))
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "event:") {
			t.Log(scanner.Text())
		}
	}
}

// 模拟请求期间管理员禁用全部渠道，内存仍保留启用对象。
func TestResponsesDoesNotFallbackToDisabledChannel(t *testing.T) {
	var fixture *responsesLifecycle
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := fixture.db.Model(&model.Channel{}).Where("id > ?", 0).Update("status", common.ChannelStatusManuallyDisabled).Error; err != nil {
			t.Error(err)
		}
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"message":"Requests have exceeded the throughput limit"}}`)
	}))
	t.Cleanup(upstream.Close)
	fixture = newResponsesLifecycle(t, upstream.URL, "test-key", make(chan struct{}))
	// 未配置 provider 时正常选择器允许循环，仍不能绕过数据库复用旧对象。
	require.NoError(t, fixture.db.Model(&model.Channel{}).Where("id > ?", 0).Update("config", "{}").Error)
	config.MemoryCacheEnabled = true
	model.InitChannelCache()
	req, err := http.NewRequest("POST", fixture.gateway.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.1","input":"hello","stream":true}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer sk-responseslifecycle")
	req.Header.Set("Content-Type", "application/json")
	res, err := fixture.gateway.Client().Do(req)
	require.NoError(t, err)
	_, err = io.ReadAll(res.Body)
	res.Body.Close()
	require.NoError(t, err)
	require.Equal(t, 429, res.StatusCode)
	fixture.assertFailure(t, 1)
	var log model.Log
	require.NoError(t, fixture.db.First(&log).Error)
	require.Contains(t, log.Other, `"stop_reason":"no_available_channel"`)
}

// 大正文在 429 后完整重放，最终只计费一次，成功日志包含先前失败渠道。
func TestResponsesLargeInputRetrySuccess(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			input := strings.Repeat("The release ledger contains a verified routing record.\n", 6000) + "FINAL_MARKER_6141"
			requestBody, err := json.Marshal(map[string]any{"model": "gpt-5.1", "input": input, "stream": stream})
			require.NoError(t, err)
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, readErr := io.ReadAll(r.Body)
				if readErr != nil || !bytes.Equal(requestBody, got) {
					t.Error("retry did not preserve the complete large request body")
					w.WriteHeader(400)
					return
				}
				if calls.Add(1) == 1 {
					w.WriteHeader(429)
					_, _ = io.WriteString(w, `{"error":{"message":"Requests have exceeded the throughput limit"}}`)
					return
				}
				response := `{"id":"resp_large_retry_success","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"FINAL_MARKER_6141"}]}],"usage":{"input_tokens":48000,"output_tokens":12,"total_tokens":48012}}`
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, response)
				}
			}))
			t.Cleanup(upstream.Close)
			fixture := newResponsesLifecycle(t, upstream.URL, "test-key", make(chan struct{}))
			req, err := http.NewRequest("POST", fixture.gateway.URL+"/v1/responses", bytes.NewReader(requestBody))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer sk-responseslifecycle")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Request-ID", fmt.Sprintf("large-retry-%t", stream))
			res, err := fixture.gateway.Client().Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			require.NoError(t, err)
			require.Equal(t, 200, res.StatusCode, string(body))
			require.Contains(t, string(body), "FINAL_MARKER_6141")
			require.EqualValues(t, 2, fixture.calls.Load())
			var logs []model.Log
			require.Eventually(t, func() bool {
				return fixture.db.Find(&logs).Error == nil && len(logs) == 1
			}, 3*time.Second, 10*time.Millisecond)
			require.Equal(t, model.LogTypeConsume, logs[0].Type)
			require.Positive(t, logs[0].Quota)
			require.Equal(t, 48000, logs[0].PromptTokens)
			require.Equal(t, 12, logs[0].CompletionTokens)
			require.Equal(t, fmt.Sprintf("large-retry-%t", stream), logs[0].XRequestID)
			parts := strings.SplitN(logs[0].Other, "retryHistory:", 2)
			require.Len(t, parts, 2)
			var attempts []util.RetryAttempt
			require.NoError(t, json.NewDecoder(strings.NewReader(parts[1])).Decode(&attempts))
			require.Len(t, attempts, 2)
			require.Equal(t, 429, attempts[0].Status)
			require.Equal(t, 200, attempts[1].Status)
			require.NotEqual(t, attempts[0].ChannelId, attempts[1].ChannelId)
			var user model.User
			var token model.Token
			require.NoError(t, fixture.db.First(&user, 1).Error)
			require.NoError(t, fixture.db.First(&token).Error)
			require.EqualValues(t, 1000000-logs[0].Quota, user.Quota)
			require.EqualValues(t, 1000000-logs[0].Quota, token.RemainQuota)
			require.EqualValues(t, logs[0].Quota, user.UsedQuota)
		})
	}
}
