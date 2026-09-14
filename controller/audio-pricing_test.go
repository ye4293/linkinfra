package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
