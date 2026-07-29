package model

import (
	"math"

	"github.com/songquanpeng/one-api/common/config"
)

// AmountToQuota 把充值金额（主货币单位，如美元）换算为系统 quota。
//
// 这是金额到 quota 的唯一换算入口。三条充值链路（Stripe Checkout、
// Stripe 套餐、加密货币）此前有各自的实现，其中两处硬编码 500000
// 而绕过了后台可配的 config.QuotaPerUnit，导致管理员改配置后口径不一致。
//
// 用 math.Round 而非直接 int64() 截断：float64 无法精确表示多数十进制
// 小数，乘积常落在真值下方一点点，截断后就少算 1 个 quota。实测在常见
// 充值面额与两位小数金额中约 2.1% 会命中（例如 $8.2 得 4099999 而非
// 4100000）。单次误差约 $0.000002 可忽略，但它是**单向**的 —— 只会少给
// 用户、永不多给，是系统性偏差。资金计算应四舍五入。
//
// 非法输入（负金额、非正的 QuotaPerUnit）一律返回 0 而不是负数或 panic：
// 调用方都在充值入账路径上，宁可不加额度也不能扣成负余额。
func AmountToQuota(amount float64) int64 {
	if amount <= 0 || config.QuotaPerUnit <= 0 {
		return 0
	}
	return int64(math.Round(amount * config.QuotaPerUnit))
}
