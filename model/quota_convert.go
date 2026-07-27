package model

import "github.com/songquanpeng/one-api/common/config"

// AmountToQuota 把充值金额（主货币单位，如美元）换算为系统 quota。
//
// 这是金额到 quota 的唯一换算入口。三条充值链路（Stripe Checkout、
// Stripe 套餐、加密货币）此前有各自的实现，其中两处硬编码 500000
// 而绕过了后台可配的 config.QuotaPerUnit，导致管理员改配置后口径不一致。
//
// 非法输入（负金额、非正的 QuotaPerUnit）一律返回 0 而不是负数或 panic：
// 调用方都在充值入账路径上，宁可不加额度也不能扣成负余额。
func AmountToQuota(amount float64) int64 {
	if amount <= 0 || config.QuotaPerUnit <= 0 {
		return 0
	}
	return int64(amount * config.QuotaPerUnit)
}
