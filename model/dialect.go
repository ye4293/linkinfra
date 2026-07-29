package model

import "github.com/songquanpeng/one-api/common"

// likeOp 返回当前数据库下「大小写不敏感的模糊匹配」运算符。
//
// 三库的默认行为并不一致：
//   - MySQL：默认排序规则 utf8mb4_general_ci 是大小写不敏感的，LIKE 即可
//   - sqlite：LIKE 对 ASCII 默认大小写不敏感
//   - PostgreSQL：LIKE 严格区分大小写，必须用 ILIKE
//
// 不加区分地用 LIKE 会让运营在 PG 上搜 "GPT-4" 什么都搜不到，而同样的
// 关键词在 MySQL 上能搜到 —— 是一种迁库后才暴露、且不报错的行为回退。
func likeOp() string {
	if common.UsingPostgreSQL {
		return "ILIKE"
	}
	return "LIKE"
}
