package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAudioDurationPricingOptionsAndCatalog(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Option{}))
	previousPrices, previousOptions := common.AudioDurationPricesJSON(), config.OptionMap
	previousRatios, previousFixed := common.ModelRatio, common.ModelPrice
	t.Cleanup(func() {
		require.NoError(t, common.UpdateAudioDurationPrices(previousPrices))
		config.OptionMap, common.ModelRatio, common.ModelPrice = previousOptions, previousRatios, previousFixed
	})
	config.OptionMap = map[string]string{}
	common.ModelRatio = map[string]float64{"gpt-transcribe": 30}
	common.ModelPrice = map[string]float64{"gpt-transcribe": 1}
	for _, input := range []string{`{"gpt-transcribe":0.0045,"another-model":0.006}`, `{"gpt-transcribe":0,"another-model":0.006}`} {
		body, err := json.Marshal(model.Option{Key: common.AudioDurationPricesOption, Value: input})
		require.NoError(t, err)
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option", bytes.NewReader(body))
		UpdateOption(ctx)
		var result struct {
			Success bool `json:"success"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
		require.True(t, result.Success, w.Body.String())
		var stored model.Option
		require.NoError(t, db.First(&stored, "key = ?", common.AudioDurationPricesOption).Error)
		assert.JSONEq(t, input, stored.Value)
		price, ok := common.GetAudioDurationPrice("gpt-transcribe")
		require.True(t, ok)
		assert.Equal(t, 0.006, common.GetAudioDurationPrices()["another-model"])
		w = httptest.NewRecorder()
		ctx, _ = gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/pricing?keyword=gpt-transcribe", nil)
		GetModelPrices(ctx)
		var catalog struct {
			Data struct {
				List []ModelPriceInfo `json:"list"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &catalog))
		require.Len(t, catalog.Data.List, 1)
		entry := catalog.Data.List[0]
		assert.Equal(t, "duration", entry.PriceType)
		require.NotNil(t, entry.DurationPricePerMinute)
		assert.Equal(t, price, *entry.DurationPricePerMinute)
		assert.Zero(t, entry.FixedPrice)
		assert.Zero(t, entry.InputPrice)
	}
	require.Error(t, model.UpdateOption(common.AudioDurationPricesOption, `{"gpt-transcribe":null}`))
	price, enabled := common.GetAudioDurationPrice("gpt-transcribe")
	assert.True(t, enabled)
	assert.Zero(t, price)
}

func TestAudioDurationConcurrentModelUpdates(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Option{}))
	previousPrices, previousOptions := common.AudioDurationPricesJSON(), config.OptionMap
	t.Cleanup(func() {
		require.NoError(t, common.UpdateAudioDurationPrices(previousPrices))
		config.OptionMap = previousOptions
	})
	config.OptionMap = map[string]string{}
	require.NoError(t, common.UpdateAudioDurationPrices(`{"existing":0.01}`))
	var wg sync.WaitGroup
	errors := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			price := float64(i) / 1000
			errors <- model.UpdateAudioDurationPriceEntries([]model.AudioDurationPriceUpdate{{ModelName: fmt.Sprintf("audio-%d", i), DurationPricePerMinute: &price}})
		}(i)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	prices := common.GetAudioDurationPrices()
	require.Len(t, prices, 21)
	assert.Equal(t, 0.01, prices["existing"])
	for i := 0; i < 20; i++ {
		assert.Equal(t, float64(i)/1000, prices[fmt.Sprintf("audio-%d", i)])
	}
	var stored model.Option
	require.NoError(t, db.First(&stored, "key = ?", common.AudioDurationPricesOption).Error)
	assert.JSONEq(t, common.AudioDurationPricesJSON(), stored.Value)
}

func TestAudioDurationPricingModelEndpoints(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Option{}))
	previousPrices, previousOptions := common.AudioDurationPricesJSON(), config.OptionMap
	t.Cleanup(func() {
		require.NoError(t, common.UpdateAudioDurationPrices(previousPrices))
		config.OptionMap = previousOptions
	})
	config.OptionMap = map[string]string{}
	require.NoError(t, common.UpdateAudioDurationPrices(`{"existing":0.01}`))

	request := func(handler gin.HandlerFunc, body string, success bool) {
		t.Helper()
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/pricing/model", bytes.NewBufferString(body))
		handler(ctx)
		var result struct {
			Success bool `json:"success"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
		require.Equal(t, success, result.Success, w.Body.String())
	}
	request(UpdateModelRatio, `{"model_name":"whisper-1","duration_price_per_minute":0.006}`, true)
	assert.Equal(t, map[string]float64{"existing": 0.01, "whisper-1": 0.006}, common.GetAudioDurationPrices())
	request(BatchUpdateModelRatio, `{"models":[{"model_name":"whisper-1","duration_price_per_minute":0},{"model_name":"custom-transcribe","duration_price_per_minute":0.0045}]}`, true)
	assert.Equal(t, 0.0, common.GetAudioDurationPrices()["whisper-1"])
	assert.Equal(t, 0.0045, common.GetAudioDurationPrices()["custom-transcribe"])
	for _, body := range []string{
		`{"model_name":"whisper-1","duration_price_per_minute":-1}`,
		`{"model_name":" whisper-1","duration_price_per_minute":0.006}`,
		`{"model_name":"whisper-1","duration_price_per_minute":0.006,"remove_duration_price":true}`,
		`{"model_name":"whisper-1","duration_price_per_minute":"0.006"}`,
		`{"model_name":"whisper-1","duration_price_per_minute":1e999}`,
	} {
		request(UpdateModelRatio, body, false)
	}
	request(BatchUpdateModelRatio, `{"models":[{"model_name":"existing","duration_price_per_minute":1},{"model_name":"whisper-1","duration_price_per_minute":-1}]}`, false)
	assert.Equal(t, 0.01, common.GetAudioDurationPrices()["existing"])
	request(UpdateModelRatio, `{"model_name":"whisper-1"}`, true)
	price, exists := common.GetAudioDurationPrice("whisper-1")
	assert.True(t, exists)
	assert.Zero(t, price)
	request(UpdateModelRatio, `{"model_name":"whisper-1","remove_duration_price":true}`, true)
	_, exists = common.GetAudioDurationPrice("whisper-1")
	assert.False(t, exists)
	var stored model.Option
	require.NoError(t, db.First(&stored, "key = ?", common.AudioDurationPricesOption).Error)
	assert.JSONEq(t, `{"existing":0.01,"custom-transcribe":0.0045}`, stored.Value)

	// 数据库写入失败时不得先修改运行中的价格。
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	request(UpdateModelRatio, `{"model_name":"existing","duration_price_per_minute":10}`, false)
	assert.Equal(t, 0.01, common.GetAudioDurationPrices()["existing"])
}
