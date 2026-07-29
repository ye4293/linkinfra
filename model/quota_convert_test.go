package model

import (
	"testing"

	"github.com/songquanpeng/one-api/common/config"
)

func TestAmountToQuota(t *testing.T) {
	// config.QuotaPerUnit 是全局可变配置，测试内临时改写后还原
	orig := config.QuotaPerUnit
	t.Cleanup(func() { config.QuotaPerUnit = orig })

	tests := []struct {
		name         string
		quotaPerUnit float64
		amount       float64
		want         int64
	}{
		{"默认口径 $1", 500000, 1, 500000},
		{"默认口径 $100", 500000, 100, 50000000},
		// 换算用 math.Round 而非截断：截断会单向少给用户（详见
		// AmountToQuota 的注释与 TestAmountToQuotaFloatTruncation 的量化）。
		{"不足1quota且过半向上舍入", 500000, 0.0000019, 1},  // 0.95 → 1
		{"不足1quota且不过半舍去", 500000, 0.0000009, 0},   // 0.45 → 0
		{"恰好半个quota向上舍入", 500000, 0.000001, 1},     // 0.50 → 1
		{"金额为 0", 500000, 0, 0},
		{"负金额一律返回 0", 500000, -5, 0},
		{"管理员改过 QuotaPerUnit 后跟随生效", 1000000, 100, 100000000},
		{"QuotaPerUnit 为 0 时返回 0", 0, 100, 0},
		{"QuotaPerUnit 为负时返回 0", -500000, 100, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config.QuotaPerUnit = tt.quotaPerUnit
			if got := AmountToQuota(tt.amount); got != tt.want {
				t.Errorf("AmountToQuota(%v) with QuotaPerUnit=%v = %d, want %d",
					tt.amount, tt.quotaPerUnit, got, tt.want)
			}
		})
	}
}
