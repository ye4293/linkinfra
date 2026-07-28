# P4: 邀请返现查询接口 —— 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 提供 4 个查询接口，让前端（`~/code/ezlinkai-web`，独立仓库）能展示邀请汇总、返现明细、被邀请人列表，以及管理员侧的全局返现报表。

**Architecture:** 纯读接口，不产生任何写操作。model 层提供查询函数并写单元测试，controller 层做参数解析、权限与脱敏。用户名一律脱敏后返回——邀请人不该看到被邀请人的完整账号。

**Tech Stack:** Go 1.24.5、GORM 1.25.7、gin

**依赖:** P1（`setupTestDB`）、P2（`aff_commission_records` 表、`GetAffCommissionSummary`）、P3（返现数据已在真实链路中产生）

**设计文档:** `docs/superpowers/specs/2026-07-27-invite-commission-by-level-design.md` §8

---

## 本期范围调整

设计文档 §8 原本把 `topup_quota` 历史回填划在 P4，但它已在 P3 完成（`model/migration_topup_quota.go`）——因为 `RecalcUserLevel` 依赖它，留到 P4 会让 P3 的安全性依赖部署顺序。**本期只做接口。**

## 现有约定（必须遵循）

响应格式（参考 `controller/topup.go:97-105`）：

```go
c.JSON(http.StatusOK, gin.H{
    "success": true,
    "message": "",
    "data": gin.H{
        "list":        items,
        "currentPage": page,
        "pageSize":    pageSize,
        "total":       total,
    },
})
```

- 分页参数是 `page` 与 **`pagesize`**（全小写，不是 `pageSize`）
- `page < 1` 时归一为 1；`pagesize <= 0` 时默认 10
- 错误一律返回 HTTP 200 + `success: false`，不用 4xx/5xx
- 当前用户 id 从 `c.GetInt("id")` 取，角色从 `c.GetInt("role")`

---

## 文件结构

| 文件 | 责任 |
|---|---|
| `model/aff_query.go`（新建） | 4 个接口所需的查询函数，纯读 |
| `model/aff_query_test.go`（新建） | 查询函数的单元测试 |
| `controller/aff.go`（新建） | 4 个 handler + 用户名脱敏工具 |
| `controller/aff_test.go`（新建） | 脱敏函数的单元测试 |
| `router/api-router.go`（修改） | 注册 4 条路由 |

`controller/` 包目前零测试。脱敏是纯函数，不需要 DB，可以直接建测试文件而无需测试基座。

---

## Task 1: 用户名脱敏

**Files:**
- Create: `controller/aff.go`
- Create: `controller/aff_test.go`

邀请人不该看到被邀请人的完整账号。脱敏规则：保留首尾字符，中间以 `*` 替代；长度 ≤ 2 时全部替代。

- [ ] **Step 1: 写失败的测试**

`controller/aff_test.go`：

```go
package controller

import "testing"

func TestMaskUsername(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"空串", "", ""},
		{"单字符", "a", "*"},
		{"两字符", "ab", "**"},
		{"三字符", "abc", "a*c"},
		{"常见长度", "zhangsan", "z******n"},
		{"中文", "张三丰", "张*丰"},
		// 中文按 rune 处理，不能按 byte 切 —— 按 byte 会切出乱码
		{"中文长名", "王小明同学", "王***学"},
		// user张三 是 6 个 rune（u/s/e/r/张/三），保留首尾后中间 4 个
		{"中英混合", "user张三", "u****三"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maskUsername(tt.in); got != tt.want {
				t.Errorf("maskUsername(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./controller/ -run TestMaskUsername -v`

Expected: 编译失败 —— `undefined: maskUsername`

- [ ] **Step 3: 写实现**

`controller/aff.go`：

```go
package controller

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/model"
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
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./controller/ -run TestMaskUsername -v`

Expected: 8 个子测试全 PASS

- [ ] **Step 5: 提交**

```bash
git add controller/aff.go controller/aff_test.go
git commit -m "feat(invite): 新增用户名脱敏工具

邀请人能看到自己邀请了谁的返现明细，但不该拿到对方的完整账号 ——
那是可以用来撞库或社工的信息。保留首尾字符、中间以 * 替代，
长度 <= 2 时全部替代。

按 rune 而非 byte 处理，否则中文用户名会被切出乱码。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 2: model 层查询函数

**Files:**
- Create: `model/aff_query.go`
- Create: `model/aff_query_test.go`

- [ ] **Step 1: 写失败的测试**

`model/aff_query_test.go`：

```go
package model

import "testing"

// affQueryFixture 造 1 个邀请人 + 3 个被邀请人 + 若干返现记录。
func affQueryFixture(t *testing.T) (inviterId int) {
	t.Helper()

	inviter := &User{Username: "inviter", Group: "Lv4", AffCode: "inv", AccessToken: "t-inv"}
	if err := DB.Create(inviter).Error; err != nil {
		t.Fatalf("create inviter failed: %v", err)
	}

	// 3 个被邀请人，其中 2 个有充值记录
	names := []string{"alice", "bob", "carol"}
	ids := make([]int, len(names))
	for i, n := range names {
		u := &User{
			Username: n, Group: "Lv1", AffCode: n, AccessToken: "t-" + n,
			InviterId: inviter.Id,
		}
		if err := DB.Create(u).Error; err != nil {
			t.Fatalf("create %s failed: %v", n, err)
		}
		ids[i] = u.Id
	}
	// alice 与 bob 有充值
	_ = DB.Model(&User{}).Where("id = ?", ids[0]).Update("topup_quota", 50000000).Error
	_ = DB.Model(&User{}).Where("id = ?", ids[1]).Update("topup_quota", 10000000).Error

	// 别人的被邀请人，不能串进来
	other := &User{Username: "other", AffCode: "oth", AccessToken: "t-oth", InviterId: 99999}
	if err := DB.Create(other).Error; err != nil {
		t.Fatalf("create other failed: %v", err)
	}

	recs := []AffCommissionRecord{
		{InviterId: inviter.Id, InviteeId: ids[0], InviteeUsername: "alice",
			SourceType: SourceTypeStripeCheckout, SourceNo: "r1",
			TopupAmount: 100, Rate: 0.08, InviterGroup: "Lv4",
			CommissionQuota: 4000000, Status: AffCommissionStatusGranted, CreatedAt: 300},
		{InviterId: inviter.Id, InviteeId: ids[1], InviteeUsername: "bob",
			SourceType: SourceTypeStripeCharge, SourceNo: "r2",
			TopupAmount: 20, Rate: 0.08, InviterGroup: "Lv4",
			CommissionQuota: 800000, Status: AffCommissionStatusGranted, CreatedAt: 200},
		{InviterId: inviter.Id, InviteeId: ids[0], InviteeUsername: "alice",
			SourceType: SourceTypeStripeCheckout, SourceNo: "r3",
			TopupAmount: 50, Rate: 0.08, InviterGroup: "Lv4",
			CommissionQuota: 2000000, Status: AffCommissionStatusReversed,
			ReversedQuota: 2000000, CreatedAt: 100},
		// 别人的返现记录
		{InviterId: 99999, InviteeId: other.Id, InviteeUsername: "other",
			SourceNo: "r4", CommissionQuota: 999, Status: AffCommissionStatusGranted, CreatedAt: 400},
	}
	for i := range recs {
		if err := DB.Create(&recs[i]).Error; err != nil {
			t.Fatalf("create record failed: %v", err)
		}
	}
	return inviter.Id
}

func TestGetAffCommissionRecords(t *testing.T) {
	setupTestDB(t, &User{}, &AffCommissionRecord{})
	inviterId := affQueryFixture(t)

	records, total, err := GetAffCommissionRecords(inviterId, 1, 10)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3（含已冲正的，明细要能看到全部）", total)
	}
	if len(records) != 3 {
		t.Fatalf("len = %d, want 3", len(records))
	}
	// 按 created_at 倒序：r1(300) > r2(200) > r3(100)
	wantOrder := []string{"r1", "r2", "r3"}
	for i, want := range wantOrder {
		if records[i].SourceNo != want {
			t.Errorf("位置 %d = %q, want %q", i, records[i].SourceNo, want)
		}
	}
	// 不能串到别人的记录
	for _, r := range records {
		if r.InviterId != inviterId {
			t.Errorf("串入了他人记录: %+v", r)
		}
	}
}

func TestGetAffCommissionRecordsPaging(t *testing.T) {
	setupTestDB(t, &User{}, &AffCommissionRecord{})
	inviterId := affQueryFixture(t)

	page1, total, err := GetAffCommissionRecords(inviterId, 1, 2)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3（total 是全量，不受分页影响）", total)
	}
	if len(page1) != 2 {
		t.Errorf("第一页 len = %d, want 2", len(page1))
	}

	page2, _, err := GetAffCommissionRecords(inviterId, 2, 2)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(page2) != 1 {
		t.Errorf("第二页 len = %d, want 1", len(page2))
	}
	if page2[0].SourceNo == page1[0].SourceNo {
		t.Error("第二页与第一页内容重复，offset 计算有误")
	}
}

func TestGetInvitees(t *testing.T) {
	setupTestDB(t, &User{}, &AffCommissionRecord{})
	inviterId := affQueryFixture(t)

	invitees, total, err := GetInvitees(inviterId, 1, 10)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(invitees) != 3 {
		t.Fatalf("len = %d, want 3", len(invitees))
	}

	paid := 0
	for _, iv := range invitees {
		if iv.HasPaid {
			paid++
		}
	}
	if paid != 2 {
		t.Errorf("已充值人数 = %d, want 2", paid)
	}
}

func TestGetAffStats(t *testing.T) {
	setupTestDB(t, &User{}, &AffCommissionRecord{})
	inviterId := affQueryFixture(t)

	stats, err := GetAffStats(inviterId)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if stats.InviteeCount != 3 {
		t.Errorf("InviteeCount = %d, want 3", stats.InviteeCount)
	}
	if stats.PaidInviteeCount != 2 {
		t.Errorf("PaidInviteeCount = %d, want 2", stats.PaidInviteeCount)
	}
	// 已冲正的 2000000 不计入累计收益
	if stats.TotalCommission != 4800000 {
		t.Errorf("TotalCommission = %d, want 4800000", stats.TotalCommission)
	}
	if stats.CommissionCount != 2 {
		t.Errorf("CommissionCount = %d, want 2", stats.CommissionCount)
	}
}

func TestGetAffReport(t *testing.T) {
	setupTestDB(t, &User{}, &AffCommissionRecord{})
	affQueryFixture(t)

	report, err := GetAffReport(10)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	// 全局：inviter 的 4000000+800000，加上 99999 的 999
	if report.TotalCommission != 4800999 {
		t.Errorf("TotalCommission = %d, want 4800999", report.TotalCommission)
	}
	if report.TotalReversed != 2000000 {
		t.Errorf("TotalReversed = %d, want 2000000", report.TotalReversed)
	}
	if len(report.TopInviters) == 0 {
		t.Fatal("TopInviters 为空")
	}
	// 按返现额降序，第一名应是发放了 4800000 的那个
	if report.TopInviters[0].TotalCommission != 4800000 {
		t.Errorf("第一名返现额 = %d, want 4800000", report.TopInviters[0].TotalCommission)
	}
}
```

**已核实**：`model/user.go` 的 `User` 结构体**没有创建时间字段**（`users` 表无 `created_at` / `created_time` 列）。因此 `GetInvitees` 返回的结构体里**不包含被邀请人的注册时间**——加列属于 schema 变更，不在本期范围。若前端需要展示注册时间，需另立一期做迁移。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./model/ -run "AffCommissionRecords|GetInvitees|GetAffStats|GetAffReport" -v`

Expected: 编译失败 —— `undefined: GetAffCommissionRecords` 等

- [ ] **Step 3: 写实现**

`model/aff_query.go`：

```go
package model

// AffStats 邀请汇总统计，供 GET /api/user/aff/stats 使用。
type AffStats struct {
	AffCode          string  `json:"aff_code"`
	InviteeCount     int64   `json:"invitee_count"`      // 已邀请人数
	PaidInviteeCount int64   `json:"paid_invitee_count"` // 其中已充值的人数
	TotalCommission  int64   `json:"total_commission"`   // 累计返现（不含已冲正）
	CommissionCount  int64   `json:"commission_count"`   // 有效返现笔数
	CurrentGroup     string  `json:"current_group"`      // 当前等级
	CommissionRate   float64 `json:"commission_rate"`    // 当前等级的返现比例
}

// InviteeItem 被邀请人列表项。
//
// 不含注册时间：users 表没有创建时间列，加列属于 schema 变更，不在本期范围。
type InviteeItem struct {
	UserId     int    `json:"user_id"`
	Username   string `json:"username"` // controller 层脱敏后返回
	Group      string `json:"group"`
	HasPaid    bool   `json:"has_paid"`
	TopupQuota int64  `json:"topup_quota"`
}

// TopInviter 管理员报表里的推广人排行项。
type TopInviter struct {
	InviterId       int    `json:"inviter_id"`
	InviterUsername string `json:"inviter_username"`
	TotalCommission int64  `json:"total_commission"`
	RecordCount     int64  `json:"record_count"`
}

// AffReport 管理员侧全局返现报表。
type AffReport struct {
	TotalCommission int64        `json:"total_commission"` // 全局累计发放（不含已冲正）
	TotalReversed   int64        `json:"total_reversed"`   // 全局累计冲正额
	ReversedLoss    int64        `json:"reversed_loss"`    // 冲正时因余额不足没扣回的差额
	TopInviters     []TopInviter `json:"top_inviters"`
}

// GetAffStats 汇总某个邀请人的邀请与返现情况。
func GetAffStats(inviterId int) (*AffStats, error) {
	stats := &AffStats{}
	if inviterId <= 0 {
		return stats, nil
	}

	var user User
	if err := DB.Where("id = ?", inviterId).First(&user).Error; err != nil {
		return nil, err
	}
	stats.AffCode = user.AffCode
	stats.CurrentGroup = user.Group

	// 分组配置可能不存在（野分组），此时比例按 0 处理而非报错
	if gc, err := GetGroupConfigByKey(user.Group); err == nil {
		stats.CommissionRate = gc.CommissionRate
	}

	if err := DB.Model(&User{}).Where("inviter_id = ?", inviterId).
		Count(&stats.InviteeCount).Error; err != nil {
		return nil, err
	}
	if err := DB.Model(&User{}).Where("inviter_id = ? AND topup_quota > 0", inviterId).
		Count(&stats.PaidInviteeCount).Error; err != nil {
		return nil, err
	}

	total, count, err := GetAffCommissionSummary(inviterId)
	if err != nil {
		return nil, err
	}
	stats.TotalCommission = total
	stats.CommissionCount = count

	return stats, nil
}

// GetAffCommissionRecords 分页查询某个邀请人的返现明细，按时间倒序。
// 已冲正的记录也会返回 —— 用户需要看到"这笔为什么被扣回"。
func GetAffCommissionRecords(inviterId, page, pageSize int) ([]AffCommissionRecord, int64, error) {
	var records []AffCommissionRecord
	var total int64
	if inviterId <= 0 {
		return records, 0, nil
	}
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 10
	}

	if err := DB.Model(&AffCommissionRecord{}).Where("inviter_id = ?", inviterId).
		Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := DB.Where("inviter_id = ?", inviterId).
		Order("created_at desc, id desc").
		Limit(pageSize).Offset((page - 1) * pageSize).
		Find(&records).Error
	if err != nil {
		return nil, total, err
	}
	return records, total, nil
}

// GetInvitees 分页查询某个邀请人邀请的用户。
func GetInvitees(inviterId, page, pageSize int) ([]InviteeItem, int64, error) {
	items := []InviteeItem{}
	var total int64
	if inviterId <= 0 {
		return items, 0, nil
	}
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 10
	}

	if err := DB.Model(&User{}).Where("inviter_id = ?", inviterId).
		Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var users []User
	err := DB.Where("inviter_id = ?", inviterId).
		Order("id desc").
		Limit(pageSize).Offset((page - 1) * pageSize).
		Find(&users).Error
	if err != nil {
		return nil, total, err
	}

	for i := range users {
		items = append(items, InviteeItem{
			UserId:     users[i].Id,
			Username:   users[i].Username,
			Group:      users[i].Group,
			HasPaid:    users[i].TopupQuota > 0,
			TopupQuota: users[i].TopupQuota,
		})
	}
	return items, total, nil
}

// GetAffReport 生成管理员侧的全局返现报表。
func GetAffReport(topN int) (*AffReport, error) {
	if topN <= 0 {
		topN = 10
	}
	report := &AffReport{TopInviters: []TopInviter{}}

	if err := DB.Model(&AffCommissionRecord{}).
		Where("status = ?", AffCommissionStatusGranted).
		Select("COALESCE(SUM(commission_quota), 0)").
		Scan(&report.TotalCommission).Error; err != nil {
		return nil, err
	}

	if err := DB.Model(&AffCommissionRecord{}).
		Where("status = ?", AffCommissionStatusReversed).
		Select("COALESCE(SUM(reversed_quota), 0)").
		Scan(&report.TotalReversed).Error; err != nil {
		return nil, err
	}

	// 冲正时因邀请人余额不足而没扣回的差额，是运营的真实损失
	if err := DB.Model(&AffCommissionRecord{}).
		Where("status = ?", AffCommissionStatusReversed).
		Select("COALESCE(SUM(commission_quota - reversed_quota), 0)").
		Scan(&report.ReversedLoss).Error; err != nil {
		return nil, err
	}

	rows := []struct {
		InviterId       int
		InviterUsername string
		TotalCommission int64
		RecordCount     int64
	}{}
	err := DB.Model(&AffCommissionRecord{}).
		Select("inviter_id, MAX(inviter_username) as inviter_username, " +
			"SUM(commission_quota) as total_commission, COUNT(*) as record_count").
		Where("status = ?", AffCommissionStatusGranted).
		Group("inviter_id").
		Order("total_commission desc").
		Limit(topN).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		report.TopInviters = append(report.TopInviters, TopInviter{
			InviterId:       r.InviterId,
			InviterUsername: r.InviterUsername,
			TotalCommission: r.TotalCommission,
			RecordCount:     r.RecordCount,
		})
	}

	return report, nil
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./model/ -run "AffCommissionRecords|GetInvitees|GetAffStats|GetAffReport" -v`

Expected: 5 个测试全 PASS

- [ ] **Step 5: 提交**

```bash
git add model/aff_query.go model/aff_query_test.go
git commit -m "feat(invite): 新增邀请返现的查询函数

4 个接口所需的 model 层查询，全部只读：
- GetAffStats 邀请汇总（人数、已充值人数、累计返现、当前等级与比例）
- GetAffCommissionRecords 返现明细分页，按时间倒序，含已冲正记录
- GetInvitees 被邀请人分页
- GetAffReport 管理员报表（全局发放/冲正总额、因余额不足没扣回的
  差额、Top 推广人排行）

被邀请人列表不含注册时间：users 表没有创建时间列，加列属于 schema
变更、不在本期范围。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 3: 4 个 handler

**Files:**
- Modify: `controller/aff.go`

- [ ] **Step 1: 写 handler**

追加到 `controller/aff.go`：

```go
// parsePaging 解析分页参数。仓库约定：query 参数名是 page 与 pagesize
// （全小写），page < 1 归一为 1，pagesize <= 0 默认 10。
func parsePaging(c *gin.Context) (page, pageSize int) {
	page, _ = strconv.Atoi(c.Query("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ = strconv.Atoi(c.Query("pagesize"))
	if pageSize <= 0 {
		pageSize = 10
	}
	return page, pageSize
}

func fail(c *gin.Context, msg string) {
	c.JSON(http.StatusOK, gin.H{"success": false, "message": msg})
}

// GetAffStats GET /api/user/aff/stats —— 当前用户的邀请汇总。
func GetAffStats(c *gin.Context) {
	stats, err := model.GetAffStats(c.GetInt("id"))
	if err != nil {
		fail(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    stats,
	})
}

// GetAffCommissionRecords GET /api/user/aff/records —— 当前用户的返现明细。
// 被邀请人用户名脱敏后返回。
func GetAffCommissionRecords(c *gin.Context) {
	page, pageSize := parsePaging(c)
	records, total, err := model.GetAffCommissionRecords(c.GetInt("id"), page, pageSize)
	if err != nil {
		fail(c, err.Error())
		return
	}

	// 不直接把 model 结构体丢给前端：里面有 inviter_username 等无需暴露的字段，
	// 而 invitee_username 必须脱敏
	type item struct {
		CreatedAt       int64   `json:"created_at"`
		InviteeUsername string  `json:"invitee_username"`
		SourceType      string  `json:"source_type"`
		TopupAmount     float64 `json:"topup_amount"`
		Rate            float64 `json:"rate"`
		CommissionQuota int64   `json:"commission_quota"`
		Status          int     `json:"status"`
		ReversedQuota   int64   `json:"reversed_quota"`
	}
	list := make([]item, 0, len(records))
	for _, r := range records {
		list = append(list, item{
			CreatedAt:       r.CreatedAt,
			InviteeUsername: maskUsername(r.InviteeUsername),
			SourceType:      r.SourceType,
			TopupAmount:     r.TopupAmount,
			Rate:            r.Rate,
			CommissionQuota: r.CommissionQuota,
			Status:          r.Status,
			ReversedQuota:   r.ReversedQuota,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"list":        list,
			"currentPage": page,
			"pageSize":    pageSize,
			"total":       total,
		},
	})
}

// GetInvitees GET /api/user/invitees —— 当前用户邀请的人，用户名脱敏。
func GetInvitees(c *gin.Context) {
	page, pageSize := parsePaging(c)
	invitees, total, err := model.GetInvitees(c.GetInt("id"), page, pageSize)
	if err != nil {
		fail(c, err.Error())
		return
	}
	for i := range invitees {
		invitees[i].Username = maskUsername(invitees[i].Username)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"list":        invitees,
			"currentPage": page,
			"pageSize":    pageSize,
			"total":       total,
		},
	})
}

// GetAffReport GET /api/aff/report —— 管理员侧全局返现报表。
//
// 管理员本就能查看用户完整信息，这里不脱敏。
func GetAffReport(c *gin.Context) {
	topN, _ := strconv.Atoi(c.Query("top"))
	if topN <= 0 || topN > 100 {
		topN = 10
	}
	report, err := model.GetAffReport(topN)
	if err != nil {
		fail(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    report,
	})
}
```

**命名冲突检查**：`controller.GetAffStats` 与 `model.GetAffStats` 同名但在不同包，合法。但 `controller/aff.go` 里的 `GetAffCommissionRecords` 与 `GetInvitees` 也和 model 层同名——同样合法，因为调用时带包名前缀。若 `go vet` 或编译报冲突，说明 controller 包内已有同名函数，此时给 controller 层的加 `Handler` 后缀。

- [ ] **Step 2: 检查 controller 包内是否已有同名符号**

Run: `grep -rn "func GetAffStats\|func GetInvitees\|func GetAffReport\|func GetAffCommissionRecords\|func fail\|func parsePaging" --include="*.go" controller/`

Expected: 只有 `controller/aff.go` 里的定义。若 `fail` 或 `parsePaging` 与既有函数重名，改成 `affFail` / `affParsePaging`。

- [ ] **Step 3: 验证**

Run: `go build ./controller/ && go vet ./controller/`

Expected: 均无输出

- [ ] **Step 4: 提交**

```bash
git add controller/aff.go
git commit -m "feat(invite): 新增 4 个邀请返现查询 handler

- GET /api/user/aff/stats    邀请汇总
- GET /api/user/aff/records  返现明细分页
- GET /api/user/invitees     被邀请人分页
- GET /api/aff/report        管理员全局报表

前三个接口的被邀请人用户名一律脱敏。返现明细不直接返回 model 结构体，
而是投影成只含必要字段的 DTO —— 避免把 inviter_username、source_no
等内部字段暴露给前端。

管理员报表不脱敏（管理员本就能查看用户完整信息），top 参数上限 100。

分页遵循仓库既有约定：query 参数是 page 与 pagesize（全小写），
响应格式 {success, message, data:{list, currentPage, pageSize, total}}。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 4: 注册路由

**Files:**
- Modify: `router/api-router.go`

- [ ] **Step 1: 在 selfRoute 组注册前 3 条**

`router/api-router.go`，在 `selfRoute.GET("/aff", controller.GetAffCode)` 之后追加：

```go
			selfRoute.GET("/aff", controller.GetAffCode)
			selfRoute.GET("/aff/stats", controller.GetAffStats)
			selfRoute.GET("/aff/records", controller.GetAffCommissionRecords)
			selfRoute.GET("/invitees", controller.GetInvitees)
```

`selfRoute` 已经挂了 `middleware.UserAuth()`，无需额外权限处理。

- [ ] **Step 2: 注册管理员报表路由**

在 `userRoute` 组之外新建一个 `/api/aff` 组（与 `/api/group-config/` 同构）。找到 `optionRoute := apiRouter.Group("/option")` 那一段，在其前后择一处插入：

```go
		affRoute := apiRouter.Group("/aff")
		affRoute.Use(middleware.AdminAuth())
		{
			affRoute.GET("/report", controller.GetAffReport)
		}
```

- [ ] **Step 3: 验证路由已注册**

Run: `grep -n "aff" router/api-router.go`

Expected: 能看到 4 条新路由 + 原有的 `/aff`

- [ ] **Step 4: 验证**

Run: `go build ./... && go vet ./...`

Expected: 均无输出

- [ ] **Step 5: 提交**

```bash
git add router/api-router.go
git commit -m "feat(invite): 注册 4 条邀请返现查询路由

selfRoute（已挂 UserAuth）：
- GET /api/user/aff/stats
- GET /api/user/aff/records
- GET /api/user/invitees

新建 /api/aff 组（AdminAuth）：
- GET /api/aff/report

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 5: 端到端验证与变更记录

**Files:**
- Modify: `docs/CHANGELOG.md`

- [ ] **Step 1: 跑完整验证门**

```bash
go build ./... && go vet ./... && go test ./... -count=1
```

Expected: build 与 vet 无输出；所有测试包 `ok`，0 失败

- [ ] **Step 2: 端到端跑一次真实接口**

单元测试证明不了路由注册、中间件、JSON 序列化是否正确。用临时 sqlite 起真实服务验证：

```bash
# 1. 编译并起服务（临时库，不碰任何真实数据）
mkdir -p /tmp/p4test
go build -o /tmp/p4test/one-api .
cd /tmp/p4test
NODE_TYPE=master SQLITE_PATH=/tmp/p4test/probe.db ./one-api --port 13000 &
sleep 5

# 2. 用 root 账号登录拿 session
curl -s -c /tmp/p4test/cookie.txt -X POST http://127.0.0.1:13000/api/user/login \
  -H "Content-Type: application/json" \
  -d '{"username":"root","password":"12345678"}'

# 3. 依次打四个接口
curl -s -b /tmp/p4test/cookie.txt http://127.0.0.1:13000/api/user/aff/stats
curl -s -b /tmp/p4test/cookie.txt "http://127.0.0.1:13000/api/user/aff/records?page=1&pagesize=10"
curl -s -b /tmp/p4test/cookie.txt "http://127.0.0.1:13000/api/user/invitees?page=1&pagesize=10"
curl -s -b /tmp/p4test/cookie.txt http://127.0.0.1:13000/api/aff/report

# 4. 收尾
kill %1
cd - && rm -rf /tmp/p4test
```

Expected: 四个接口都返回 `{"success":true,...}`。空库下 stats 的计数全为 0、list 为空数组（**不是 `null`**——前端拿到 `null` 会崩，`GetInvitees` 与 `GetAffReport` 里已用 `[]T{}` 初始化，这一步就是验证它真的生效）。

若某个接口返回 404，说明路由没注册成功；返回 `success:false` 则看 message 定位。

- [ ] **Step 3: 插入 CHANGELOG 记录**

在 `docs/CHANGELOG.md` 的 `---` 之后、当日最新条目之前插入：

```markdown
### feat(invite): 邀请返现查询接口
- **分支**: `main`
- **类型**: feat
- **涉及文件**: `model/aff_query.go`、`model/aff_query_test.go`、`controller/aff.go`、`controller/aff_test.go`、`router/api-router.go`
- **说明**: 新增 4 个只读查询接口供前端（`~/code/ezlinkai-web`，独立仓库）对接：`GET /api/user/aff/stats`（邀请汇总：人数、已充值人数、累计返现、当前等级与返现比例）、`GET /api/user/aff/records`（返现明细分页，按时间倒序，含已冲正记录）、`GET /api/user/invitees`（被邀请人分页）、`GET /api/aff/report`（管理员全局报表：发放/冲正总额、因余额不足没扣回的差额、Top 推广人）。前三个接口的被邀请人用户名一律脱敏（保留首尾、中间打星，按 rune 处理以免中文乱码），且不直接返回 model 结构体而是投影成 DTO，避免暴露 `source_no` 等内部字段。分页遵循仓库既有约定（`page`/`pagesize`、`{success,message,data:{list,currentPage,pageSize,total}}`）。列表字段用 `[]T{}` 初始化而非 nil，避免前端拿到 `null`。
- **关联计划**: `docs/superpowers/plans/2026-07-27-invite-commission-p4-api.md`
```

- [ ] **Step 4: 提交**

```bash
git add docs/CHANGELOG.md
git commit -m "docs(changelog): 记录 P4 邀请返现查询接口

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## 本期完成标准

- [ ] `maskUsername` 正确处理空串、1/2 字符、中文、中英混合
- [ ] 4 个 model 查询函数有测试覆盖，且不会串入他人数据
- [ ] 分页的 `total` 是全量数，不受当前页影响
- [ ] 已冲正的返现不计入 `stats.TotalCommission`，但**出现在**明细列表里
- [ ] 4 条路由注册正确，前 3 条走 `UserAuth`、报表走 `AdminAuth`
- [ ] 端到端 curl 四个接口均返回 `success: true`，空库下 list 是 `[]` 而非 `null`
- [ ] `go build ./... && go vet ./... && go test ./... -count=1` 全绿
- [ ] `docs/CHANGELOG.md` 已更新

## 本期不做

- 不做前端页面（在 `~/code/ezlinkai-web` 独立仓库）
- 不给 `users` 表加注册时间列（schema 变更，超出本期）
- 不做提现/划转（用户明确排除）
- 不接加密货币返现
- 不收敛消费侧 `500000` 硬编码
