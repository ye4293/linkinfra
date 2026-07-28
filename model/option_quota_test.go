package model

import (
	"testing"

	"github.com/songquanpeng/one-api/common/config"
)

// TestUpdateOptionMapQuotaPerUnitRejectsBadValues QuotaPerUnit 是「$1 换多少
// quota」的汇率，充值入账直接乘它。此前 strconv.ParseFloat 的错误被丢弃，
// 非法输入会让它静默变成 0 —— 之后所有充值都入账 0 quota（AmountToQuota
// 的守卫），而消费侧硬编码 500000 仍照常扣费，用户付了钱拿不到额度。
func TestUpdateOptionMapQuotaPerUnitRejectsBadValues(t *testing.T) {
	const sentinel = 500000.0

	// updateOptionMap 第一行就写 config.OptionMap，生产环境由 InitOptionMap
	// 建好；测试里需要自己准备，否则会 panic on nil map。
	setupOptionMap := func(t *testing.T) {
		t.Helper()
		config.OptionMapRWMutex.Lock()
		origMap := config.OptionMap
		config.OptionMap = map[string]string{}
		config.OptionMapRWMutex.Unlock()
		t.Cleanup(func() {
			config.OptionMapRWMutex.Lock()
			config.OptionMap = origMap
			config.OptionMapRWMutex.Unlock()
		})
	}

	bad := []struct {
		name  string
		value string
	}{
		{"非数字", "abc"},
		{"带中文单位", "50万"},
		{"空串", ""},
		{"零", "0"},
		{"负数", "-500000"},
	}

	for _, tt := range bad {
		t.Run("拒绝_"+tt.name, func(t *testing.T) {
			setupOptionMap(t)
			orig := config.QuotaPerUnit
			config.QuotaPerUnit = sentinel
			t.Cleanup(func() { config.QuotaPerUnit = orig })

			if err := updateOptionMap("QuotaPerUnit", tt.value); err != nil {
				t.Fatalf("updateOptionMap 不应返回错误: %v", err)
			}
			if config.QuotaPerUnit != sentinel {
				t.Errorf("QuotaPerUnit 被改成了 %v，应保留旧值 %v", config.QuotaPerUnit, sentinel)
			}
		})
	}

	good := []struct {
		name  string
		value string
		want  float64
	}{
		{"正常值", "500000", 500000},
		{"改大", "1000000", 1000000},
		{"小数", "0.5", 0.5},
	}

	for _, tt := range good {
		t.Run("接受_"+tt.name, func(t *testing.T) {
			setupOptionMap(t)
			orig := config.QuotaPerUnit
			config.QuotaPerUnit = sentinel
			t.Cleanup(func() { config.QuotaPerUnit = orig })

			if err := updateOptionMap("QuotaPerUnit", tt.value); err != nil {
				t.Fatalf("updateOptionMap 不应返回错误: %v", err)
			}
			if config.QuotaPerUnit != tt.want {
				t.Errorf("QuotaPerUnit = %v, want %v", config.QuotaPerUnit, tt.want)
			}
		})
	}
}
