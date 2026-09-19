package common

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
)

const ModelDiscountOption = "ModelDiscount"

var modelDiscountMu sync.RWMutex
var modelDiscounts = map[string]float64{}

func ValidateModelDiscount(name string, discount float64) error {
	if name == "" || strings.TrimSpace(name) != name || math.IsNaN(discount) || math.IsInf(discount, 0) || discount <= 0 || discount > 1 {
		return fmt.Errorf("model discount requires a model name and a finite number greater than 0 and at most 1")
	}
	return nil
}

func ParseModelDiscounts(value string) (map[string]float64, error) {
	var entries map[string]*float64
	if err := json.Unmarshal([]byte(value), &entries); err != nil || entries == nil {
		return nil, fmt.Errorf("model discounts must be a JSON object")
	}
	discounts := make(map[string]float64, len(entries))
	for name, discount := range entries {
		if discount == nil {
			return nil, fmt.Errorf("model discount cannot be null")
		}
		if err := ValidateModelDiscount(name, *discount); err != nil {
			return nil, err
		}
		discounts[name] = *discount
	}
	return discounts, nil
}

func UpdateModelDiscounts(value string) error {
	discounts, err := ParseModelDiscounts(value)
	if err != nil {
		return err
	}
	modelDiscountMu.Lock()
	modelDiscounts = discounts
	modelDiscountMu.Unlock()
	return nil
}

func GetModelDiscount(name string) float64 {
	modelDiscountMu.RLock()
	defer modelDiscountMu.RUnlock()
	if discount, ok := modelDiscounts[name]; ok {
		return discount
	}
	return 1
}

func GetModelDiscounts() map[string]float64 {
	modelDiscountMu.RLock()
	defer modelDiscountMu.RUnlock()
	discounts := make(map[string]float64, len(modelDiscounts))
	for name, discount := range modelDiscounts {
		discounts[name] = discount
	}
	return discounts
}

func ModelDiscountsJSON() string {
	data, _ := json.Marshal(GetModelDiscounts())
	return string(data)
}
