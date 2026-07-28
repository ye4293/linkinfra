package controller

import (
	"strings"
)

// maskUsername 对用户名脱敏：保留首尾字符，中间以 * 替代。
// 长度 ≤ 2 时全部替代。
//
// 邀请人能看到自己邀请了谁的返现明细，但不该拿到对方的完整账号 ——
// 那是可以用来撞库或社工的信息。
//
// 按 rune 而非 byte 处理：中文用户名按 byte 切会切出乱码。
func maskUsername(name string) string {
	runes := []rune(name)
	switch len(runes) {
	case 0:
		return ""
	case 1, 2:
		return strings.Repeat("*", len(runes))
	default:
		return string(runes[0]) + strings.Repeat("*", len(runes)-2) + string(runes[len(runes)-1])
	}
}
