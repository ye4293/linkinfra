package controller

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	dbmodel "github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/util"
	"github.com/songquanpeng/one-api/service"
	"github.com/stretchr/testify/require"
)

func TestResponsesModelMappingPreservesHistoryAndExplicitValues(t *testing.T) {
	body := []byte(`{"model":"old","store":false,"parallel_tool_calls":false,"temperature":0,"future":{"integer":9007199254740993},"input":[{"type":"reasoning","id":"rs_a","encrypted_content":"opaque","summary":[]},{"type":"compaction","encrypted_content":"compact"},{"type":"function_call","call_id":"a","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"done"}]}`)
	before := append([]byte(nil), body...)
	result, err := mapResponsesModel(body, "deployment")
	require.NoError(t, err)
	var old, updated map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &old))
	require.NoError(t, json.Unmarshal(result, &updated))
	for k, v := range old {
		if k != "model" {
			require.JSONEq(t, string(v), string(updated[k]))
		}
	}
	require.Equal(t, before, body)
	require.Equal(t, `"deployment"`, string(updated["model"]))
}

func TestResponsesReturnedStateRoutesNextRequest(t *testing.T) {
	oldRedis, oldPing := common.RedisEnabled, config.PingIntervalEnabled
	common.RedisEnabled, config.PingIntervalEnabled = false, false
	t.Cleanup(func() { common.RedisEnabled, config.PingIntervalEnabled = oldRedis, oldPing })
	for _, stream := range []bool{false, true} {
		name := "normal"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			opaque := "controller-test-" + name
			response := `{"id":"resp_` + name + `","output":[{"id":"rs_` + name + `","type":"reasoning","encrypted_content":"` + opaque + `","summary":[]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
			payload := response
			if stream {
				payload = "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_" + name + "\",\"type\":\"reasoning\",\"encrypted_content\":\"" + opaque + "\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{}`))
			c.Request = c.Request.WithContext(dbmodel.WithRetryProvider(c.Request.Context(), "azure"))
			c.Set("id", 2718)
			c.Set("channel_id", 12)
			c.Set("key_index", 0)
			upstream := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(payload)), Header: make(http.Header)}
			meta := &util.RelayMeta{ActualModelName: "test", DisablePing: true}
			if stream {
				_, err := doNativeOpenaiResponseStream(c, upstream, meta)
				require.Nil(t, err)
			} else {
				_, err := doNativeOpenaiResponse(c, upstream, meta)
				require.Nil(t, err)
				require.Equal(t, response, w.Body.String())
			}
			require.Contains(t, w.Body.String(), opaque)
			next, _ := gin.CreateTestContext(httptest.NewRecorder())
			next.Set("id", 2718)
			nextBody := []byte(`{"input":[{"type":"reasoning","encrypted_content":"` + opaque + `"}]}`)
			next.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(nextBody))
			require.NoError(t, service.BindResponsesState(next))
			require.Equal(t, "azure", dbmodel.RetryProvider(next.Request.Context()))
		})
	}
}

func TestResponsesStreamCacheFailureBeforeAndAfterFirstEvent(t *testing.T) {
	oldRedis, oldRDB, oldPing := common.RedisEnabled, common.RDB, config.PingIntervalEnabled
	common.RedisEnabled, common.RDB, config.PingIntervalEnabled = true, nil, false
	t.Cleanup(func() { common.RedisEnabled, common.RDB, config.PingIntervalEnabled = oldRedis, oldRDB, oldPing })
	for _, started := range []bool{false, true} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		c.Set("id", 1)
		c.Set("channel_id", 12)
		payload := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_failure\"}}\n\n"
		if started {
			payload = "data: {\"type\":\"response.in_progress\",\"response\":{}}\n\n" + payload
		}
		upstream := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(payload)), Header: make(http.Header)}
		_, err := doNativeOpenaiResponseStream(c, upstream, &util.RelayMeta{DisablePing: true})
		require.NotNil(t, err)
		require.Equal(t, http.StatusServiceUnavailable, err.StatusCode)
		require.True(t, c.GetBool("responses_state_record_failed"))
		require.NotContains(t, w.Body.String(), "resp_failure")
		if started {
			require.Contains(t, w.Body.String(), "responses_state_cache_unavailable")
			require.Equal(t, "text/event-stream", w.Header().Get("Content-Type"))
		} else {
			require.False(t, c.Writer.Written())
			require.Empty(t, w.Header().Get("Transfer-Encoding"))
			c.JSON(err.StatusCode, gin.H{"error": err.Error})
			require.Equal(t, http.StatusServiceUnavailable, w.Code)
			require.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))
		}
	}
}
