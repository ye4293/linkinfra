package model

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/songquanpeng/one-api/common"
)

var audioDurationPricingMu sync.Mutex

// AudioDurationPriceUpdate 使用美元/分钟；省略价格表示不修改，0 表示免费。
type AudioDurationPriceUpdate struct {
	ModelName              string   `json:"model_name" binding:"required"`
	DurationPricePerMinute *float64 `json:"duration_price_per_minute"`
	RemoveDurationPrice    bool     `json:"remove_duration_price"`
}

// UpdateAudioDurationPriceEntries 在同一锁内合并和保存，避免覆盖其他模型的配置。
func UpdateAudioDurationPriceEntries(updates []AudioDurationPriceUpdate) error {
	changed := false
	for _, update := range updates {
		if update.DurationPricePerMinute == nil && !update.RemoveDurationPrice {
			continue
		}
		changed = true
		if update.ModelName == "" || strings.TrimSpace(update.ModelName) != update.ModelName {
			return fmt.Errorf("invalid model name")
		}
		if price := update.DurationPricePerMinute; price != nil {
			if update.RemoveDurationPrice || *price < 0 || math.IsNaN(*price) || math.IsInf(*price, 0) {
				return fmt.Errorf("duration price must be finite and non-negative and cannot be removed in the same update")
			}
		}
	}
	if !changed {
		return nil
	}
	audioDurationPricingMu.Lock()
	defer audioDurationPricingMu.Unlock()
	prices := common.GetAudioDurationPrices()
	for _, update := range updates {
		if update.RemoveDurationPrice {
			delete(prices, update.ModelName)
		} else if update.DurationPricePerMinute != nil {
			prices[update.ModelName] = *update.DurationPricePerMinute
		}
	}
	value, err := json.Marshal(prices)
	if err != nil {
		return err
	}
	return updateOption(common.AudioDurationPricesOption, string(value))
}
