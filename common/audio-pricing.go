package common

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
)

const AudioDurationPricesOption = "AudioDurationPrices"

var audioDurationPricesMu sync.RWMutex
var audioDurationPrices = map[string]float64{"gpt-transcribe": 0.0045}

// ParseAudioDurationPrices distinguishes a free tariff (0) from no tariff.
func ParseAudioDurationPrices(value string) (map[string]float64, error) {
	var entries map[string]*float64
	if err := json.Unmarshal([]byte(value), &entries); err != nil || entries == nil {
		return nil, fmt.Errorf("audio duration prices must be a JSON object")
	}
	prices := make(map[string]float64, len(entries))
	for name, price := range entries {
		if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) || price == nil || *price < 0 || math.IsNaN(*price) || math.IsInf(*price, 0) {
			return nil, fmt.Errorf("audio duration prices require model names and finite, non-negative prices")
		}
		prices[name] = *price
	}
	return prices, nil
}

func UpdateAudioDurationPrices(value string) error {
	prices, err := ParseAudioDurationPrices(value)
	if err != nil {
		return err
	}
	audioDurationPricesMu.Lock()
	audioDurationPrices = prices
	audioDurationPricesMu.Unlock()
	return nil
}

func GetAudioDurationPrice(name string) (float64, bool) {
	audioDurationPricesMu.RLock()
	defer audioDurationPricesMu.RUnlock()
	price, ok := audioDurationPrices[name]
	return price, ok
}

func GetAudioDurationPrices() map[string]float64 {
	audioDurationPricesMu.RLock()
	defer audioDurationPricesMu.RUnlock()
	prices := make(map[string]float64, len(audioDurationPrices))
	for name, price := range audioDurationPrices {
		prices[name] = price
	}
	return prices
}

func AudioDurationPricesJSON() string {
	data, _ := json.Marshal(GetAudioDurationPrices())
	return string(data)
}
