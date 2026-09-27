package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const systemOneTestRequest = `{"model":"test-alias","state":{"records":["hello",9007199254740993]},"questions":{"check":{"type":"noul","instructions":{"question":"Is this a greeting?"}},"choice":{"type":"choice","instructions":"Choose","criteria":{"a":null,"b":{"hint":"B"}}},"score":{"type":"score","instructions":"Rate","criteria":["low","high"]}},"extension":{"keep":true}}`
const systemOneTestResponse = `{"model":"jev-1.13.0","answers":{"check":{"type":"noul","noul":0.99},"choice":{"type":"choice","choice":"a","probabilities":{"a":0.9,"b":0.1},"confidence":0.8},"score":{"type":"score","score":0.1,"legend":{"0":"low","1":"high"},"probabilities":{"0":0.9,"1":0.1},"confidence":0.8}},"usage":{"input_tokens":1000,"output_tokens":20}}`

func TestSystemOneValidation(t *testing.T) {
	for _, body := range []string{
		`null`, `{}`, `[]`, `{"model":"jev-latest","state":null,"questions":{"x":{}}}`,
		`{"model":"jev-latest","state":12,"questions":{"x":{}}}`,
		`{"model":"jev-latest","state":"hi","questions":[]}`,
		`{"model":"jev-latest","state":"hi","questions":{}}`,
		`{"model":"jev-latest","state":"hi","questions":{"x":{}},"stream":true}`,
	} {
		_, _, _, err := systemOneRequest([]byte(body), nil)
		require.Error(t, err, body)
	}
	for _, suffix := range []string{"", "/", "/v1", "/v1/", "/v1/systemone"} {
		actual, err := SystemOneRequestURL("https://api.typesafe.ai" + suffix)
		require.NoError(t, err)
		assert.Equal(t, "https://api.typesafe.ai/v1/systemone", actual)
	}
	for _, base := range []string{"", "api.typesafe.ai", "file:///tmp/secret", "https://example.com?key=secret"} {
		_, err := SystemOneRequestURL(base)
		require.Error(t, err)
	}
	for _, usage := range []string{`null`, `{}`, `{"input_tokens":1}`, `{"input_tokens":-1,"output_tokens":0}`, `{"input_tokens":1.5,"output_tokens":0}`, `{"input_tokens":9223372036854775807,"output_tokens":1}`} {
		_, err := ParseSystemOneUsage([]byte(`{"model":"jev-latest","answers":{"x":{}},"usage":` + usage + `}`))
		require.Error(t, err, usage)
	}
}

func TestSystemOneRelayBillingAndRefunds(t *testing.T) {
	oldDB, oldLogs := model.DB, model.LOG_DB
	oldRedis, oldBatch, oldConsume := common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled
	oldClient, oldGroups, oldPrices := util.HTTPClient, common.GroupRatio, common.ModelPrice
	oldDiscounts := common.ModelDiscountsJSON()
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogs
		common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled = oldRedis, oldBatch, oldConsume
		util.HTTPClient, common.GroupRatio, common.ModelPrice = oldClient, oldGroups, oldPrices
		require.NoError(t, common.UpdateModelDiscounts(oldDiscounts))
	})
	common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled = false, false, true
	common.GroupRatio = map[string]float64{"systemone-test": 1}
	for _, tc := range []struct {
		name       string
		response   string
		status     int
		discount   float64
		fixedPrice bool
		lowToken   bool
		quota      int64
	}{
		{"actual input only", systemOneTestResponse, 200, 1, false, false, 21},
		{"combined discounts", systemOneTestResponse, 200, 0.5, false, false, 2},
		{"fixed price", systemOneTestResponse, 200, 1, true, false, 1000},
		{"422 refunds", `{"detail":[{"loc":["body","questions"],"msg":"invalid question"}]}`, 422, 1, false, false, 0},
		{"429 refunds", `{"detail":"rate limited"}`, 429, 1, false, false, 0},
		{"529 refunds", `{"detail":"overloaded"}`, 529, 1, false, false, 0},
		{"network failure refunds", "", -1, 1, false, false, 0},
		{"missing usage refunds", `{"model":"jev-1.13.0","answers":{"check":{"noul":0.9}}}`, 200, 1, false, false, 0},
		{"invalid response refunds", `<html>error</html>`, 200, 1, false, false, 0},
		{"token quota enforced", systemOneTestResponse, 200, 1, false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			common.ModelPrice = map[string]float64{}
			common.GroupRatio["systemone-test"] = tc.discount
			require.NoError(t, common.UpdateModelDiscounts(`{}`))
			if tc.discount != 1 {
				require.NoError(t, common.UpdateModelDiscounts(`{"jev-latest":0.5}`))
			}
			if tc.fixedPrice {
				common.ModelPrice["jev-latest"] = 0.002
			}
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			defer sqlDB.Close()
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Log{}))
			model.DB, model.LOG_DB = db, db
			const balance int64 = 100000
			tokenBalance := balance
			if tc.lowToken {
				tokenBalance = 1
			}
			require.NoError(t, db.Create(&model.User{Id: 9501, Username: "systemone", Quota: balance}).Error)
			require.NoError(t, db.Create(&model.Token{Id: 9502, UserId: 9501, Key: "systemone", RemainQuota: tokenBalance}).Error)
			require.NoError(t, db.Create(&model.Channel{Id: 9503, Name: "TypeSafe"}).Error)
			called := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				assert.Equal(t, "/v1/systemone", r.URL.Path)
				assert.Equal(t, "Bearer upstream-secret", r.Header.Get("Authorization"))
				assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
				assert.Equal(t, "present", r.Header.Get("X-Channel"))
				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				var fields map[string]json.RawMessage
				assert.NoError(t, json.Unmarshal(body, &fields))
				assert.Equal(t, `"jev-latest"`, string(fields["model"]))
				assert.Equal(t, `{"records":["hello",9007199254740993]}`, string(fields["state"]))
				assert.Equal(t, `{"keep":true}`, string(fields["extension"]))
				var original map[string]json.RawMessage
				assert.NoError(t, json.Unmarshal([]byte(systemOneTestRequest), &original))
				assert.JSONEq(t, string(original["questions"]), string(fields["questions"]))
				if tc.status == -1 {
					connection, _, err := w.(http.Hijacker).Hijack()
					if assert.NoError(t, err) {
						_ = connection.Close()
					}
					return
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			}))
			defer server.Close()
			util.HTTPClient = server.Client()
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(systemOneTestRequest))
			c.Request.Header.Set("Authorization", "Bearer client-key-must-not-leak")
			c.Set("id", 9501)
			c.Set("token_id", 9502)
			c.Set("channel_id", 9503)
			c.Set("channel", common.ChannelTypeCustom)
			c.Set("actual_key", "upstream-secret")
			c.Set("base_url", server.URL+"/v1/")
			c.Set("group", "systemone-test")
			c.Set("token_name", "systemone-token")
			c.Set("X-Request-ID", "systemone-request-id")
			c.Set("model_mapping", map[string]string{"test-alias": "jev-latest"})
			c.Set("headers_override", map[string]string{"X-Channel": "present"})
			c.Set("channel_discount", tc.discount)
			c.Set("user_channel_ratio", tc.discount)
			c.Set("admin_channel_history", []int{9503})
			apiErr := RelaySystemOneHelper(c)
			success := tc.quota > 0
			if success {
				require.Nil(t, apiErr)
				assert.Equal(t, tc.response, w.Body.String())
				assert.Equal(t, "systemone-request-id", w.Header().Get("X-Request-ID"))
			} else {
				require.NotNil(t, apiErr)
				if tc.lowToken {
					assert.Equal(t, 403, apiErr.StatusCode)
					assert.False(t, called)
				} else if tc.status != 200 && tc.status != -1 {
					assert.Equal(t, tc.status, apiErr.StatusCode)
					assert.NotEmpty(t, apiErr.Error.Message)
				} else {
					assert.Equal(t, 502, apiErr.StatusCode)
				}
				assert.Empty(t, w.Body.String())
			}
			var user model.User
			var token model.Token
			var channel model.Channel
			require.NoError(t, db.First(&user, 9501).Error)
			require.NoError(t, db.First(&token, 9502).Error)
			require.NoError(t, db.First(&channel, 9503).Error)
			assert.Equal(t, balance-tc.quota, user.Quota)
			assert.Equal(t, tokenBalance-tc.quota, token.RemainQuota)
			assert.Equal(t, tc.quota, user.UsedQuota)
			assert.Equal(t, tc.quota, token.UsedQuota)
			assert.Equal(t, tc.quota, channel.UsedQuota)
			var logs []model.Log
			require.NoError(t, db.Find(&logs).Error)
			if success {
				require.Len(t, logs, 1)
				assert.Equal(t, 1000, logs[0].PromptTokens)
				assert.Equal(t, 20, logs[0].CompletionTokens)
				assert.Equal(t, "test-alias", logs[0].ModelName)
				assert.Equal(t, "systemone-request-id", logs[0].XRequestID)
				assert.Contains(t, logs[0].Other, "is_model_mapped:true")
				assert.NotContains(t, logs[0].Other, "upstream-secret")
			} else {
				assert.Empty(t, logs)
			}
		})
	}
}
