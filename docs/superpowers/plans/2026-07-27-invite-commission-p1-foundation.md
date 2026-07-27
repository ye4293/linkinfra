# P1: 测试基座与计费口径修复 —— 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 `model/` 包立起 in-memory sqlite 测试基座，并把散落在 3 处的「充值金额 → quota」硬编码收敛为单一可测函数，同时修掉充值邮件金额恒为 $0 的变量遮蔽 bug。

**Architecture:** 不引入任何新功能。新增一个 `AmountToQuota` 纯函数作为唯一换算入口，三条充值链路全部改为调用它——Bug 1（硬编码 `500000` 绕过 `config.QuotaPerUnit`）由此从根上消除，而不是逐处打补丁。测试基座用 `gorm.io/driver/sqlite` 的 `:memory:` 库替换全局 `DB`/`LOG_DB`，用 `t.Cleanup` 还原。

**Tech Stack:** Go 1.24.5、GORM 1.25.7、`gorm.io/driver/sqlite` v1.5.5（CGO，已实测可用）

**依赖:** 无。这是第一期。

**设计文档:** `docs/superpowers/specs/2026-07-27-invite-commission-by-level-design.md` §3 Bug 1 / Bug 4

---

## 背景：为什么先做这一期

`model/` 与 `controller/` 包目前**一个测试文件都没有**（全仓 14 个 `_test.go` 全在 `relay/channel/` 与 `common/`）。后续三期的每个核心函数都需要 DB 交互测试，测试基座必须先立起来。

同时，「金额 → quota」的换算目前有 3 个实现，其中 2 个硬编码 `500000`：

| 位置 | 当前代码 | 问题 |
|---|---|---|
| `model/topup.go:124` | `int64(float64(topUp.Amount) * config.QuotaPerUnit)` | 正确 |
| `model/charge_order.go:200` | `int64(amount*500000)` | 硬编码，绕过后台配置 |
| `model/order.go:82` | `int64(addAmount*500000)` | 硬编码，绕过后台配置 |

管理员在后台改 `QuotaPerUnit` 后，三条链路口径不一致。返现要按充值额算比例，基数不统一则返现必然算错——所以这一期必须先于 P3。

---

## 文件结构

| 文件 | 责任 |
|---|---|
| `model/testutil_test.go`（新建） | 测试专用：in-memory sqlite 建库、替换全局 `DB`/`LOG_DB`、`t.Cleanup` 还原。仅测试编译期存在，不进生产二进制 |
| `model/quota_convert.go`（新建） | `AmountToQuota` —— 金额到 quota 的唯一换算入口 |
| `model/quota_convert_test.go`（新建） | `AmountToQuota` 的单元测试 |
| `model/charge_order.go:200`（修改） | 改用 `AmountToQuota` |
| `model/order.go:81,82`（修改） | 改用 `AmountToQuota`；修变量遮蔽 |
| `model/topup.go:124`（修改） | 改用 `AmountToQuota`，统一口径 |

---

## Task 1: 建立 `model/` 包测试基座

**Files:**
- Create: `model/testutil_test.go`

- [ ] **Step 1: 写测试基座文件**

注意三点：`:memory:` 库每个连接是独立的，必须 `SetMaxOpenConns(1)` 才能让多次查询看到同一份数据；`RecordLog` 用的是 `LOG_DB` 而非 `DB`，两者都要替换；GORM 的 logger 包与项目的 `common/logger` 同名，必须起别名。

```go
package model

import (
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// setupTestDB 建立一个隔离的 in-memory sqlite 库并替换全局 DB / LOG_DB。
// 测试结束后自动还原，供 model 包内所有需要 DB 的测试复用。
func setupTestDB(t *testing.T, models ...interface{}) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open in-memory sqlite: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to get underlying sql.DB: %v", err)
	}
	// ":memory:" 下每个连接是一个独立的库。限制为单连接，
	// 否则同一个测试里的两次查询可能落在不同的空库上。
	sqlDB.SetMaxOpenConns(1)

	if len(models) > 0 {
		if err := db.AutoMigrate(models...); err != nil {
			t.Fatalf("failed to migrate test models: %v", err)
		}
	}

	origDB, origLogDB := DB, LOG_DB
	DB, LOG_DB = db, db
	t.Cleanup(func() {
		DB, LOG_DB = origDB, origLogDB
		_ = sqlDB.Close()
	})

	return db
}
```

- [ ] **Step 2: 写一个自检测试，确认基座本身可用**

追加到同一文件末尾：

```go
// TestSetupTestDB 自检：基座能建表、能写读、单连接可见性正确。
func TestSetupTestDB(t *testing.T) {
	db := setupTestDB(t, &GroupConfig{})

	if err := db.Create(&GroupConfig{GroupKey: "Lv1", DisplayName: "Lv1", Discount: 1.0}).Error; err != nil {
		t.Fatalf("create failed: %v", err)
	}

	var got GroupConfig
	if err := DB.Where("group_key = ?", "Lv1").First(&got).Error; err != nil {
		t.Fatalf("read back through global DB failed: %v", err)
	}
	if got.DisplayName != "Lv1" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Lv1")
	}
}
```

- [ ] **Step 3: 运行测试确认通过**

Run: `go test ./model/ -run TestSetupTestDB -v`

Expected: `--- PASS: TestSetupTestDB`。首次运行会编译 CGO 的 sqlite3 绑定，约 5-10 秒并输出若干 C 编译 warning，属正常，后续走缓存。

- [ ] **Step 4: 提交**

```bash
git add model/testutil_test.go
git commit -m "test(model): 建立 in-memory sqlite 测试基座

model 包此前零测试。新增 setupTestDB helper，用 :memory: 库替换全局
DB/LOG_DB 并在 t.Cleanup 中还原，供后续邀请返现相关测试复用。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 2: 收敛金额→quota 换算为单一函数

**Files:**
- Create: `model/quota_convert.go`
- Create: `model/quota_convert_test.go`

- [ ] **Step 1: 先写失败的测试**

`model/quota_convert_test.go`：

```go
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
		{"小数金额向下取整", 500000, 0.0000019, 0},
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
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./model/ -run TestAmountToQuota -v`

Expected: 编译失败 —— `undefined: AmountToQuota`

- [ ] **Step 3: 写最小实现**

`model/quota_convert.go`：

```go
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
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./model/ -run TestAmountToQuota -v`

Expected: 全部子测试 PASS

- [ ] **Step 5: 提交**

```bash
git add model/quota_convert.go model/quota_convert_test.go
git commit -m "feat(model): 新增 AmountToQuota 作为金额换算唯一入口

金额到 quota 的换算此前有 3 个实现，其中 charge_order.go 与 order.go
硬编码 500000，绕过了后台可配的 config.QuotaPerUnit。先立函数与测试，
下一个 commit 替换调用点。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 3: 三条充值链路改用 `AmountToQuota`（修 Bug 1）

**Files:**
- Modify: `model/charge_order.go:200`
- Modify: `model/order.go:82`
- Modify: `model/topup.go:124`

- [ ] **Step 1: 改 Stripe 套餐链路**

`model/charge_order.go`，将第 200 行：

```go
			err := IncreaseUserQuota(chargeOrder.UserId, int64(amount*500000))
```

替换为：

```go
			err := IncreaseUserQuota(chargeOrder.UserId, AmountToQuota(amount))
```

- [ ] **Step 2: 改加密货币链路**

`model/order.go`，将第 82 行：

```go
					err = IncreaseUserQuota(response.UserId, int64(addAmount*500000))
```

替换为：

```go
					err = IncreaseUserQuota(response.UserId, AmountToQuota(addAmount))
```

- [ ] **Step 3: 改 Stripe Checkout 链路（口径本已正确，仅统一为同一入口）**

`model/topup.go`，将第 124 行：

```go
		quotaToAdd = int64(float64(topUp.Amount) * config.QuotaPerUnit)
```

替换为：

```go
		quotaToAdd = AmountToQuota(float64(topUp.Amount))
```

- [ ] **Step 4: 确认 `config` 包导入未变成未使用**

`model/topup.go` 中 `config` 包在别处仍有使用（如 `config.QuotaForNewUser` 等），但改动后需确认：

Run: `go build ./model/`

Expected: 无输出（编译通过）。若报 `"github.com/songquanpeng/one-api/common/config" imported and not used`，则从 `model/topup.go` 的 import 块中删除该行。

- [ ] **Step 5: 确认全仓再无硬编码 500000 的充值换算**

Run: `grep -rn "\* *500000\|500000)" model/ controller/ --include="*.go"`

Expected: 不再出现于任何充值入账路径。`controller/stripeCharge.go:23-27` 的等级门槛仍含 `500000`，属预期——那部分在 P3 迁移到 `group_configs.upgrade_threshold`。

- [ ] **Step 6: 全量验证**

Run: `go build ./... && go vet ./... && go test ./model/ -v`

Expected: build 与 vet 无输出，测试全 PASS

- [ ] **Step 7: 提交**

```bash
git add model/charge_order.go model/order.go model/topup.go
git commit -m "fix(topup): 充值入账不再硬编码 500000，统一走 AmountToQuota

charge_order.go:200 与 order.go:82 硬编码 500000，绕过后台可配的
config.QuotaPerUnit —— 管理员改配置后这两条链路不生效，与
topup.go 的口径不一致。三处统一改用 AmountToQuota。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 4: 修加密货币充值邮件金额恒为 $0（Bug 4）

**Files:**
- Modify: `model/order.go:81`

- [ ] **Step 1: 理解 bug**

`model/order.go` 中存在变量遮蔽：

```go
47          var addAmount float64                          // 外层声明，值恒为 0
48          if err := DB.Transaction(func(tx *gorm.DB) error {
...
81              addAmount := response.ValueForwardedCoin   // := 在闭包内新建变量，遮蔽外层
82              err = IncreaseUserQuota(...)
...
88          }); err != nil { ... }
93          AfterChargeSuccess(response.UserId, addAmount) // 读的是外层的 0
```

结果：加密货币充值成功邮件里的金额永远显示 `0.000000$`。`go vet` 不检测这类遮蔽，所以一直没被发现。

- [ ] **Step 2: 修复 —— 把闭包内的声明改为赋值**

`model/order.go` 第 81 行：

```go
					addAmount := response.ValueForwardedCoin
```

替换为：

```go
					// 这里必须是赋值而非 := 声明：外层 addAmount 在事务提交后
					// 要传给 AfterChargeSuccess 发送充值成功邮件。用 := 会新建
					// 一个闭包内变量，导致邮件金额恒为 0。
					addAmount = response.ValueForwardedCoin
```

- [ ] **Step 3: 确认编译通过**

Run: `go build ./model/`

Expected: 无输出。若报 `addAmount declared and not used`，说明外层声明位置或作用域与预期不符，需重新阅读 `model/order.go:44-94` 的嵌套结构后再改。

- [ ] **Step 4: 全量验证**

Run: `go build ./... && go vet ./...`

Expected: 均无输出

- [ ] **Step 5: 提交**

```bash
git add model/order.go
git commit -m "fix(topup): 修加密货币充值邮件金额恒为 0 的变量遮蔽

order.go:81 用 := 在事务闭包内新建了 addAmount，遮蔽了 :47 的外层变量，
导致 :93 传给 AfterChargeSuccess 的金额永远是 0，充值成功邮件一直
显示 0.000000\$。改为赋值。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 5: 更新变更记录

**Files:**
- Modify: `docs/CHANGELOG.md`

- [ ] **Step 1: 在 CHANGELOG.md 顶部插入本期记录**

按项目 CLAUDE.md 规定的格式，插入在文件现有内容之前（保持最新日期在上）：

```markdown
## 2026-07-27

### fix(topup): 统一充值金额换算口径并修复邮件金额 bug
- **分支**: `main`
- **类型**: 修复 + 测试基础设施
- **涉及文件**: `model/testutil_test.go`、`model/quota_convert.go`、`model/quota_convert_test.go`、`model/charge_order.go`、`model/order.go`、`model/topup.go`
- **说明**: 为 model 包建立 in-memory sqlite 测试基座（此前该包零测试）。新增 `AmountToQuota` 作为金额→quota 的唯一换算入口，替换 `charge_order.go` 与 `order.go` 中硬编码的 `500000`——此前管理员修改后台 `QuotaPerUnit` 对这两条链路无效。同时修复 `order.go` 中 `addAmount` 变量遮蔽导致加密货币充值邮件金额恒为 $0 的问题。
- **关联计划**: `docs/superpowers/plans/2026-07-27-invite-commission-p1-foundation.md`
```

- [ ] **Step 2: 最终全量验证**

Run: `go build ./... && go vet ./... && go test ./model/ -v`

Expected: build 与 vet 无输出；`TestSetupTestDB` 与 `TestAmountToQuota` 全部 PASS

- [ ] **Step 3: 提交**

```bash
git add docs/CHANGELOG.md
git commit -m "docs(changelog): 记录 P1 充值口径修复与测试基座

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## 本期完成标准

- [ ] `model/` 包有可复用的 `setupTestDB` helper，`go test ./model/` 通过
- [ ] 全仓充值入账路径不再出现硬编码 `500000`
- [ ] 三条充值链路的金额换算都走 `AmountToQuota`，跟随后台 `QuotaPerUnit`
- [ ] 加密货币充值邮件显示真实金额
- [ ] `go build ./... && go vet ./...` 干净
- [ ] `docs/CHANGELOG.md` 已更新

## 本期不做

- 不新增任何数据库字段或表（P2）
- 不动等级升级逻辑（P3，其硬编码的 `500000` 门槛在 P3 迁移到 `group_configs`）
- 不给兑换码、管理员补单路径补测试（超出范围）
- 不修 `model/redemption.go` 的缓存失效（设计文档 §5.4 已记录为既有缺陷，不扩大范围）
