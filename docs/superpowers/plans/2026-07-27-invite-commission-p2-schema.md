# P2: 数据模型与后台配置 —— 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把返现体系所需的全部数据结构就位——`users` 加两个累计字段、`group_configs` 加返现比例与升级门槛、新建返现明细表——并修掉两个会让 P3 建在流沙上的 bug（GroupConfig 变更不持久化、`sort_order` 由随机 map 顺序分配）。

**Architecture:** 只加结构、不接业务逻辑。`commission_rate` 默认 0，即返现全局关闭；本期上线后管理员可在后台逐级配置比例与门槛，P3 再让它真正生效。这样 P3 上线时可以按等级灰度放量，而不是一刀切全开。

**Tech Stack:** Go 1.24.5、GORM 1.25.7 AutoMigrate、sqlite/MySQL/PostgreSQL 三库兼容

**依赖:** P1（提供 `setupTestDB` 测试基座与 `AmountToQuota`）

**设计文档:** `docs/superpowers/specs/2026-07-27-invite-commission-by-level-design.md` §3 Bug 2、§4 数据模型、§5.4 缓存失效

---

## 本期修的 bug

### Bug 2 —— GroupConfig 变更不持久化到 options 表

`controller/group_config.go:67`、`:112`、`:151` 在写 DB 成功后只更新内存：

```go
common.GroupRatio[config.GroupKey] = config.Discount   // 只改内存
```

从未调用 `model.UpdateOption("GroupRatio", ...)`。重启后 `model/option.go` 会用 options 表的旧值覆盖内存，但 `group_configs` 表仍是新值——两处永久漂移。管理员在后台改的折扣重启就丢。

修复很简单，因为 `common.GroupRatio2JSONString()`（`common/group-ratio.go:18`）已存在。

### Bug 5 —— `sort_order` 由随机 map 迭代顺序分配（本期新发现）

`model/group_config.go:54-66`：

```go
order := 0
for key, ratio := range common.GroupRatio {   // Go map 迭代顺序是随机的
    config := GroupConfig{..., SortOrder: order}
    order++
}
```

`common.GroupRatio` 是 `map[string]float64`（`common/group-ratio.go:9`，含 Lv1~Lv6）。每次全新部署，6 个等级拿到的 `sort_order` 都不一样。

这在今天只影响后台列表的显示顺序（已经是个 bug），但 P3 的 `RecalcUserLevel` 要用 `sort_order` 作为「门槛并列时取较低等级」的判定依据——建在随机值上会让等级判定不可复现。必须在 P2 修掉。

---

## 文件结构

| 文件 | 责任 |
|---|---|
| `model/user.go`（修改） | `User` 结构体加 `GiftQuota` / `TopupQuota` |
| `model/group_config.go`（修改） | `GroupConfig` 加 `CommissionRate` / `UpgradeThreshold`；`InitGroupConfigs` 改确定性排序并写入默认门槛；新增按门槛降序查询与 tx 版按 key 查询 |
| `model/aff_commission.go`（新建） | `AffCommissionRecord` 模型定义与查询函数。返现的发放/冲正逻辑留到 P3，本期只有结构与读取 |
| `model/aff_commission_test.go`（新建） | 表结构与查询函数的测试 |
| `model/group_config_test.go`（新建） | `InitGroupConfigs` 确定性、按门槛降序查询的测试 |
| `model/log.go`（修改） | 追加 `LogTypeAffCommission` |
| `model/cache.go`（修改） | 新增 `InvalidateUserGroupCache` |
| `model/main.go`（修改） | AutoMigrate 追加 `AffCommissionRecord` |
| `controller/group_config.go`（修改） | 修 Bug 2；新增两个字段的校验 |

---

## Task 1: `users` 表新增两个累计字段

**Files:**
- Modify: `model/user.go:41`（`ChannelRatios` 之后）
- Test: `model/user_quota_fields_test.go`（新建）

- [ ] **Step 1: 写失败的测试**

`model/user_quota_fields_test.go`：

```go
package model

import (
	"testing"

	"gorm.io/gorm"
)

// TestUserGiftAndTopupQuotaFields 验证两个累计字段能建表、默认为 0、能原子累加。
func TestUserGiftAndTopupQuotaFields(t *testing.T) {
	setupTestDB(t, &User{})

	u := &User{Username: "alice", AffCode: "aaaa", AccessToken: "t-alice"}
	if err := DB.Create(u).Error; err != nil {
		t.Fatalf("create user failed: %v", err)
	}

	// 默认值必须是 0，而不是 NULL 导致的扫描错误
	var fresh User
	if err := DB.First(&fresh, u.Id).Error; err != nil {
		t.Fatalf("read user failed: %v", err)
	}
	if fresh.GiftQuota != 0 {
		t.Errorf("GiftQuota default = %d, want 0", fresh.GiftQuota)
	}
	if fresh.TopupQuota != 0 {
		t.Errorf("TopupQuota default = %d, want 0", fresh.TopupQuota)
	}

	// 两个字段能在一条 SQL 内原子累加（P3 的返现发放依赖这个用法）
	err := DB.Model(&User{}).Where("id = ?", u.Id).Updates(map[string]interface{}{
		"gift_quota":  gorm.Expr("gift_quota + ?", 4000000),
		"topup_quota": gorm.Expr("topup_quota + ?", 50000000),
	}).Error
	if err != nil {
		t.Fatalf("atomic update failed: %v", err)
	}

	var after User
	if err := DB.First(&after, u.Id).Error; err != nil {
		t.Fatalf("read back failed: %v", err)
	}
	if after.GiftQuota != 4000000 {
		t.Errorf("GiftQuota = %d, want 4000000", after.GiftQuota)
	}
	if after.TopupQuota != 50000000 {
		t.Errorf("TopupQuota = %d, want 50000000", after.TopupQuota)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./model/ -run TestUserGiftAndTopupQuotaFields -v`

Expected: 编译失败 —— `u.GiftQuota undefined (type User has no field or method GiftQuota)`

- [ ] **Step 3: 加字段**

`model/user.go`，在第 41 行 `ChannelRatios` 之后追加两行。注意用 `bigint` 而非 `int`——现有 `Quota`/`UsedQuota` 用的是 `gorm:"type:int"`，在 MySQL 上是 32 位 INT（约 21 亿上限），累计充值字段很容易溢出，新字段不重复这个错误：

```go
	ChannelRatios           string `json:"channel_ratios" gorm:"type:text"`
	// GiftQuota 累计获赠总额（注册奖励 + 邀请返现），只增不减；
	// 退款冲正是唯一例外。这是累计量，不是可用余额——可用余额始终是 Quota。
	GiftQuota int64 `json:"gift_quota" gorm:"type:bigint;default:0;column:gift_quota"`
	// TopupQuota 累计真实充值总额（仅 Stripe 入账），只增不减；退款冲正例外。
	// 用户等级升级以此为唯一依据，赠金不计入。
	TopupQuota int64 `json:"topup_quota" gorm:"type:bigint;default:0;column:topup_quota"`
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./model/ -run TestUserGiftAndTopupQuotaFields -v`

Expected: PASS

- [ ] **Step 5: 确认字段不会泄漏到前端本地存储**

`model/user.go:18-19` 的注释警告：新增敏感字段必须在 `setupLogin` 中清理。这两个字段不敏感（用户看自己的累计获赠/充值是正常需求），无需清理。但要确认 `controller/user.go` 的 `setupLogin` 不会因新字段报错：

Run: `grep -n "setupLogin" controller/user.go | head -3`

然后人工确认该函数是显式构造响应结构体（而非直接序列化整个 User）。若是显式构造，本步无需改动。

- [ ] **Step 6: 提交**

```bash
git add model/user.go model/user_quota_fields_test.go
git commit -m "feat(model): users 新增 gift_quota / topup_quota 累计字段

gift_quota 累计获赠总额（注册奖励 + 邀请返现），topup_quota 累计真实
充值总额。两者都是只增的累计量而非可用余额——可用余额始终是 quota，
本期不改动任何扣费链路。

用 bigint 而非现有 Quota 字段的 int，避免 MySQL 上 32 位溢出。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 2: `group_configs` 新增返现比例与升级门槛（含修 Bug 5）

**Files:**
- Modify: `model/group_config.go`
- Test: `model/group_config_test.go`（新建）

- [ ] **Step 1: 写失败的测试**

`model/group_config_test.go`：

```go
package model

import (
	"testing"
)

// TestInitGroupConfigsDeterministic 验证 InitGroupConfigs 的 sort_order 是确定的。
// 原实现用 for range map 分配 sort_order，Go map 迭代顺序随机，
// 导致每次全新部署的等级排序都不同。P3 的等级判定要依赖 sort_order。
func TestInitGroupConfigsDeterministic(t *testing.T) {
	// 连续初始化两次独立的库，sort_order 分配必须完全一致
	first := runInitGroupConfigs(t)
	second := runInitGroupConfigs(t)

	if len(first) == 0 {
		t.Fatal("InitGroupConfigs produced no rows")
	}
	for key, order := range first {
		if second[key] != order {
			t.Errorf("group %q sort_order not deterministic: %d vs %d",
				key, order, second[key])
		}
	}
}

// runInitGroupConfigs 在一个全新的库上跑一次 InitGroupConfigs，返回 key -> sort_order。
func runInitGroupConfigs(t *testing.T) map[string]int {
	t.Helper()
	db := setupTestDB(t, &GroupConfig{})
	if err := InitGroupConfigs(db); err != nil {
		t.Fatalf("InitGroupConfigs failed: %v", err)
	}
	var configs []GroupConfig
	if err := db.Find(&configs).Error; err != nil {
		t.Fatalf("query failed: %v", err)
	}
	result := make(map[string]int, len(configs))
	for _, c := range configs {
		result[c.GroupKey] = c.SortOrder
	}
	return result
}

// TestInitGroupConfigsDefaults 验证默认门槛与返现比例。
// commission_rate 必须默认为 0（返现全局关闭，安全默认）；
// upgrade_threshold 必须沿用原 controller/stripeCharge.go 的硬编码值。
func TestInitGroupConfigsDefaults(t *testing.T) {
	db := setupTestDB(t, &GroupConfig{})
	if err := InitGroupConfigs(db); err != nil {
		t.Fatalf("InitGroupConfigs failed: %v", err)
	}

	want := map[string]int64{
		"Lv1": 0,
		"Lv2": 2500000,   // $5
		"Lv3": 25000000,  // $50
		"Lv4": 50000000,  // $100
		"Lv5": 125000000, // $250
		"Lv6": 250000000, // $500
	}

	for key, wantThreshold := range want {
		var c GroupConfig
		if err := db.Where("group_key = ?", key).First(&c).Error; err != nil {
			t.Errorf("group %q not created: %v", key, err)
			continue
		}
		if c.UpgradeThreshold != wantThreshold {
			t.Errorf("group %q UpgradeThreshold = %d, want %d",
				key, c.UpgradeThreshold, wantThreshold)
		}
		if c.CommissionRate != 0 {
			t.Errorf("group %q CommissionRate = %v, want 0 (返现默认关闭)",
				key, c.CommissionRate)
		}
	}
}

// TestGetGroupConfigsByThresholdDesc 验证按门槛降序查询，
// 门槛并列时按 sort_order 升序——P3 的 RecalcUserLevel 依赖这个顺序。
func TestGetGroupConfigsByThresholdDesc(t *testing.T) {
	db := setupTestDB(t, &GroupConfig{})

	rows := []GroupConfig{
		{GroupKey: "A", DisplayName: "A", Discount: 1, UpgradeThreshold: 100, SortOrder: 3},
		{GroupKey: "B", DisplayName: "B", Discount: 1, UpgradeThreshold: 500, SortOrder: 1},
		{GroupKey: "C", DisplayName: "C", Discount: 1, UpgradeThreshold: 100, SortOrder: 2},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("create %s failed: %v", rows[i].GroupKey, err)
		}
	}

	got, err := GetGroupConfigsByThresholdDesc()
	if err != nil {
		t.Fatalf("GetGroupConfigsByThresholdDesc failed: %v", err)
	}

	// 期望：B(500) 最前；A 与 C 门槛并列 100，按 sort_order 升序 → C(2) 先于 A(3)
	wantOrder := []string{"B", "C", "A"}
	if len(got) != len(wantOrder) {
		t.Fatalf("got %d rows, want %d", len(got), len(wantOrder))
	}
	for i, key := range wantOrder {
		if got[i].GroupKey != key {
			t.Errorf("position %d = %q, want %q", i, got[i].GroupKey, key)
		}
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./model/ -run "TestInitGroupConfigs|TestGetGroupConfigsByThresholdDesc" -v`

Expected: 编译失败 —— `c.UpgradeThreshold undefined`、`undefined: GetGroupConfigsByThresholdDesc`

- [ ] **Step 3: 加字段**

`model/group_config.go`，替换整个 `GroupConfig` 结构体（第 8-16 行）：

```go
// GroupConfig 分组等级配置表
type GroupConfig struct {
	ID          int     `json:"id" gorm:"primaryKey;autoIncrement"`
	GroupKey    string  `json:"group_key" gorm:"type:varchar(32);uniqueIndex;not null"` // 对应 GroupRatio 的 key
	DisplayName string  `json:"display_name" gorm:"type:varchar(64);not null"`          // 显示名称，如 "Lv1 基础版"
	Discount    float64 `json:"discount" gorm:"type:decimal(4,2);default:1.0"`          // 等级折扣倍率
	SortOrder   int     `json:"sort_order" gorm:"default:0"`                            // 显示排序
	Description string  `json:"description" gorm:"type:varchar(255)"`                   // 等级描述
	// CommissionRate 该等级的邀请返现比例，[0, 1]。默认 0 = 该等级不返现。
	// 取值时看的是「邀请人」自己的等级，而非被邀请人的。
	CommissionRate float64 `json:"commission_rate" gorm:"type:decimal(5,4);default:0"`
	// UpgradeThreshold 升到本等级所需的累计真实充值 quota（对应 users.topup_quota）。
	// 0 表示无门槛。等级判定取「满足门槛的最高等级」，并列时取 SortOrder 较小者。
	UpgradeThreshold int64 `json:"upgrade_threshold" gorm:"type:bigint;default:0"`
}
```

- [ ] **Step 4: 改 `InitGroupConfigs` 为确定性排序并写入默认门槛**

`model/group_config.go`，替换 `InitGroupConfigs`（第 47-68 行）：

```go
// defaultUpgradeThresholds 各等级的默认升级门槛（单位 quota）。
// 数值沿用重构前 controller/stripeCharge.go 中硬编码的 levelMap，
// 保持既有升级行为不变：Lv2=$5、Lv3=$50、Lv4=$100、Lv5=$250。
// Lv6 原逻辑没有升级路径（levels 切片只到 Lv5），这里补一个 $500 的门槛。
//
// 门槛不能留 0：若某等级门槛为 0，则任何新用户都同时满足它与 Lv1 的门槛，
// 等级判定会把所有人拉到那个等级上。
var defaultUpgradeThresholds = map[string]int64{
	"Lv1": 0,
	"Lv2": 5 * 500000,
	"Lv3": 50 * 500000,
	"Lv4": 100 * 500000,
	"Lv5": 250 * 500000,
	"Lv6": 500 * 500000,
}

// InitGroupConfigs 在表为空时，从现有 GroupRatio 初始化默认配置。
//
// sort_order 按 group key 字典序分配，而不是按 map 迭代顺序——
// Go 的 map 迭代顺序是随机的，原实现会让每次全新部署的等级排序都不同，
// 而等级判定在门槛并列时要依赖 sort_order，必须确定。
func InitGroupConfigs(db *gorm.DB) error {
	var count int64
	db.Model(&GroupConfig{}).Count(&count)
	if count > 0 {
		return nil
	}

	keys := make([]string, 0, len(common.GroupRatio))
	for key := range common.GroupRatio {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for order, key := range keys {
		config := GroupConfig{
			GroupKey:    key,
			DisplayName: key,
			Discount:    common.GroupRatio[key],
			SortOrder:   order,
			// CommissionRate 保持零值：返现默认全局关闭，由管理员在后台逐级开启
			UpgradeThreshold: defaultUpgradeThresholds[key],
		}
		if err := db.Create(&config).Error; err != nil {
			return err
		}
	}
	return nil
}
```

同时在 `model/group_config.go` 的 import 块加入 `"sort"`：

```go
import (
	"sort"

	"github.com/songquanpeng/one-api/common"
	"gorm.io/gorm"
)
```

- [ ] **Step 5: 新增两个查询函数**

`model/group_config.go` 末尾追加。`GetGroupConfigByKeyTx` 是 P3 的 `GrantCommission` 需要的——它必须在充值事务内查配置，不能用走全局 `DB` 的 `GetGroupConfigByKey`：

```go
// GetGroupConfigsByThresholdDesc 按升级门槛降序返回全部分组配置。
// 门槛并列时按 sort_order 升序，即并列时较低等级排在前面——
// 等级判定取第一个满足门槛的分组，这个顺序保证运营新增分组时
// 若忘记设门槛（默认 0），用户会落到 Lv1 而不是被拉到新分组。
func GetGroupConfigsByThresholdDesc() (configs []GroupConfig, err error) {
	err = DB.Order("upgrade_threshold desc, sort_order asc, id asc").Find(&configs).Error
	return configs, err
}

// GetGroupConfigByKeyTx 在给定事务内按 key 查询分组配置。
// 返现发放必须与充值入账同事务，因此不能复用走全局 DB 的 GetGroupConfigByKey。
func GetGroupConfigByKeyTx(tx *gorm.DB, key string) (*GroupConfig, error) {
	var config GroupConfig
	err := tx.Where("group_key = ?", key).First(&config).Error
	return &config, err
}
```

- [ ] **Step 6: 运行确认通过**

Run: `go test ./model/ -run "TestInitGroupConfigs|TestGetGroupConfigsByThresholdDesc" -v`

Expected: 3 个测试全 PASS

- [ ] **Step 7: 提交**

```bash
git add model/group_config.go model/group_config_test.go
git commit -m "feat(model): group_configs 新增 commission_rate 与 upgrade_threshold

commission_rate 是该等级的邀请返现比例（默认 0 = 返现全局关闭，
由管理员在后台逐级开启）；upgrade_threshold 是升级所需累计充值，
数值沿用 controller/stripeCharge.go 原硬编码的 levelMap，另补 Lv6。

同时修 InitGroupConfigs 用 for range map 分配 sort_order 的 bug ——
Go map 迭代顺序随机，导致每次全新部署等级排序都不同，而后续的等级
判定要用 sort_order 做门槛并列时的取值依据。改为按 key 字典序。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 3: 新建 `aff_commission_records` 表

**Files:**
- Create: `model/aff_commission.go`
- Create: `model/aff_commission_test.go`
- Modify: `model/main.go`（AutoMigrate 追加）

- [ ] **Step 1: 写失败的测试**

`model/aff_commission_test.go`：

```go
package model

import (
	"testing"
)

// TestAffCommissionRecordUniqueSourceNo source_no 的唯一索引是幂等的核心：
// Stripe 会重放 webhook，唯一索引是防重复发放的最后一道保险。
func TestAffCommissionRecordUniqueSourceNo(t *testing.T) {
	db := setupTestDB(t, &AffCommissionRecord{})

	first := &AffCommissionRecord{
		InviterId: 1, InviteeId: 2,
		SourceType: SourceTypeStripeCheckout, SourceNo: "trade-001",
		TopupAmount: 100, TopupQuota: 50000000,
		Rate: 0.08, InviterGroup: "Lv4", CommissionQuota: 4000000,
		Status: AffCommissionStatusGranted, CreatedAt: 1000,
	}
	if err := db.Create(first).Error; err != nil {
		t.Fatalf("first insert should succeed: %v", err)
	}

	// 同一个 source_no 再插一次必须失败
	dup := *first
	dup.Id = 0
	if err := db.Create(&dup).Error; err == nil {
		t.Fatal("duplicate source_no was accepted; unique index missing")
	}
}

// TestGetAffCommissionRecordBySourceNo 冲正逻辑要按 source_no 定位记录。
func TestGetAffCommissionRecordBySourceNo(t *testing.T) {
	setupTestDB(t, &AffCommissionRecord{})

	rec := &AffCommissionRecord{
		InviterId: 7, InviteeId: 8,
		SourceType: SourceTypeStripeCharge, SourceNo: "order-042",
		TopupAmount: 20, TopupQuota: 10000000,
		Rate: 0.05, InviterGroup: "Lv3", CommissionQuota: 500000,
		Status: AffCommissionStatusGranted, CreatedAt: 2000,
	}
	if err := DB.Create(rec).Error; err != nil {
		t.Fatalf("create failed: %v", err)
	}

	got, err := GetAffCommissionRecordBySourceNo("order-042")
	if err != nil {
		t.Fatalf("lookup failed: %v", err)
	}
	if got == nil {
		t.Fatal("got nil record for existing source_no")
	}
	if got.InviterId != 7 || got.CommissionQuota != 500000 {
		t.Errorf("got InviterId=%d CommissionQuota=%d, want 7 / 500000",
			got.InviterId, got.CommissionQuota)
	}

	// 不存在的 source_no 必须返回 (nil, nil) 而非 error —— 调用方据此判断
	// 「那笔充值本来就没有返现」，不该当成故障处理
	missing, err := GetAffCommissionRecordBySourceNo("does-not-exist")
	if err != nil {
		t.Errorf("missing record should not error, got: %v", err)
	}
	if missing != nil {
		t.Errorf("missing record should be nil, got %+v", missing)
	}
}

// TestGetAffCommissionSummary 邀请汇总接口的数据来源。
func TestGetAffCommissionSummary(t *testing.T) {
	setupTestDB(t, &AffCommissionRecord{})

	rows := []AffCommissionRecord{
		{InviterId: 1, InviteeId: 2, SourceNo: "a", CommissionQuota: 100, Status: AffCommissionStatusGranted, CreatedAt: 1},
		{InviterId: 1, InviteeId: 3, SourceNo: "b", CommissionQuota: 200, Status: AffCommissionStatusGranted, CreatedAt: 2},
		// 已冲正的不计入累计收益
		{InviterId: 1, InviteeId: 4, SourceNo: "c", CommissionQuota: 900, Status: AffCommissionStatusReversed, CreatedAt: 3},
		// 别人的记录不能串进来
		{InviterId: 99, InviteeId: 5, SourceNo: "d", CommissionQuota: 700, Status: AffCommissionStatusGranted, CreatedAt: 4},
	}
	for i := range rows {
		if err := DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("create failed: %v", err)
		}
	}

	total, count, err := GetAffCommissionSummary(1)
	if err != nil {
		t.Fatalf("summary failed: %v", err)
	}
	if total != 300 {
		t.Errorf("total = %d, want 300 (已冲正的 900 不计入)", total)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./model/ -run TestAffCommission -v`

Expected: 编译失败 —— `undefined: AffCommissionRecord`

- [ ] **Step 3: 写模型与查询函数**

`model/aff_commission.go`：

```go
package model

import (
	"errors"

	"gorm.io/gorm"
)

// 返现记录的来源渠道。用字符串而非枚举整数，为加密货币等新渠道
// 预留扩展位而不需要数据迁移。
const (
	SourceTypeStripeCheckout = "stripe_checkout" // Stripe Checkout 链路（model/topup.go）
	SourceTypeStripeCharge   = "stripe_charge"   // Stripe 套餐链路（model/charge_order.go）
)

// 返现记录状态
const (
	AffCommissionStatusGranted  = 1 // 已发放
	AffCommissionStatusReversed = 2 // 已冲正（对应充值被退款）
)

// AffCommissionRecord 邀请返现明细。每一笔返现都有一条记录，用于对账与展示。
//
// Rate 与 InviterGroup 是快照：后台改了比例不影响已发放记录的解释，
// 历史记录永远可复现当时的计算过程。
// 用户名也是快照，用户改名后对账不断链（用户 id 同时保留，两者互补）。
type AffCommissionRecord struct {
	Id              int     `json:"id" gorm:"primaryKey;autoIncrement"`
	InviterId       int     `json:"inviter_id" gorm:"index;not null"`
	InviteeId       int     `json:"invitee_id" gorm:"index;not null"`
	InviterUsername string  `json:"inviter_username" gorm:"type:varchar(64)"`
	InviteeUsername string  `json:"invitee_username" gorm:"type:varchar(64)"`
	SourceType      string  `json:"source_type" gorm:"type:varchar(32)"`
	// SourceNo 是幂等的核心。Stripe 会重放 webhook，这个唯一索引是
	// 防重复发放的最后一道保险，比事务本身更重要。
	SourceNo    string  `json:"source_no" gorm:"type:varchar(128);uniqueIndex;not null"`
	TopupAmount float64 `json:"topup_amount" gorm:"type:decimal(20,6)"` // 被邀请人实付金额
	TopupQuota  int64   `json:"topup_quota"`                            // 换算后的充值 quota
	Rate        float64 `json:"rate" gorm:"type:decimal(5,4)"`          // 比例快照
	InviterGroup    string `json:"inviter_group" gorm:"type:varchar(32)"` // 等级快照
	CommissionQuota int64  `json:"commission_quota"`                      // 实发返现 quota
	Status          int    `json:"status" gorm:"default:1;index"`
	// ReversedQuota 实际扣回的额度，可能小于 CommissionQuota——
	// 冲正时邀请人余额不足则扣到 0 为止，差额是运营的真实损失，必须可查。
	ReversedQuota int64 `json:"reversed_quota" gorm:"default:0"`
	CreatedAt     int64 `json:"created_at" gorm:"bigint;index"`
	ReversedAt    int64 `json:"reversed_at" gorm:"bigint;default:0"`
}

// GetAffCommissionRecordBySourceNo 按来源单号查询返现记录。
// 记录不存在时返回 (nil, nil)——调用方据此判断「那笔充值本来没有返现」，
// 这是正常分支而非故障。
func GetAffCommissionRecordBySourceNo(sourceNo string) (*AffCommissionRecord, error) {
	if sourceNo == "" {
		return nil, nil
	}
	var record AffCommissionRecord
	err := DB.Where("source_no = ?", sourceNo).First(&record).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &record, nil
}

// GetAffCommissionSummary 汇总某个邀请人的累计返现额与有效返现笔数。
// 已冲正（退款）的记录不计入。
func GetAffCommissionSummary(inviterId int) (totalQuota int64, count int64, err error) {
	if inviterId <= 0 {
		return 0, 0, nil
	}

	// 两次查询各自新建链式调用：GORM 的 *gorm.DB 在执行过终结方法
	// （Count / Scan 等）后会携带残留状态，复用同一个 tx 变量会让
	// 第二次查询带上第一次的 SELECT 子句。
	if err = DB.Model(&AffCommissionRecord{}).
		Where("inviter_id = ? AND status = ?", inviterId, AffCommissionStatusGranted).
		Count(&count).Error; err != nil {
		return 0, 0, err
	}

	// SUM 在无匹配行时返回 NULL，用 COALESCE 兜成 0，否则 Scan 到 int64 会失败
	if err = DB.Model(&AffCommissionRecord{}).
		Where("inviter_id = ? AND status = ?", inviterId, AffCommissionStatusGranted).
		Select("COALESCE(SUM(commission_quota), 0)").
		Scan(&totalQuota).Error; err != nil {
		return 0, count, err
	}

	return totalQuota, count, nil
}
```

- [ ] **Step 4: 登记 AutoMigrate**

`model/main.go`，在 `AutoMigrate(&ModelMetrics{})` 之后、`logger.SysLog("database migrated")` 之前插入：

```go
		err = db.AutoMigrate(&AffCommissionRecord{})
		if err != nil {
			return nil, err
		}
```

- [ ] **Step 5: 运行确认通过**

Run: `go test ./model/ -run "AffCommission" -v`

Expected: 3 个测试全 PASS

注意 `-run` 的模式不要写成 `TestAffCommission` —— Go 的 `-run` 是对测试名做非锚定正则匹配，`TestGetAffCommissionRecordBySourceNo` 与 `TestGetAffCommissionSummary` 并不包含 `TestAffCommission` 这个子串，会被漏掉。用 `AffCommission` 才能匹配全部三个。

- [ ] **Step 6: 提交**

```bash
git add model/aff_commission.go model/aff_commission_test.go model/main.go
git commit -m "feat(model): 新增 aff_commission_records 返现明细表

source_no 唯一索引是幂等的核心 —— Stripe 会重放 webhook，这是防重复
发放的最后一道保险。rate / inviter_group / 用户名均为快照，保证历史
记录在后台改配置或用户改名后依然可解释、可对账。

reversed_quota 与 commission_quota 分开记：冲正时邀请人余额不足会
扣不满，差额是运营的真实损失，必须能查出来。

本期只有结构与读取函数，发放/冲正逻辑在 P3。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 4: 新增 log type 与 group 缓存失效函数

**Files:**
- Modify: `model/log.go:52-59`
- Modify: `model/cache.go`

- [ ] **Step 1: 追加 log type**

`model/log.go`，在常量组末尾追加。**必须追加在末尾**——iota 值一旦变动，历史日志的 type 语义就全部漂移了：

```go
const (
	LogTypeUnknown = iota
	LogTypeTopup
	LogTypeConsume
	LogTypeManage
	LogTypeSystem
	LogTypeError
	// LogTypeAffCommission 邀请返现（发放与冲正）。
	// 必须追加在末尾：iota 值变动会让历史日志的 type 语义全部漂移。
	LogTypeAffCommission
)
```

- [ ] **Step 2: 新增 group 缓存失效函数**

`model/cache.go`，在 `InvalidateUserChannelRatiosCache`（第 124 行附近）之后追加。与它同构：

```go
// InvalidateUserGroupCache 清除指定用户的分组缓存。
// 等级升级后必须调用，否则计费仍按旧分组的折扣走，最长陈旧一个
// config.SyncFrequency 周期。
func InvalidateUserGroupCache(id int) {
	if id <= 0 || !common.RedisEnabled {
		return
	}
	if err := common.RedisDel(fmt.Sprintf("user_group:%d", id)); err != nil {
		logger.SysError("Redis del user group error: " + err.Error())
	}
}
```

- [ ] **Step 3: 确认编译通过**

Run: `go build ./model/`

Expected: 无输出。`fmt`、`common`、`logger` 在 `model/cache.go` 中已有导入（`cache.go:3-20`），无需新增。

- [ ] **Step 4: 提交**

```bash
git add model/log.go model/cache.go
git commit -m "feat(model): 新增 LogTypeAffCommission 与 InvalidateUserGroupCache

返现的发放与冲正需要独立 log type 才能按邀请维度对账，此前注册奖励
复用 LogTypeSystem 无法区分。新常量追加在 iota 末尾，避免历史日志的
type 语义漂移。

等级升级后必须失效 user_group Redis 缓存，否则计费仍按旧分组折扣走。
该缓存此前没有失效函数，补上，与已有的
InvalidateUserChannelRatiosCache 同构。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 5: 修 Bug 2 并为新字段加校验

**Files:**
- Modify: `controller/group_config.go`

- [ ] **Step 1: 抽出共用的持久化函数**

`controller/group_config.go` 末尾追加。三个 handler 都要调它，抽出来避免三处重复：

```go
// persistGroupRatio 把内存中的 common.GroupRatio 持久化到 options 表。
//
// 原实现只改内存不写 options 表：重启后 model.InitOptionMap 会用 options
// 表的旧值覆盖内存，但 group_configs 表仍是新值，两处永久漂移，管理员
// 在后台改的折扣重启就丢。
//
// 注意 commission_rate 与 upgrade_threshold 不走 options 表、也不进内存
// map —— 它们只在充值回调里被读取（低频），每次直接查 group_configs，
// 从根本上避免这一类缓存漂移。
func persistGroupRatio() {
	if err := model.UpdateOption("GroupRatio", common.GroupRatio2JSONString()); err != nil {
		logger.SysError("failed to persist GroupRatio to options: " + err.Error())
	}
}
```

在 import 块加入 logger：

```go
import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/model"
)
```

- [ ] **Step 2: 抽出共用的字段校验**

同一文件继续追加。Create 与 Update 两个 handler 的校验逻辑相同，抽出来：

```go
// validateGroupConfigRanges 校验 discount / commission_rate / upgrade_threshold 的取值范围。
// 返回空串表示通过。
func validateGroupConfigRanges(config *model.GroupConfig) string {
	// discount 是计费乘数：1.0 = 无折扣，0.5 = 五折，0 = 免费。
	// 任何 > 1 的值都会让该分组的所有请求被放大 N 倍，必须挡住。
	if config.Discount < 0 || config.Discount > 1 {
		return "discount must be between 0 and 1 (multiplier; 1 = no discount)."
	}
	// commission_rate 是返现比例。> 1 意味着返的比充的多，直接资金漏洞。
	if config.CommissionRate < 0 || config.CommissionRate > 1 {
		return "commission_rate must be between 0 and 1."
	}
	// 负门槛会让等级判定行为不可预期
	if config.UpgradeThreshold < 0 {
		return "upgrade_threshold must not be negative."
	}
	return ""
}
```

- [ ] **Step 3: 改 Create handler**

`controller/group_config.go` 的 `CreateGroupConfigHandler` 中，把第 48-56 行的 discount 校验块整段替换为：

```go
	if msg := validateGroupConfigRanges(&config); msg != "" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": msg,
		})
		return
	}
```

并把第 66-67 行的内存同步：

```go
	// 同步更新 common.GroupRatio
	common.GroupRatio[config.GroupKey] = config.Discount
```

替换为：

```go
	// 同步内存并持久化到 options 表，两者缺一都会导致重启后配置漂移
	common.GroupRatio[config.GroupKey] = config.Discount
	persistGroupRatio()
```

- [ ] **Step 4: 改 Update handler**

同样地，把 `UpdateGroupConfigHandler` 中第 94-101 行的 discount 校验块替换为：

```go
	if msg := validateGroupConfigRanges(&config); msg != "" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": msg,
		})
		return
	}
```

把第 111-112 行替换为：

```go
	// 同步内存并持久化到 options 表
	common.GroupRatio[config.GroupKey] = config.Discount
	persistGroupRatio()
```

- [ ] **Step 5: 改 Delete handler**

把 `DeleteGroupConfigHandler` 第 150-151 行：

```go
	// 同步删除 common.GroupRatio 中的条目
	delete(common.GroupRatio, config.GroupKey)
```

替换为：

```go
	// 同步内存并持久化到 options 表
	delete(common.GroupRatio, config.GroupKey)
	persistGroupRatio()
```

- [ ] **Step 6: 确认编译与静态检查通过**

Run: `go build ./controller/ && go vet ./controller/`

Expected: 均无输出

- [ ] **Step 7: 提交**

```bash
git add controller/group_config.go
git commit -m "fix(group-config): 分组配置变更持久化到 options 表并校验新字段

三个 handler 此前只更新内存 common.GroupRatio，从未调用
model.UpdateOption('GroupRatio', ...)。重启后 InitOptionMap 用 options
表的旧值覆盖内存，而 group_configs 表仍是新值 —— 两处永久漂移，
管理员在后台改的折扣重启就丢。

同时为 commission_rate（> 1 意味着返的比充的多，是资金漏洞）与
upgrade_threshold 加范围校验，并把 Create/Update 重复的校验逻辑
抽成 validateGroupConfigRanges。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 6: 全量验证与变更记录

**Files:**
- Modify: `docs/CHANGELOG.md`

- [ ] **Step 1: 跑全量验证**

注意：若 `origin/main` 的 epay 清理尚未提交，`router` 与根包会因引用已删除的 `controller.EpayNotify` 而编译失败——那是既有问题，与本期无关。此时用排除法验证并在报告中明确说明：

```bash
# 优先尝试完整验证
go build ./... && go vet ./... && go test ./model/ ./controller/

# 若上面因 epay 失败，退化为排除 router 与根包
PKGS=$(go list ./... | grep -v -E "/router$|^github.com/songquanpeng/one-api$")
go build $PKGS && go vet $PKGS && go test ./model/
```

Expected: build 与 vet 无输出；`go test ./model/` 输出 `ok`

- [ ] **Step 2: 确认本期新增的测试都在跑**

Run: `go test ./model/ -v -run "TestUserGiftAndTopupQuotaFields|TestInitGroupConfigs|TestGetGroupConfigsByThresholdDesc|TestAffCommission" 2>&1 | grep -E "^(=== RUN|--- (PASS|FAIL)|ok|FAIL)"`

Expected: 6 个测试函数全部 PASS（`TestUserGiftAndTopupQuotaFields`、`TestInitGroupConfigsDeterministic`、`TestInitGroupConfigsDefaults`、`TestGetGroupConfigsByThresholdDesc`、`TestAffCommissionRecordUniqueSourceNo`、`TestGetAffCommissionRecordBySourceNo`、`TestGetAffCommissionSummary`）

- [ ] **Step 3: 插入 CHANGELOG 记录**

`docs/CHANGELOG.md`，在 `## 2026-07-27` 这个日期标题下、已有的 P1 条目**之后**追加（同一天的多条记录并列）：

```markdown
### feat(invite): 邀请返现体系数据模型与后台配置
- **分支**: `worktree-p1-foundation`
- **类型**: feat + fix
- **涉及文件**: `model/user.go`、`model/group_config.go`、`model/aff_commission.go`、`model/log.go`、`model/cache.go`、`model/main.go`、`controller/group_config.go`
- **说明**: 为按等级返现的邀请体系铺设数据结构。`users` 新增 `gift_quota`/`topup_quota` 两个只增的累计字段（不改动任何扣费链路）；`group_configs` 新增 `commission_rate`（默认 0，即返现全局关闭）与 `upgrade_threshold`；新建 `aff_commission_records` 明细表，`source_no` 唯一索引保证 Stripe webhook 重放幂等。同时修两个 bug：`controller/group_config.go` 三个 handler 只改内存不持久化 `GroupRatio` 导致重启配置漂移；`InitGroupConfigs` 用 `for range map` 分配 `sort_order` 导致每次全新部署等级排序随机。
- **关联计划**: `docs/superpowers/plans/2026-07-27-invite-commission-p2-schema.md`
```

- [ ] **Step 4: 提交**

```bash
git add docs/CHANGELOG.md
git commit -m "docs(changelog): 记录 P2 数据模型与后台配置

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## 本期完成标准

- [ ] `users` 表有 `gift_quota` / `topup_quota`，默认 0，可原子累加
- [ ] `group_configs` 表有 `commission_rate`（默认 0）/ `upgrade_threshold`（按等级回填默认值）
- [ ] `InitGroupConfigs` 的 `sort_order` 分配是确定的（同一份输入两次初始化结果一致）
- [ ] `aff_commission_records` 表建成，`source_no` 唯一索引生效（重复插入被拒）
- [ ] `LogTypeAffCommission` 与 `InvalidateUserGroupCache` 可用
- [ ] 后台改分组配置后重启不再丢失（`GroupRatio` 已持久化到 options 表）
- [ ] `commission_rate > 1` 与 `upgrade_threshold < 0` 被 API 拒绝
- [ ] `go test ./model/` 全绿，7 个新测试全 PASS
- [ ] `docs/CHANGELOG.md` 已更新

## 本期不做

- 不实现 `GrantCommission` / `ReverseCommission` / `RecalcUserLevel`（P3）
- 不接入任何充值链路（P3）
- 不删除 `controller/stripeCharge.go` 的 `UserLevelUpgrade`（P3）
- 不做历史数据回填（P4）——本期新字段对历史用户全是 0，这是预期状态
- 不做任何查询接口（P4）
- 不动前端（前端在 `~/code/ezlinkai-web` 独立仓库）

## 上线注意

本期上线后 `commission_rate` 全为 0，返现处于关闭状态且 P3 的发放逻辑尚不存在，**功能上完全无感**。这是有意的：数据结构先落地并经历一轮真实流量的 AutoMigrate 验证，再上业务逻辑。

`upgrade_threshold` 虽然已写入默认值，但 P3 之前没有任何代码读它——现有的 `controller/stripeCharge.go:14-44` 仍在用自己的硬编码 `levelMap`（且因两个 bug 从未真正生效）。两者并存不冲突。
