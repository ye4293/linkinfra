package controller

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
	"github.com/songquanpeng/one-api/relay/constant"
	"github.com/songquanpeng/one-api/relay/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestDurationTranscriptionRejectsUnpricedAliasesAndDuplicateFormats(t *testing.T) {
	previous := common.AudioDurationPricesJSON()
	t.Cleanup(func() { require.NoError(t, common.UpdateAudioDurationPrices(previous)) })
	require.NoError(t, common.UpdateAudioDurationPrices(`{"gpt-transcribe":0.0045}`))
	for _, tc := range []struct {
		name, model, mapping string
		formats              []string
	}{
		{"unpriced alias", "transcribe-alias", `{"transcribe-alias":"gpt-transcribe"}`, []string{"json"}},
		{"duplicate formats", "gpt-transcribe", "", []string{"json", "text"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			require.NoError(t, writer.WriteField("model", tc.model))
			for _, format := range tc.formats {
				require.NoError(t, writer.WriteField("response_format", format))
			}
			file, err := writer.CreateFormFile("file", "sample.wav")
			require.NoError(t, err)
			_, err = io.WriteString(file, "audio fixture")
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
			ctx.Request.Header.Set("Content-Type", writer.FormDataContentType())
			ctx.Set("model_mapping", tc.mapping)
			apiErr := RelayAudioHelper(ctx, constant.RelayModeAudioTranscription)
			require.NotNil(t, apiErr)
			assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
		})
	}
}

func TestDurationTranscriptionRelayAndBalances(t *testing.T) {
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousRedis, previousBatch, previousLogs := common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled
	previousQuota, previousClient, previousPrices := config.QuotaPerUnit, util.HTTPClient, common.AudioDurationPricesJSON()
	previousGroups := common.GroupRatio
	previousDiscounts := common.ModelDiscountsJSON()
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled = previousRedis, previousBatch, previousLogs
		config.QuotaPerUnit, util.HTTPClient, common.GroupRatio = previousQuota, previousClient, previousGroups
		require.NoError(t, common.UpdateAudioDurationPrices(previousPrices))
		require.NoError(t, common.UpdateModelDiscounts(previousDiscounts))
	})
	common.RedisEnabled, config.BatchUpdateEnabled, config.LogConsumeEnabled = false, false, true
	common.GroupRatio = map[string]float64{"audio-test": 1}
	for _, tc := range []struct {
		name, body, source string
		stream             bool
		status             int
		price, discount    float64
		quota              int64
	}{
		{"ten minutes", `{"text":"hello","usage":{"type":"duration","seconds":600}}`, "upstream", false, 200, 0.0045, 1, 22500},
		{"one second", `{"text":"hello","usage":{"type":"duration","seconds":1}}`, "upstream", false, 200, 0.0045, 1, 38},
		{"explicit zero", `{"text":"","usage":{"type":"duration","seconds":0}}`, "upstream", false, 200, 0.0045, 1, 0},
		{"free tariff", `{"text":"hello","usage":{"type":"duration","seconds":600}}`, "upstream", false, 200, 0, 1, 0},
		{"model discount", `{"text":"hello","usage":{"type":"duration","seconds":600}}`, "upstream", false, 200, 0.0045, 0.5, 5625},
		{"model discount failure refunds", `{"error":{"message":"test failure"}}`, "", false, 400, 0.0045, 0.5, 0},
		{"channel discount", `{"text":"hello","usage":{"type":"duration","seconds":600}}`, "upstream", false, 200, 0.0045, 0.5, 11250},
		{"missing usage refunds", `{"text":"hello"}`, "missing", false, 200, 0.0045, 1, 0},
		{"token usage cannot replace duration", `{"text":"hello","usage":{"type":"tokens","input_tokens":1000,"output_tokens":20}}`, "missing", false, 200, 0.0045, 1, 0},
		{"negative duration refunds", `{"text":"hello","usage":{"type":"duration","seconds":-1}}`, "missing", false, 200, 0.0045, 1, 0},
		{"upstream failure refunds once", `{"error":{"message":"test failure","type":"invalid_request_error"}}`, "", false, 400, 0.0045, 1, 0},
		{"terminal stream usage", "event: transcript.text.delta\ndata: {\"type\":\"transcript.text.delta\",\"delta\":\"hello\"}\n\nevent: transcript.text.done\ndata: {\"type\":\"transcript.text.done\",\"usage\":{\"type\":\"duration\",\"seconds\":9}}\n\n", "upstream", true, 200, 0.0045, 1, 338},
		{"unfinished stream refunds", "data: {\"type\":\"transcript.text.delta\",\"delta\":\"hello\"}\n\n", "missing", true, 200, 0.0045, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, common.UpdateModelDiscounts(`{}`))
			if strings.HasPrefix(tc.name, "model discount") {
				require.NoError(t, common.UpdateModelDiscounts(`{"gpt-transcribe":0.5}`))
			}
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Log{}))
			model.DB, model.LOG_DB = db, db
			config.QuotaPerUnit = 500000
			priceData, err := json.Marshal(map[string]float64{"gpt-transcribe": tc.price})
			require.NoError(t, err)
			require.NoError(t, common.UpdateAudioDurationPrices(string(priceData)))
			require.NoError(t, db.Create(&model.User{Id: 9201, Username: "audio-test", Quota: 1000000}).Error)
			require.NoError(t, db.Create(&model.Token{Id: 9202, UserId: 9201, Key: "audio-test", RemainQuota: 1000000}).Error)
			require.NoError(t, db.Create(&model.Channel{Id: 9203, Name: "audio-test"}).Error)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1/audio/transcriptions", r.URL.Path)
				assert.Equal(t, "Bearer upstream-test", r.Header.Get("Authorization"))
				assert.NoError(t, r.ParseMultipartForm(1024*1024))
				assert.Equal(t, "mapped-transcribe", r.FormValue("model"))
				assert.Equal(t, "0", r.FormValue("temperature"))
				assert.Equal(t, []string{"alpha", "beta"}, r.MultipartForm.Value["hints[]"])
				file, _, err := r.FormFile("file")
				if assert.NoError(t, err) {
					data, err := io.ReadAll(file)
					assert.NoError(t, err)
					assert.Equal(t, "audio fixture", string(data))
					file.Close()
				}
				// Tariff and quota conversion changes after dispatch must not affect settlement.
				assert.NoError(t, common.UpdateAudioDurationPrices(`{"gpt-transcribe":100}`))
				config.QuotaPerUnit = 100
				contentType := "application/json"
				if tc.stream {
					contentType = "text/event-stream"
				}
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			util.HTTPClient = server.Client()
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			require.NoError(t, writer.WriteField("model", "gpt-transcribe"))
			require.NoError(t, writer.WriteField("temperature", "0"))
			require.NoError(t, writer.WriteField("hints[]", "alpha"))
			require.NoError(t, writer.WriteField("hints[]", "beta"))
			file, err := writer.CreateFormFile("file", "sample.wav")
			require.NoError(t, err)
			_, err = io.WriteString(file, "audio fixture")
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			w := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(w)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
			ctx.Request.Header.Set("Content-Type", writer.FormDataContentType())
			ctx.Request.Header.Set("Authorization", "Bearer upstream-test")
			ctx.Set("id", 9201)
			ctx.Set("token_id", 9202)
			ctx.Set("channel_id", 9203)
			ctx.Set("channel", common.ChannelTypeOpenAI)
			ctx.Set("base_url", server.URL)
			ctx.Set("group", "audio-test")
			ctx.Set("channel_discount", tc.discount)
			ctx.Set("model_mapping", `{"gpt-transcribe":"mapped-transcribe"}`)
			apiErr := RelayAudioHelper(ctx, constant.RelayModeAudioTranscription)
			if tc.status == 200 {
				require.Nil(t, apiErr)
				assert.Equal(t, tc.body, w.Body.String())
			} else {
				require.NotNil(t, apiErr)
			}
			var user model.User
			var token model.Token
			var channel model.Channel
			require.NoError(t, db.First(&user, 9201).Error)
			require.NoError(t, db.First(&token, 9202).Error)
			require.NoError(t, db.First(&channel, 9203).Error)
			assert.Equal(t, int64(1000000)-tc.quota, user.Quota)
			assert.Equal(t, int64(1000000)-tc.quota, token.RemainQuota)
			assert.Equal(t, tc.quota, token.UsedQuota)
			assert.Equal(t, tc.quota, user.UsedQuota)
			assert.EqualValues(t, tc.quota, channel.UsedQuota)
			if tc.status == 200 {
				var log model.Log
				require.NoError(t, db.First(&log).Error)
				assert.Equal(t, "gpt-transcribe", log.ModelName)
				assert.EqualValues(t, tc.quota, log.Quota)
				var other map[string]interface{}
				require.NoError(t, json.Unmarshal([]byte(log.Other), &other))
				assert.Equal(t, "duration", other["billing_mode"])
				assert.Equal(t, tc.source, other["transcription_usage_source"])
				assert.Equal(t, tc.price, other["duration_price_per_minute"])
			}
		})
	}
}
