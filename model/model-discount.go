package model

import (
	"encoding/json"
	"github.com/songquanpeng/one-api/common"
	"sync"
)

var modelDiscountUpdateMu sync.Mutex

// UpdateModelDiscountEntries 合并保存，省略字段不会覆盖已有折扣。
func UpdateModelDiscountEntries(updates map[string]float64) error {
	if len(updates) == 0 {
		return nil
	}
	for name, discount := range updates {
		if err := common.ValidateModelDiscount(name, discount); err != nil {
			return err
		}
	}
	modelDiscountUpdateMu.Lock()
	defer modelDiscountUpdateMu.Unlock()
	discounts := common.GetModelDiscounts()
	for name, discount := range updates {
		discounts[name] = discount
	}
	data, err := json.Marshal(discounts)
	if err != nil {
		return err
	}
	return updateOption(common.ModelDiscountOption, string(data))
}
