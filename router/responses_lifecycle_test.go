package router

import (
	"bufio"
	"bytes"
	"context"
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
	for _, mode := range []string{"numeric", "empty", "ping_error", "ping_eof", "http429"} {
		t.Run(mode, func(t *testing.T) {
			pingSeen := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "http429" {
					// 即使开启 ping，等待 HTTP 头阶段也必须保留 HTTP 错误状态。
					time.Sleep(1200 * time.Millisecond)
					w.WriteHeader(429)
					_, _ = io.WriteString(w, `{"error":{"code":"insufficient_quota","message":"Quota exceeded"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				if strings.HasPrefix(mode, "ping_") {
					select {
					case <-pingSeen:
					case <-time.After(5 * time.Second):
						return
					}
				} else {
					_, _ = io.WriteString(w, "data: {\"type\":\"response.in_progress\",\"response\":{}}\n\n")
				}
				switch mode {
				case "numeric":
					_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":429,\"message\":\"Too many requests\"}}}\n\n")
				case "empty":
					_, _ = io.WriteString(w, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{}}}\n\n")
				case "ping_error":
					_, _ = io.WriteString(w, "data: {\"type\":\"error\",\"code\":\"insufficient_quota\",\"message\":\"Quota exceeded\"}\n\n")
				}
			}))
			t.Cleanup(upstream.Close)
			fixture := newResponsesLifecycle(t, upstream.URL, "upstream-test-key", pingSeen)
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
			if mode == "http429" {
				require.Equal(t, 429, res.StatusCode, string(body))
				require.NotContains(t, string(body), "event: ping")
				fixture.assertFailure(t, 2)
			} else {
				require.Equal(t, 200, res.StatusCode)
				require.Equal(t, 1, strings.Count(string(body), "event: response.failed"), string(body))
				if strings.HasPrefix(mode, "ping_") {
					require.Contains(t, string(body), "event: ping")
					require.Less(t, strings.Index(string(body), "event: ping"), strings.Index(string(body), "event: response.failed"))
				}
				fixture.assertFailure(t, 1)
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
