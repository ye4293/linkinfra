package model

import (
	"testing"

	"github.com/songquanpeng/one-api/common/config"
)

// TestUpdateOptionMapQuotaPerUnitWhitespace 复现一个真实缺陷：
//
// controller/option.go 的校验用 strconv.ParseFloat(strings.TrimSpace(v))，
// 但落库时 model.UpdateOption(key, option.Value) 存的是**未 trim 的原值**。
// Go 的 ParseFloat 不接受前导/尾随空格，于是：
//
//	管理员粘贴 " 500000"（带前导空格，复制粘贴极常见）
//	→ 校验 trim 后通过，接口返回 success: true
//	→ DB 写入 " 500000"
//	→ updateOptionMap 解析失败，走保留旧值分支，只打一行 SysError
//	→ 管理员以为改成功了，实际汇率没变；且这个坏值每次重启都加载失败，
//	  QuotaPerUnit 永久停在编译期默认值
//
// 修复方式是在落库前统一 trim，让校验与存储看到同一个值。
func TestUpdateOptionMapQuotaPerUnitWhitespace(t *testing.T) {
	const sentinel = 500000.0

	setupOptionMap := func(t *testing.T) {
		t.Helper()
		config.OptionMapRWMutex.Lock()
		orig := config.OptionMap
		config.OptionMap = map[string]string{}
		config.OptionMapRWMutex.Unlock()
		t.Cleanup(func() {
			config.OptionMapRWMutex.Lock()
			config.OptionMap = orig
			config.OptionMapRWMutex.Unlock()
		})
	}

	cases := []struct {
		name  string
		value string
		want  float64
	}{
		{"前导空格", " 1000000", 1000000},
		{"尾随空格", "1000000 ", 1000000},
		{"两侧空格", "  1000000  ", 1000000},
		{"制表符与换行", "\t1000000\n", 1000000},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			setupOptionMap(t)
			origVal := config.QuotaPerUnit
			config.QuotaPerUnit = sentinel
			t.Cleanup(func() { config.QuotaPerUnit = origVal })

			if err := updateOptionMap("QuotaPerUnit", tt.value); err != nil {
				t.Fatalf("updateOptionMap 不应返回错误: %v", err)
			}
			if config.QuotaPerUnit != tt.want {
				t.Errorf("QuotaPerUnit = %v, want %v —— 带空白的合法数字应被接受，"+
					"否则管理员会遇到「保存成功但汇率没变」", config.QuotaPerUnit, tt.want)
			}
		})
	}
}
