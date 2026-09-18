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
	"github.com/stretchr/testify/require"
)

func TestModelDiscountPricingEndpoints(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Option{}, &model.Channel{}, &model.GroupConfig{}))
	previous, options := common.ModelDiscountsJSON(), config.OptionMap
	ratios, fixed := common.ModelRatio, common.ModelPrice
	t.Cleanup(func() {
		require.NoError(t, common.UpdateModelDiscounts(previous))
		config.OptionMap, common.ModelRatio, common.ModelPrice = options, ratios, fixed
	})
	config.OptionMap = map[string]string{}
	common.ModelRatio = map[string]float64{"discount-test": 2.5}
	common.ModelPrice = map[string]float64{"fixed-test": 0.1}
	require.NoError(t, common.UpdateModelDiscounts(`{}`))
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
	request(UpdateModelRatio, `{"model_name":"discount-test","model_discount":0.53}`, true)
	require.Equal(t, 0.53, common.GetModelDiscount("discount-test"))
	request(UpdateModelRatio, `{"model_name":"discount-test"}`, true)
	require.Equal(t, 0.53, common.GetModelDiscount("discount-test"))
	request(BatchUpdateModelRatio, `{"models":[{"model_name":"discount-test","model_discount":0.8},{"model_name":"fixed-test","model_discount":0.5}]}`, true)
	request(BatchUpdateModelRatio, `{"models":[{"model_name":"discount-test","model_discount":0.1},{"model_name":"fixed-test","model_discount":2}]}`, false)
	require.Equal(t, 0.8, common.GetModelDiscount("discount-test"))
	request(UpdateModelRatio, `{"model_name":"discount-test","model_discount":-1,"model_ratio":99}`, false)
	require.Equal(t, 2.5, common.ModelRatio["discount-test"])
	prices := buildPriceMap()
	require.Equal(t, 5.0, prices["discount-test"].InputPrice)
	require.Equal(t, 0.8, prices["discount-test"].ModelDiscount)
	require.Equal(t, 0.1, prices["fixed-test"].FixedPrice)
	require.Equal(t, 0.5, prices["fixed-test"].ModelDiscount)
	channelDiscount := 0.5
	require.NoError(t, db.Create(&model.Channel{Name: "test", Status: common.ChannelStatusEnabled, Models: "discount-test,fixed-test", Discount: &channelDiscount}).Error)
	require.NoError(t, db.Create(&model.GroupConfig{GroupKey: "test", DisplayName: "Test", Discount: 0.8}).Error)
	detail := getModelPricing("discount-test")
	require.NotNil(t, detail)
	require.Equal(t, 5.0, detail.BaseInputPrice)
	require.InDelta(t, 1.6, detail.GroupPrices[0].FinalInputPrice, 1e-12)
	require.InDelta(t, 0.32, detail.GroupPrices[0].CombinedDiscount, 1e-12)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/model-plaza", nil)
	GetModelPlaza(ctx)
	var catalog struct {
		Data ModelPlazaResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &catalog))
	require.Len(t, catalog.Data.Models, 2)
	for _, item := range catalog.Data.Models {
		require.Equal(t, common.GetModelDiscount(item.ModelName), item.ModelDiscount)
		if item.PriceType == "fixed" {
			require.Equal(t, 0.1, item.BaseFixedPrice)
			require.InDelta(t, 0.02, item.GroupPrices[0].FinalFixedPrice, 1e-12)
		} else {
			require.InDelta(t, 1.6, item.GroupPrices[0].FinalInputPrice, 1e-12)
		}
	}
	var stored model.Option
	require.NoError(t, db.First(&stored, "key = ?", common.ModelDiscountOption).Error)
	require.JSONEq(t, `{"discount-test":0.8,"fixed-test":0.5}`, stored.Value)
	require.Error(t, model.UpdateOption(common.ModelDiscountOption, `{"discount-test":null}`))
	request(UpdateModelRatio, `{"model_name":"discount-test","model_discount":1}`, true)
	require.Equal(t, 1.0, common.GetModelDiscount("discount-test"))
}
