package model

import (
	"testing"

	"github.com/songquanpeng/one-api/common/config"
)

// TestAmountToQuotaFloatTruncation 探查浮点截断造成的系统性少算。
//
// AmountToQuota 与 GrantCommission 都是 int64(float 乘积) 的直接截断。
// float64 无法精确表示多数十进制小数，乘积常落在真值下方一点点，
// 截断后就少算 1 个 quota。金额越是「看起来是整数的小数」越容易命中。
//
// 单次误差是 1 quota（QuotaPerUnit=500000 时约 $0.000002），绝对值可忽略；
// 但它是**单向**的 —— 只会少给用户、不会多给，累积起来是系统性偏差。
// 这条测试用来量化到底有多普遍，再决定要不要改成四舍五入。
func TestAmountToQuotaFloatTruncation(t *testing.T) {
	orig := config.QuotaPerUnit
	config.QuotaPerUnit = 500000
	t.Cleanup(func() { config.QuotaPerUnit = orig })

	// 覆盖常见的充值面额与两位小数金额
	amounts := []float64{}
	for _, v := range []float64{1, 2, 5, 8.2, 9.99, 10, 19.99, 20, 29.9, 50, 99.99, 100, 200, 500} {
		amounts = append(amounts, v)
	}
	// 再扫一遍 0.01 ~ 5.00 的两位小数
	for i := 1; i <= 500; i++ {
		amounts = append(amounts, float64(i)/100)
	}

	truncated := 0
	var examples []float64
	for _, a := range amounts {
		got := AmountToQuota(a)
		// 期望值：按十进制精确计算（金额最多两位小数，×500000 必为整数）
		want := int64(a*100+0.5) * 5000 // (a*100 四舍五入取分) * (500000/100)
		if got != want {
			truncated++
			if len(examples) < 5 {
				examples = append(examples, a)
			}
		}
	}

	t.Logf("样本 %d 个，截断少算 %d 个（%.1f%%）", len(amounts), truncated,
		float64(truncated)/float64(len(amounts))*100)
	if len(examples) > 0 {
		t.Logf("示例金额: %v", examples)
		for _, a := range examples {
			t.Logf("  $%v → 实得 %d, 应得 %d", a, AmountToQuota(a), int64(a*100+0.5)*5000)
		}
	}

	if truncated > 0 {
		t.Errorf("有 %d 个金额因浮点截断少算 quota —— 资金计算应四舍五入而非截断", truncated)
	}
}
