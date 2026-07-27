# P3: 返现核心逻辑与充值接入 —— 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让按等级返现真正生效——实现返现发放、退款冲正、等级重算三个核心函数，接入两条 Stripe 充值链路，并修掉从未生效过的等级升级逻辑。

**Architecture:** 返现与充值入账在**同一个事务**内完成，靠 `aff_commission_records.source_no` 的唯一索引保证 Stripe webhook 重放幂等。等级重算与 Redis 缓存失效放在事务**提交后**（前者失败可由下次充值自愈，后者不参与事务回滚）。历史回填从 P4 提前到本期，否则 P3 一上线所有历史用户会因 `topup_quota = 0` 掉回 Lv1。

**Tech Stack:** Go 1.24.5、GORM 1.25.7、Stripe webhook、sqlite/MySQL/PostgreSQL 三库兼容

**依赖:** P1（`setupTestDB`、`AmountToQuota`）、P2（`gift_quota`/`topup_quota`、`commission_rate`/`upgrade_threshold`、`aff_commission_records`、`LogTypeAffCommission`、`InvalidateUserGroupCache`、`GetGroupConfigsByThresholdDesc`、`GetGroupConfigByKeyTx`）

**设计文档:** `docs/superpowers/specs/2026-07-27-invite-commission-by-level-design.md` §3 Bug 3、§5 全部

---

## 本期为什么把回填从 P4 提前

设计文档 §7.1 写明：`topup_quota` 是等级判定的新基准，若不回填，**所有历史用户会因 `topup_quota = 0` 被判定为 Lv1**，折扣被打回原形。

而 P3 一上线 `RecalcUserLevel` 就会在每笔充值后跑。若回填留在 P4，P3 与 P4 之间的任何一笔充值都会把该用户降级（虽然 `RecalcUserLevel` 只升不降，但历史用户的 `Group` 字段本就已是 Lv3/Lv4，而 `topup_quota = 0` 会让重算逻辑认为他只够 Lv1——只升不降会保住现状，不会主动降级，但这层保护是脆弱的，任何后续改动都可能破掉）。

把回填放进本期，P3 自身就是安全可上线的，不依赖部署顺序的人工纪律。

## 本期发现的既有约束

**`ChargeOrder` 不在 AutoMigrate 清单里。** `model/main.go:117-181` 只迁移了 `Order`，没有 `ChargeOrder`——`charge_orders` 表由外部手工创建。因此回填函数查询该表时必须容忍「表不存在」，否则全新部署会在启动迁移阶段直接失败。

**退款路径没有事务。** `stripeChargeRefund`（`model/charge_order.go:157`）与 `UpdateChargeOrderStatusWithCondition` 都走全局 `DB`，且后者的 `userId` 参数是 `string`。冲正需要与改单状态原子，故本期新增一个 tx 变体。

---

## 文件结构

| 文件 | 责任 |
|---|---|
| `model/user_level.go`（新建） | `RecalcUserLevel` —— 依据 `topup_quota` 重算等级 |
| `model/user_level_test.go`（新建） | 等级重算测试，含 Bug 3 的回归用例 |
| `model/aff_commission.go`（修改） | 追加 `GrantCommission`、`ReverseCommission`、`isDuplicateKeyError` |
| `model/aff_commission_test.go`（修改） | 追加发放与冲正测试 |
| `model/migration_topup_quota.go`（新建） | `topup_quota` 历史回填，用 options 表标记位保证只跑一次 |
| `model/migration_topup_quota_test.go`（新建） | 回填测试，含「charge_orders 表不存在」用例 |
| `model/topup.go`（修改） | Checkout 链路接入返现与 `topup_quota` |
| `model/charge_order.go`（修改） | 套餐链路接入返现与 `topup_quota`；退款接入冲正；新增 tx 变体 |
| `model/main.go`（修改） | 启动时调用回填 |
| `controller/stripeCharge.go`（修改） | 删除失效的 `UserLevelUpgrade` 与其调用点 |
| `controller/cryptoPay.go`（修改） | 删除 `UserLevelUpgrade` 调用 |

---

## Task 1: `RecalcUserLevel` —— 等级重算（修 Bug 3）

**Files:**
- Create: `model/user_level.go`
- Create: `model/user_level_test.go`

### Bug 3 回顾

`controller/stripeCharge.go:31-42` 现有实现：

```go
for i := 0; i < len(levels)-1; i++ {
    if user.Group == currentLevel &&
       totalQuota > levelMap[currentLevel] &&
       totalQuota <= levelMap[nextLevel] {   // ← 超过下一级门槛反而不升级
```

一次充 $300 的用户 `totalQuota = 150000000`，超过 Lv5 门槛 `125000000`，所有分支条件都不满足 → 停在 Lv1。

调用点 `controller/stripeCharge.go:105-106` 用 `c.GetInt("id")` 取 userId，而 Stripe webhook 请求没有登录态，永远是 0 → 升级从来没有生效过。

- [ ] **Step 1: 写失败的测试**

`model/user_level_test.go`：

```go
package model

import (
	"testing"
)

// seedLevels 建立 Lv1~Lv5 的门槛配置，sort_order 与等级顺序一致。
func seedLevels(t *testing.T) {
	t.Helper()
	rows := []GroupConfig{
		{GroupKey: "Lv1", DisplayName: "Lv1", Discount: 1.00, SortOrder: 0, UpgradeThreshold: 0},
		{GroupKey: "Lv2", DisplayName: "Lv2", Discount: 0.95, SortOrder: 1, UpgradeThreshold: 2500000},
		{GroupKey: "Lv3", DisplayName: "Lv3", Discount: 0.90, SortOrder: 2, UpgradeThreshold: 25000000},
		{GroupKey: "Lv4", DisplayName: "Lv4", Discount: 0.85, SortOrder: 3, UpgradeThreshold: 50000000},
		{GroupKey: "Lv5", DisplayName: "Lv5", Discount: 0.80, SortOrder: 4, UpgradeThreshold: 125000000},
	}
	for i := range rows {
		if err := DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed %s failed: %v", rows[i].GroupKey, err)
		}
	}
}

// mkUser 造一个指定分组与累计充值的用户。
func mkUser(t *testing.T, name, group string, topupQuota int64) *User {
	t.Helper()
	u := &User{
		Username: name, Group: group, TopupQuota: topupQuota,
		AffCode: name, AccessToken: "t-" + name,
	}
	if err := DB.Create(u).Error; err != nil {
		t.Fatalf("create user %s failed: %v", name, err)
	}
	return u
}

func TestRecalcUserLevel(t *testing.T) {
	tests := []struct {
		name        string
		group       string
		topupQuota  int64
		wantGroup   string
		wantChanged bool
	}{
		{"零充值留在Lv1", "Lv1", 0, "Lv1", false},
		{"刚好够Lv2门槛", "Lv1", 2500000, "Lv2", true},
		{"差1个quota不够Lv2", "Lv1", 2499999, "Lv1", false},
		// Bug 3 的核心回归：一次充 $300 必须从 Lv1 直达 Lv5，
		// 而不是因为「超过下一级门槛」而卡在原地
		{"一次充300美元跨级直达Lv5", "Lv1", 150000000, "Lv5", true},
		{"跨级到Lv4", "Lv1", 60000000, "Lv4", true},
		{"已是Lv5不再变", "Lv5", 150000000, "Lv5", false},
		// 只升不降：手工调高过等级的用户不能被打回去
		{"只升不降", "Lv5", 0, "Lv5", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupTestDB(t, &User{}, &GroupConfig{}, &Log{})
			seedLevels(t)
			u := mkUser(t, "u1", tt.group, tt.topupQuota)

			changed, err := RecalcUserLevel(u.Id)
			if err != nil {
				t.Fatalf("RecalcUserLevel failed: %v", err)
			}
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
			}

			var after User
			if err := DB.First(&after, u.Id).Error; err != nil {
				t.Fatalf("read back failed: %v", err)
			}
			if after.Group != tt.wantGroup {
				t.Errorf("Group = %q, want %q", after.Group, tt.wantGroup)
			}
		})
	}
}

// TestRecalcUserLevelThresholdTie 门槛并列时取 sort_order 较小者（较低等级）。
// 这保证运营新增分组忘记设门槛（默认 0）时，用户落到 Lv1 而不是被拉到新分组。
func TestRecalcUserLevelThresholdTie(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &Log{})

	rows := []GroupConfig{
		{GroupKey: "Lv1", DisplayName: "Lv1", Discount: 1, SortOrder: 0, UpgradeThreshold: 0},
		// 运营新加的分组，忘了设门槛
		{GroupKey: "VIP", DisplayName: "VIP", Discount: 0.5, SortOrder: 9, UpgradeThreshold: 0},
	}
	for i := range rows {
		if err := DB.Create(&rows[i]).Error; err != nil {
			t.Fatalf("seed failed: %v", err)
		}
	}

	u := mkUser(t, "u1", "Lv1", 0)
	if _, err := RecalcUserLevel(u.Id); err != nil {
		t.Fatalf("RecalcUserLevel failed: %v", err)
	}

	var after User
	if err := DB.First(&after, u.Id).Error; err != nil {
		t.Fatalf("read back failed: %v", err)
	}
	if after.Group != "Lv1" {
		t.Errorf("Group = %q, want Lv1（并列时不能被拉到 VIP）", after.Group)
	}
}

// TestRecalcUserLevelEdgeCases 分组表为空、用户不存在、当前分组不在表中。
func TestRecalcUserLevelEdgeCases(t *testing.T) {
	t.Run("分组表为空时不报错也不改动", func(t *testing.T) {
		setupTestDB(t, &User{}, &GroupConfig{}, &Log{})
		u := mkUser(t, "u1", "Lv3", 999999999)

		changed, err := RecalcUserLevel(u.Id)
		if err != nil {
			t.Fatalf("空分组表不应报错，got: %v", err)
		}
		if changed {
			t.Error("changed = true，空分组表不该改动等级")
		}

		var after User
		_ = DB.First(&after, u.Id).Error
		if after.Group != "Lv3" {
			t.Errorf("Group = %q, want Lv3（不该被改）", after.Group)
		}
	})

	t.Run("用户不存在返回错误", func(t *testing.T) {
		setupTestDB(t, &User{}, &GroupConfig{}, &Log{})
		seedLevels(t)
		if _, err := RecalcUserLevel(999999); err == nil {
			t.Error("用户不存在时应返回错误")
		}
	})

	t.Run("非法userId直接返回", func(t *testing.T) {
		setupTestDB(t, &User{}, &GroupConfig{}, &Log{})
		if changed, err := RecalcUserLevel(0); err != nil || changed {
			t.Errorf("userId=0 应返回 (false, nil)，got (%v, %v)", changed, err)
		}
	})

	t.Run("当前分组不在表中视为门槛0允许升级", func(t *testing.T) {
		setupTestDB(t, &User{}, &GroupConfig{}, &Log{})
		seedLevels(t)
		// 手工改过 DB 留下的野分组
		u := mkUser(t, "u1", "LegacyGroup", 60000000)

		changed, err := RecalcUserLevel(u.Id)
		if err != nil {
			t.Fatalf("RecalcUserLevel failed: %v", err)
		}
		if !changed {
			t.Error("changed = false，野分组应被当作门槛 0 从而允许升级")
		}

		var after User
		_ = DB.First(&after, u.Id).Error
		if after.Group != "Lv4" {
			t.Errorf("Group = %q, want Lv4", after.Group)
		}
	})
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./model/ -run RecalcUserLevel -v`

Expected: 编译失败 —— `undefined: RecalcUserLevel`

- [ ] **Step 3: 写实现**

`model/user_level.go`：

```go
package model

import (
	"fmt"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/logger"
)

// RecalcUserLevel 依据累计真实充值（users.topup_quota）重算用户等级。
//
// 取「满足门槛的最高等级」，而不是重构前那样逐级 +1 —— 原实现的条件写成
// totalQuota <= levelMap[nextLevel]，导致一次大额充值的用户「超过下一级
// 门槛反而不升级」，永远卡在原地（见 controller/stripeCharge.go 重构前的
// 第 31-42 行）。
//
// 只升不降：判定依据是 upgrade_threshold 的大小关系，而非 group key 的
// 字典序，这样运营新增或重命名等级时不会破坏语义。当前分组不在
// group_configs 中时（手工改过 DB 留下的野分组），视其门槛为 0，即允许升级。
//
// 必须在充值事务**提交后**调用：等级变化不影响资金正确性，失败可由下一次
// 充值自愈；放在事务内会让分组表的任何异常都阻塞充值入账。
//
// 返回 changed 表示等级是否真的变了，调用方据此决定是否失效 Redis 缓存。
func RecalcUserLevel(userId int) (changed bool, err error) {
	if userId <= 0 {
		return false, nil
	}

	var user User
	if err = DB.Where("id = ?", userId).First(&user).Error; err != nil {
		return false, err
	}

	configs, err := GetGroupConfigsByThresholdDesc()
	if err != nil {
		return false, err
	}
	if len(configs) == 0 {
		// 分组表为空（未初始化或被清空）：不做任何判断，保持现状
		return false, nil
	}

	// configs 已按 upgrade_threshold 降序、并列时 sort_order 升序排列，
	// 因此第一个满足门槛的就是目标等级
	var target *GroupConfig
	for i := range configs {
		if user.TopupQuota >= configs[i].UpgradeThreshold {
			target = &configs[i]
			break
		}
	}
	if target == nil {
		// 连最低门槛都不满足（所有门槛都 > 0 且用户充值为 0）
		return false, nil
	}
	if target.GroupKey == user.Group {
		return false, nil
	}

	// 只升不降：比较门槛大小，而非 group key 字典序
	currentThreshold := int64(0)
	for i := range configs {
		if configs[i].GroupKey == user.Group {
			currentThreshold = configs[i].UpgradeThreshold
			break
		}
	}
	if target.UpgradeThreshold <= currentThreshold {
		return false, nil
	}

	oldGroup := user.Group
	if err = DB.Model(&User{}).Where("id = ?", userId).
		Update("group", target.GroupKey).Error; err != nil {
		return false, err
	}

	RecordLog(userId, LogTypeSystem, fmt.Sprintf(
		"level upgraded from %s to %s (cumulative top-up %s)",
		oldGroup, target.GroupKey, common.LogQuota(user.TopupQuota)))

	return true, nil
}

// RecalcUserLevelAndRefreshCache 重算等级，并在等级确实变化时失效分组缓存。
//
// 分组缓存不失效会让计费在一个 config.SyncFrequency 周期内继续用旧折扣。
// 缓存操作失败只记 log 不返回错误：等级已落库，缓存最多陈旧一个周期，
// 不能因为 Redis 抖动就让充值回调返回失败触发 Stripe 重试。
func RecalcUserLevelAndRefreshCache(userId int) {
	changed, err := RecalcUserLevel(userId)
	if err != nil {
		logger.SysError(fmt.Sprintf("failed to recalc level for user %d: %s", userId, err.Error()))
		return
	}
	if changed {
		InvalidateUserGroupCache(userId)
	}
}
```

**注意 `group` 是 SQL 保留字。** `Update("group", ...)` 由 GORM 自动加引号处理，但若改写成原生 SQL 或 `Where("group = ?")`，必须参照 `model/user.go:470-477` `GetUserGroup` 的做法按数据库类型选择反引号或双引号。

- [ ] **Step 4: 运行确认通过**

Run: `go test ./model/ -run RecalcUserLevel -v`

Expected: 全部子测试 PASS，其中 `一次充300美元跨级直达Lv5` 是 Bug 3 的回归证明

- [ ] **Step 5: 提交**

```bash
git add model/user_level.go model/user_level_test.go
git commit -m "feat(invite): 新增 RecalcUserLevel 依据累计充值重算等级

替换 controller/stripeCharge.go 中从未生效过的 UserLevelUpgrade：
原实现的条件写成 totalQuota <= levelMap[nextLevel]，导致一次大额充值
的用户「超过下一级门槛反而不升级」，一次充 \$300 会永远卡在 Lv1。

改为取「满足门槛的最高等级」，只升不降，判定依据是 upgrade_threshold
的大小关系而非 group key 字典序。门槛来自 group_configs，实时查询不缓存。

测试含 Bug 3 的回归用例（一次充 \$300 从 Lv1 直达 Lv5）、门槛并列取
较低等级、分组表为空、野分组等边界。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 2: `GrantCommission` —— 返现发放

**Files:**
- Modify: `model/aff_commission.go`
- Modify: `model/aff_commission_test.go`

- [ ] **Step 1: 写失败的测试**

追加到 `model/aff_commission_test.go` 末尾：

```go
// grantFixture 建立「邀请人 Lv4 返现 8%，被邀请人已绑定邀请人」的场景。
func grantFixture(t *testing.T) (inviter, invitee *User) {
	t.Helper()

	cfgs := []GroupConfig{
		{GroupKey: "Lv1", DisplayName: "Lv1", Discount: 1, SortOrder: 0, CommissionRate: 0},
		{GroupKey: "Lv4", DisplayName: "Lv4", Discount: 0.85, SortOrder: 3, CommissionRate: 0.08},
	}
	for i := range cfgs {
		if err := DB.Create(&cfgs[i]).Error; err != nil {
			t.Fatalf("seed group failed: %v", err)
		}
	}

	inviter = &User{Username: "inviter", Group: "Lv4", AffCode: "inv1", AccessToken: "t-inviter"}
	if err := DB.Create(inviter).Error; err != nil {
		t.Fatalf("create inviter failed: %v", err)
	}
	invitee = &User{Username: "invitee", Group: "Lv1", AffCode: "inv2", AccessToken: "t-invitee", InviterId: inviter.Id}
	if err := DB.Create(invitee).Error; err != nil {
		t.Fatalf("create invitee failed: %v", err)
	}
	return inviter, invitee
}

func TestGrantCommission(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
	inviter, invitee := grantFixture(t)

	// 被邀请人充 $100，邀请人 Lv4 返 8% = $8 = 4000000 quota
	err := DB.Transaction(func(tx *gorm.DB) error {
		gotInviter, commission, err := GrantCommission(
			tx, invitee.Id, 100, 50000000, SourceTypeStripeCheckout, "trade-100")
		if err != nil {
			return err
		}
		if gotInviter != inviter.Id {
			t.Errorf("inviterId = %d, want %d", gotInviter, inviter.Id)
		}
		if commission != 4000000 {
			t.Errorf("commission = %d, want 4000000", commission)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("transaction failed: %v", err)
	}

	// 邀请人的 quota 与 gift_quota 都要 +4000000
	var after User
	if err := DB.First(&after, inviter.Id).Error; err != nil {
		t.Fatalf("read inviter failed: %v", err)
	}
	if after.Quota != 4000000 {
		t.Errorf("inviter Quota = %d, want 4000000", after.Quota)
	}
	if after.GiftQuota != 4000000 {
		t.Errorf("inviter GiftQuota = %d, want 4000000", after.GiftQuota)
	}

	// 明细记录的快照字段
	rec, err := GetAffCommissionRecordBySourceNo("trade-100")
	if err != nil || rec == nil {
		t.Fatalf("record not found: %v", err)
	}
	if rec.Rate != 0.08 {
		t.Errorf("Rate = %v, want 0.08", rec.Rate)
	}
	if rec.InviterGroup != "Lv4" {
		t.Errorf("InviterGroup = %q, want Lv4", rec.InviterGroup)
	}
	if rec.InviterUsername != "inviter" || rec.InviteeUsername != "invitee" {
		t.Errorf("username snapshot wrong: %q / %q", rec.InviterUsername, rec.InviteeUsername)
	}
	if rec.Status != AffCommissionStatusGranted {
		t.Errorf("Status = %d, want %d", rec.Status, AffCommissionStatusGranted)
	}
}

// TestGrantCommissionIdempotent Stripe 会重放 webhook，同一个 source_no
// 重复发放必须被挡住，且余额不能二次增加。
func TestGrantCommissionIdempotent(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
	inviter, invitee := grantFixture(t)

	for i := 0; i < 3; i++ {
		err := DB.Transaction(func(tx *gorm.DB) error {
			_, _, err := GrantCommission(tx, invitee.Id, 100, 50000000,
				SourceTypeStripeCheckout, "trade-replay")
			return err
		})
		if err != nil {
			t.Fatalf("第 %d 次调用不应报错: %v", i+1, err)
		}
	}

	var after User
	_ = DB.First(&after, inviter.Id).Error
	if after.Quota != 4000000 {
		t.Errorf("Quota = %d, want 4000000（重放不该累加）", after.Quota)
	}

	var count int64
	DB.Model(&AffCommissionRecord{}).Where("source_no = ?", "trade-replay").Count(&count)
	if count != 1 {
		t.Errorf("记录数 = %d, want 1", count)
	}
}

func TestGrantCommissionEarlyReturns(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) int // 返回 inviteeId
	}{
		{
			name: "无邀请人",
			setup: func(t *testing.T) int {
				u := &User{Username: "solo", AffCode: "solo", AccessToken: "t-solo", InviterId: 0}
				if err := DB.Create(u).Error; err != nil {
					t.Fatalf("create failed: %v", err)
				}
				return u.Id
			},
		},
		{
			name: "邀请人等级返现比例为0",
			setup: func(t *testing.T) int {
				cfg := GroupConfig{GroupKey: "Lv1", DisplayName: "Lv1", Discount: 1, CommissionRate: 0}
				if err := DB.Create(&cfg).Error; err != nil {
					t.Fatalf("seed failed: %v", err)
				}
				inviter := &User{Username: "inv", Group: "Lv1", AffCode: "a1", AccessToken: "t-a1"}
				if err := DB.Create(inviter).Error; err != nil {
					t.Fatalf("create failed: %v", err)
				}
				u := &User{Username: "u", AffCode: "a2", AccessToken: "t-a2", InviterId: inviter.Id}
				if err := DB.Create(u).Error; err != nil {
					t.Fatalf("create failed: %v", err)
				}
				return u.Id
			},
		},
		{
			name: "邀请人分组配置缺失时降级为不返现",
			setup: func(t *testing.T) int {
				inviter := &User{Username: "inv", Group: "GhostGroup", AffCode: "b1", AccessToken: "t-b1"}
				if err := DB.Create(inviter).Error; err != nil {
					t.Fatalf("create failed: %v", err)
				}
				u := &User{Username: "u", AffCode: "b2", AccessToken: "t-b2", InviterId: inviter.Id}
				if err := DB.Create(u).Error; err != nil {
					t.Fatalf("create failed: %v", err)
				}
				return u.Id
			},
		},
		{
			name: "自邀请",
			setup: func(t *testing.T) int {
				cfg := GroupConfig{GroupKey: "Lv4", DisplayName: "Lv4", Discount: 0.85, CommissionRate: 0.08}
				if err := DB.Create(&cfg).Error; err != nil {
					t.Fatalf("seed failed: %v", err)
				}
				u := &User{Username: "self", Group: "Lv4", AffCode: "c1", AccessToken: "t-c1"}
				if err := DB.Create(u).Error; err != nil {
					t.Fatalf("create failed: %v", err)
				}
				// 手工把 inviter_id 指向自己
				if err := DB.Model(&User{}).Where("id = ?", u.Id).
					Update("inviter_id", u.Id).Error; err != nil {
					t.Fatalf("update failed: %v", err)
				}
				return u.Id
			},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
			inviteeId := tt.setup(t)

			var gotInviter int
			var commission int64
			err := DB.Transaction(func(tx *gorm.DB) error {
				var err error
				gotInviter, commission, err = GrantCommission(
					tx, inviteeId, 100, 50000000,
					SourceTypeStripeCheckout, "trade-early-"+string(rune('a'+i)))
				return err
			})
			if err != nil {
				t.Fatalf("早退分支不应报错: %v", err)
			}
			if gotInviter != 0 || commission != 0 {
				t.Errorf("got (%d, %d), want (0, 0)", gotInviter, commission)
			}

			var count int64
			DB.Model(&AffCommissionRecord{}).Count(&count)
			if count != 0 {
				t.Errorf("记录数 = %d, want 0（早退不该产生记录）", count)
			}
		})
	}
}

// TestGrantCommissionRounding 返现额向下取整，不足 1 quota 时不产生记录。
func TestGrantCommissionRounding(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
	_, invitee := grantFixture(t)

	// $0.0000001 × 8% × 500000 = 0.004 → 取整为 0，不发放
	var commission int64
	err := DB.Transaction(func(tx *gorm.DB) error {
		var err error
		_, commission, err = GrantCommission(tx, invitee.Id, 0.0000001, 50,
			SourceTypeStripeCheckout, "trade-tiny")
		return err
	})
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if commission != 0 {
		t.Errorf("commission = %d, want 0", commission)
	}

	var count int64
	DB.Model(&AffCommissionRecord{}).Count(&count)
	if count != 0 {
		t.Errorf("记录数 = %d, want 0（返现额为 0 不该产生记录）", count)
	}
}
```

测试文件需要新增 `gorm.io/gorm` 导入：

```go
import (
	"testing"

	"gorm.io/gorm"
)
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./model/ -run GrantCommission -v`

Expected: 编译失败 —— `undefined: GrantCommission`

- [ ] **Step 3: 写实现**

追加到 `model/aff_commission.go` 末尾：

```go
// isDuplicateKeyError 判断是否为唯一键冲突。
//
// GORM 的 gorm.ErrDuplicatedKey 需要在 gorm.Config 里开启 TranslateError，
// 本项目未开启，因此只能按驱动的错误文本判断。三种数据库的措辞都覆盖到。
func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || // sqlite / postgres
		strings.Contains(msg, "constraint failed") || // sqlite 另一种措辞
		strings.Contains(msg, "duplicate key") || // postgres
		strings.Contains(msg, "duplicate entry") // mysql
}

// GrantCommission 在给定事务内为被邀请人的一笔充值发放邀请返现。
//
// 必须在充值入账的同一事务内调用，以保证「入账 + 返现 + 明细」原子。
// 返回 (inviterId, commissionQuota, error)；无需返现时返回 (0, 0, nil)。
//
// 幂等：source_no 上有唯一索引。Stripe 会重放 webhook，重复调用会在
// INSERT 处被挡住并按成功返回，不会二次加额。
//
// 错误处理：除唯一键冲突外的 DB 错误一律返回 err，从而回滚整笔充值。
// 这看似激进，但是唯一正确的选择 —— Stripe 会重试 webhook，重试时靠
// source_no 唯一索引保证不重复入账，最终一致。反之若「返现失败只记 log
// 不阻塞充值」，返现就会静默丢钱，事后只能靠人工对账捞回。
//
// 唯一的例外是邀请人分组配置缺失：此时降级为「不返现」而非报错，
// 避免分组表的异常阻塞充值入账。
//
// topupAmount 取用户实付金额，不取扣手续费后的净额：对用户承诺「充值额的
// N%」必须字面成立，毛利通过调低 commission_rate 控制，而不是隐藏基数。
func GrantCommission(tx *gorm.DB, inviteeId int, topupAmount float64,
	topupQuota int64, sourceType, sourceNo string) (int, int64, error) {

	if inviteeId <= 0 || topupAmount <= 0 || sourceNo == "" {
		return 0, 0, nil
	}

	// 不用 Select 挑列：group 是 SQL 保留字，整行读取避免引号处理
	var invitee User
	if err := tx.Where("id = ?", inviteeId).First(&invitee).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, 0, nil
		}
		return 0, 0, err
	}

	inviterId := invitee.InviterId
	if inviterId <= 0 {
		return 0, 0, nil
	}
	// 自邀请：理论上不可能，但 DB 被手工改过或未来新增绑定入口时会出现
	if inviterId == inviteeId {
		logger.SysError(fmt.Sprintf("user %d has itself as inviter, skipping commission", inviteeId))
		return 0, 0, nil
	}

	var inviter User
	if err := tx.Where("id = ?", inviterId).First(&inviter).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 邀请人已被删除
			return 0, 0, nil
		}
		return 0, 0, err
	}

	gc, err := GetGroupConfigByKeyTx(tx, inviter.Group)
	if err != nil {
		// 分组配置缺失降级为不返现，不阻塞充值入账
		logger.SysError(fmt.Sprintf(
			"group config %q not found for inviter %d, skipping commission: %s",
			inviter.Group, inviterId, err.Error()))
		return 0, 0, nil
	}
	if gc.CommissionRate <= 0 {
		return 0, 0, nil
	}

	commissionQuota := int64(topupAmount * gc.CommissionRate * config.QuotaPerUnit)
	if commissionQuota <= 0 {
		return 0, 0, nil
	}

	record := &AffCommissionRecord{
		InviterId:       inviterId,
		InviteeId:       inviteeId,
		InviterUsername: inviter.Username,
		InviteeUsername: invitee.Username,
		SourceType:      sourceType,
		SourceNo:        sourceNo,
		TopupAmount:     topupAmount,
		TopupQuota:      topupQuota,
		Rate:            gc.CommissionRate,
		InviterGroup:    inviter.Group,
		CommissionQuota: commissionQuota,
		Status:          AffCommissionStatusGranted,
		CreatedAt:       helper.GetTimestamp(),
	}
	if err := tx.Create(record).Error; err != nil {
		if isDuplicateKeyError(err) {
			// webhook 重放，已发放过
			return 0, 0, nil
		}
		return 0, 0, err
	}

	// 不能用 IncreaseUserQuota：它走全局 DB 且受 BatchUpdateEnabled 影响，
	// 会脱离当前事务
	if err := tx.Model(&User{}).Where("id = ?", inviterId).Updates(map[string]interface{}{
		"quota":      gorm.Expr("quota + ?", commissionQuota),
		"gift_quota": gorm.Expr("gift_quota + ?", commissionQuota),
	}).Error; err != nil {
		return 0, 0, err
	}

	return inviterId, commissionQuota, nil
}
```

`model/aff_commission.go` 的 import 块需要扩展为：

```go
import (
	"errors"
	"fmt"
	"strings"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"gorm.io/gorm"
)
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./model/ -run GrantCommission -v`

Expected: 全部子测试 PASS，其中 `TestGrantCommissionIdempotent` 是 webhook 重放防护的证明

- [ ] **Step 5: 提交**

```bash
git add model/aff_commission.go model/aff_commission_test.go
git commit -m "feat(invite): 实现 GrantCommission 按邀请人等级发放返现

在充值事务内完成「入账 + 返现 + 明细」，靠 source_no 唯一索引保证
Stripe webhook 重放幂等。除唯一键冲突外的 DB 错误一律回滚整笔充值 ——
Stripe 会重试 webhook，靠唯一索引保证最终一致；反之返现失败静默记 log
会导致丢钱且只能人工对账。

唯一的降级例外是邀请人分组配置缺失，此时按不返现处理，避免分组表异常
阻塞充值入账。

返现基数取用户实付金额而非扣手续费后的净额，保证「充值额的 N%」字面成立。

不复用 IncreaseUserQuota：它走全局 DB 且受 BatchUpdateEnabled 影响，
会脱离当前事务。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 3: `ReverseCommission` —— 退款冲正

**Files:**
- Modify: `model/aff_commission.go`
- Modify: `model/aff_commission_test.go`

### 为什么必须做

`model/charge_order.go:157` 的 `stripeChargeRefund` 目前**只改订单状态、不扣回已发放的额度** —— 这本身已是漏洞。加上立即到账的返现后，「充值 → 拿返现 → 退款」就是一条免费套利路径。

- [ ] **Step 1: 写失败的测试**

追加到 `model/aff_commission_test.go` 末尾：

```go
func TestReverseCommission(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
	inviter, invitee := grantFixture(t)

	// 先正常发放
	err := DB.Transaction(func(tx *gorm.DB) error {
		_, _, err := GrantCommission(tx, invitee.Id, 100, 50000000,
			SourceTypeStripeCharge, "order-refund")
		return err
	})
	if err != nil {
		t.Fatalf("grant failed: %v", err)
	}

	// 再冲正
	var gotInviter int
	var reversed int64
	err = DB.Transaction(func(tx *gorm.DB) error {
		var err error
		gotInviter, reversed, err = ReverseCommission(tx, "order-refund")
		return err
	})
	if err != nil {
		t.Fatalf("reverse failed: %v", err)
	}
	if gotInviter != inviter.Id {
		t.Errorf("inviterId = %d, want %d", gotInviter, inviter.Id)
	}
	if reversed != 4000000 {
		t.Errorf("reversed = %d, want 4000000", reversed)
	}

	var after User
	_ = DB.First(&after, inviter.Id).Error
	if after.Quota != 0 {
		t.Errorf("Quota = %d, want 0", after.Quota)
	}
	if after.GiftQuota != 0 {
		t.Errorf("GiftQuota = %d, want 0", after.GiftQuota)
	}

	rec, _ := GetAffCommissionRecordBySourceNo("order-refund")
	if rec == nil {
		t.Fatal("record disappeared")
	}
	if rec.Status != AffCommissionStatusReversed {
		t.Errorf("Status = %d, want %d", rec.Status, AffCommissionStatusReversed)
	}
	if rec.ReversedQuota != 4000000 {
		t.Errorf("ReversedQuota = %d, want 4000000", rec.ReversedQuota)
	}
	if rec.ReversedAt == 0 {
		t.Error("ReversedAt 未设置")
	}
}

// TestReverseCommissionInsufficientBalance 邀请人已把返现花掉时，
// 扣到 0 为止，绝不产生负余额。差额记在 reversed_quota 与 commission_quota
// 的落差里，是运营的真实损失。
func TestReverseCommissionInsufficientBalance(t *testing.T) {
	setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
	inviter, invitee := grantFixture(t)

	err := DB.Transaction(func(tx *gorm.DB) error {
		_, _, err := GrantCommission(tx, invitee.Id, 100, 50000000,
			SourceTypeStripeCharge, "order-spent")
		return err
	})
	if err != nil {
		t.Fatalf("grant failed: %v", err)
	}

	// 邀请人把大部分返现花掉，只剩 1000000
	if err := DB.Model(&User{}).Where("id = ?", inviter.Id).
		Update("quota", 1000000).Error; err != nil {
		t.Fatalf("update quota failed: %v", err)
	}

	var reversed int64
	err = DB.Transaction(func(tx *gorm.DB) error {
		var err error
		_, reversed, err = ReverseCommission(tx, "order-spent")
		return err
	})
	if err != nil {
		t.Fatalf("reverse failed: %v", err)
	}
	if reversed != 1000000 {
		t.Errorf("reversed = %d, want 1000000（只能扣到 0）", reversed)
	}

	var after User
	_ = DB.First(&after, inviter.Id).Error
	if after.Quota != 0 {
		t.Errorf("Quota = %d, want 0（绝不能为负）", after.Quota)
	}

	rec, _ := GetAffCommissionRecordBySourceNo("order-spent")
	if rec.ReversedQuota != 1000000 {
		t.Errorf("ReversedQuota = %d, want 1000000", rec.ReversedQuota)
	}
	if rec.CommissionQuota != 4000000 {
		t.Errorf("CommissionQuota 应保留原值 4000000, got %d", rec.CommissionQuota)
	}
}

func TestReverseCommissionIdempotentAndMissing(t *testing.T) {
	t.Run("重复冲正只生效一次", func(t *testing.T) {
		setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
		inviter, invitee := grantFixture(t)

		_ = DB.Transaction(func(tx *gorm.DB) error {
			_, _, err := GrantCommission(tx, invitee.Id, 100, 50000000,
				SourceTypeStripeCharge, "order-twice")
			return err
		})
		// 额外给邀请人一些余额，确保第二次冲正若生效会被察觉
		_ = DB.Model(&User{}).Where("id = ?", inviter.Id).
			Update("quota", gorm.Expr("quota + ?", 9000000)).Error

		for i := 0; i < 3; i++ {
			err := DB.Transaction(func(tx *gorm.DB) error {
				_, _, err := ReverseCommission(tx, "order-twice")
				return err
			})
			if err != nil {
				t.Fatalf("第 %d 次冲正报错: %v", i+1, err)
			}
		}

		var after User
		_ = DB.First(&after, inviter.Id).Error
		// 4000000 + 9000000 - 4000000 = 9000000
		if after.Quota != 9000000 {
			t.Errorf("Quota = %d, want 9000000（只该扣一次）", after.Quota)
		}
	})

	t.Run("记录不存在时按无返现处理", func(t *testing.T) {
		setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
		err := DB.Transaction(func(tx *gorm.DB) error {
			inviterId, reversed, err := ReverseCommission(tx, "never-existed")
			if inviterId != 0 || reversed != 0 {
				t.Errorf("got (%d, %d), want (0, 0)", inviterId, reversed)
			}
			return err
		})
		if err != nil {
			t.Errorf("记录不存在不该报错: %v", err)
		}
	})

	t.Run("空sourceNo直接返回", func(t *testing.T) {
		setupTestDB(t, &User{}, &GroupConfig{}, &AffCommissionRecord{}, &Log{})
		err := DB.Transaction(func(tx *gorm.DB) error {
			_, _, err := ReverseCommission(tx, "")
			return err
		})
		if err != nil {
			t.Errorf("空 sourceNo 不该报错: %v", err)
		}
	})
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./model/ -run ReverseCommission -v`

Expected: 编译失败 —— `undefined: ReverseCommission`

- [ ] **Step 3: 写实现**

追加到 `model/aff_commission.go` 末尾：

```go
// ReverseCommission 冲正一笔已发放的返现（对应充值被退款）。
//
// 必须在改单状态的同一事务内调用。
// 返回 (inviterId, actualReversedQuota, error)；无需冲正时返回 (0, 0, nil)。
//
// 余额不足时扣到 0 为止，绝不产生负余额 —— 邀请人可能已经把返现花掉了，
// 强行扣成负数会让他后续所有请求都被拒。差额（commission_quota -
// reversed_quota）是运营的真实损失，记在明细里并告警，可事后查账。
//
// 幂等：status != Granted 时直接返回，重复冲正不会二次扣款。
func ReverseCommission(tx *gorm.DB, sourceNo string) (int, int64, error) {
	if sourceNo == "" {
		return 0, 0, nil
	}

	var record AffCommissionRecord
	if err := tx.Where("source_no = ?", sourceNo).First(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 那笔充值本来就没有返现（无邀请人、比例为 0 等）
			return 0, 0, nil
		}
		return 0, 0, err
	}

	if record.Status != AffCommissionStatusGranted {
		// 已冲正过
		return 0, 0, nil
	}

	var inviter User
	if err := tx.Where("id = ?", record.InviterId).First(&inviter).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 邀请人已注销：只标记记录，无处可扣
			if err := markCommissionReversed(tx, &record, 0); err != nil {
				return 0, 0, err
			}
			logger.SysError(fmt.Sprintf(
				"commission reversal for %s: inviter %d no longer exists, %d quota unrecoverable",
				sourceNo, record.InviterId, record.CommissionQuota))
			return record.InviterId, 0, nil
		}
		return 0, 0, err
	}

	actualReverse := record.CommissionQuota
	if inviter.Quota < actualReverse {
		actualReverse = inviter.Quota
	}
	if actualReverse < 0 {
		actualReverse = 0
	}

	if actualReverse > 0 {
		// gift_quota 与 quota 同步递减。gift_quota 平时只增，
		// 退款冲正是唯一的例外 —— 那笔钱事实上没有发生。
		if err := tx.Model(&User{}).Where("id = ?", record.InviterId).
			Updates(map[string]interface{}{
				"quota":      gorm.Expr("quota - ?", actualReverse),
				"gift_quota": gorm.Expr("gift_quota - ?", actualReverse),
			}).Error; err != nil {
			return 0, 0, err
		}
	}

	if err := markCommissionReversed(tx, &record, actualReverse); err != nil {
		return 0, 0, err
	}

	if actualReverse < record.CommissionQuota {
		logger.SysError(fmt.Sprintf(
			"commission reversal for %s incomplete: reversed %d of %d from inviter %d (balance insufficient)",
			sourceNo, actualReverse, record.CommissionQuota, record.InviterId))
	}

	return record.InviterId, actualReverse, nil
}

// markCommissionReversed 把记录标记为已冲正。
func markCommissionReversed(tx *gorm.DB, record *AffCommissionRecord, reversedQuota int64) error {
	return tx.Model(&AffCommissionRecord{}).Where("id = ?", record.Id).
		Updates(map[string]interface{}{
			"status":         AffCommissionStatusReversed,
			"reversed_quota": reversedQuota,
			"reversed_at":    helper.GetTimestamp(),
		}).Error
}
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./model/ -run ReverseCommission -v`

Expected: 全部子测试 PASS

- [ ] **Step 5: 提交**

```bash
git add model/aff_commission.go model/aff_commission_test.go
git commit -m "feat(invite): 实现 ReverseCommission 退款冲正返现

stripeChargeRefund 此前只改订单状态、不扣回已发放额度 —— 加上立即到账
的返现后，「充值→拿返现→退款」就是一条免费套利路径。

余额不足时扣到 0 为止，绝不产生负余额（邀请人可能已把返现花掉，强行扣成
负数会让他后续所有请求被拒）。差额记在 reversed_quota 与 commission_quota
的落差里并告警，是运营的真实损失，可事后查账。

邀请人已注销时只标记记录并告警。status != Granted 时直接返回，保证重复
冲正不会二次扣款。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 4: `topup_quota` 历史回填（自 P4 提前）

**Files:**
- Create: `model/migration_topup_quota.go`
- Create: `model/migration_topup_quota_test.go`
- Modify: `model/main.go`

### 为什么提前到本期

`topup_quota` 是 `RecalcUserLevel` 的唯一基准。若不回填，历史用户的 `topup_quota` 全是 0。虽然「只升不降」会保住他们现有的 `Group`，但这层保护很脆弱——任何后续改动都可能破掉。放进本期，P3 自身就安全可上线，不依赖部署顺序的人工纪律。

### 关键约束

`ChargeOrder` **不在 `model/main.go` 的 AutoMigrate 清单里**（`main.go:117-181` 只迁移了 `Order`），`charge_orders` 表由外部手工创建。全新部署上该表不存在，回填必须容忍这种情况，否则启动迁移直接失败。

- [ ] **Step 1: 写失败的测试**

`model/migration_topup_quota_test.go`：

```go
package model

import (
	"testing"

	"github.com/songquanpeng/one-api/common/config"
)

func TestBackfillTopupQuota(t *testing.T) {
	setupTestDB(t, &User{}, &Option{}, &TopUp{}, &ChargeOrder{})

	orig := config.QuotaPerUnit
	config.QuotaPerUnit = 500000
	t.Cleanup(func() { config.QuotaPerUnit = orig })

	u1 := &User{Username: "u1", AffCode: "u1", AccessToken: "t1"}
	u2 := &User{Username: "u2", AffCode: "u2", AccessToken: "t2"}
	u3 := &User{Username: "u3", AffCode: "u3", AccessToken: "t3"}
	for _, u := range []*User{u1, u2, u3} {
		if err := DB.Create(u).Error; err != nil {
			t.Fatalf("create user failed: %v", err)
		}
	}

	topups := []TopUp{
		{UserId: u1.Id, Amount: 20, TradeNo: "t-a", Status: "success"},
		{UserId: u1.Id, Amount: 30, TradeNo: "t-b", Status: "success"},
		// pending 不计入
		{UserId: u1.Id, Amount: 999, TradeNo: "t-c", Status: "pending"},
		{UserId: u2.Id, Amount: 10, TradeNo: "t-d", Status: "success"},
	}
	for i := range topups {
		if err := DB.Create(&topups[i]).Error; err != nil {
			t.Fatalf("create topup failed: %v", err)
		}
	}

	orders := []ChargeOrder{
		{UserId: u1.Id, AppOrderId: "o-a", Amount: 5, Status: StatusMap["success"]},
		// 退款订单不计入
		{UserId: u1.Id, AppOrderId: "o-b", Amount: 777, Status: StatusMap["refund"]},
		{UserId: u2.Id, AppOrderId: "o-c", Amount: 1, Status: StatusMap["success"]},
	}
	for i := range orders {
		if err := DB.Create(&orders[i]).Error; err != nil {
			t.Fatalf("create charge order failed: %v", err)
		}
	}

	if err := BackfillTopupQuota(DB); err != nil {
		t.Fatalf("BackfillTopupQuota failed: %v", err)
	}

	want := map[int]int64{
		u1.Id: (20 + 30 + 5) * 500000, // 27500000
		u2.Id: (10 + 1) * 500000,      // 5500000
		u3.Id: 0,                      // 无任何充值
	}
	for id, wantQuota := range want {
		var u User
		if err := DB.First(&u, id).Error; err != nil {
			t.Fatalf("read user %d failed: %v", id, err)
		}
		if u.TopupQuota != wantQuota {
			t.Errorf("user %d TopupQuota = %d, want %d", id, u.TopupQuota, wantQuota)
		}
	}
}

// TestBackfillTopupQuotaIdempotent 回填只能跑一次，重复调用不能翻倍。
func TestBackfillTopupQuotaIdempotent(t *testing.T) {
	setupTestDB(t, &User{}, &Option{}, &TopUp{}, &ChargeOrder{})

	orig := config.QuotaPerUnit
	config.QuotaPerUnit = 500000
	t.Cleanup(func() { config.QuotaPerUnit = orig })

	u := &User{Username: "u1", AffCode: "u1", AccessToken: "t1"}
	if err := DB.Create(u).Error; err != nil {
		t.Fatalf("create user failed: %v", err)
	}
	if err := DB.Create(&TopUp{UserId: u.Id, Amount: 20, TradeNo: "t-a", Status: "success"}).Error; err != nil {
		t.Fatalf("create topup failed: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := BackfillTopupQuota(DB); err != nil {
			t.Fatalf("第 %d 次回填报错: %v", i+1, err)
		}
	}

	var after User
	_ = DB.First(&after, u.Id).Error
	if after.TopupQuota != 10000000 {
		t.Errorf("TopupQuota = %d, want 10000000（重复回填不该累加）", after.TopupQuota)
	}

	var opt Option
	if err := DB.Where("key = ?", migratedTopupQuotaOptionKey).First(&opt).Error; err != nil {
		t.Errorf("标记位未写入 options 表: %v", err)
	}
}

// TestBackfillTopupQuotaMissingChargeOrders charge_orders 表不在
// AutoMigrate 清单里，全新部署上不存在。回填必须容忍，否则启动迁移失败。
func TestBackfillTopupQuotaMissingChargeOrders(t *testing.T) {
	// 刻意不迁移 ChargeOrder
	setupTestDB(t, &User{}, &Option{}, &TopUp{})

	orig := config.QuotaPerUnit
	config.QuotaPerUnit = 500000
	t.Cleanup(func() { config.QuotaPerUnit = orig })

	u := &User{Username: "u1", AffCode: "u1", AccessToken: "t1"}
	if err := DB.Create(u).Error; err != nil {
		t.Fatalf("create user failed: %v", err)
	}
	if err := DB.Create(&TopUp{UserId: u.Id, Amount: 20, TradeNo: "t-a", Status: "success"}).Error; err != nil {
		t.Fatalf("create topup failed: %v", err)
	}

	if err := BackfillTopupQuota(DB); err != nil {
		t.Fatalf("charge_orders 表缺失时不应报错: %v", err)
	}

	var after User
	_ = DB.First(&after, u.Id).Error
	if after.TopupQuota != 10000000 {
		t.Errorf("TopupQuota = %d, want 10000000（topups 部分仍应回填）", after.TopupQuota)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./model/ -run BackfillTopupQuota -v`

Expected: 编译失败 —— `undefined: BackfillTopupQuota`

- [ ] **Step 3: 写实现**

`model/migration_topup_quota.go`：

```go
package model

import (
	"errors"
	"fmt"

	"github.com/songquanpeng/one-api/common/config"
	"github.com/songquanpeng/one-api/common/logger"
	"gorm.io/gorm"
)

// migratedTopupQuotaOptionKey options 表里的一次性迁移标记位。
const migratedTopupQuotaOptionKey = "MigratedTopupQuotaV1"

// BackfillTopupQuota 从历史订单聚合回填 users.topup_quota。
//
// topup_quota 是 RecalcUserLevel 的唯一基准。若不回填，所有历史用户的
// 累计充值都是 0，等级判定会认为他们只够最低等级。
//
// 幂等：靠 options 表的标记位保证只执行一次。用 SET（而非 +=）赋值，
// 即使标记位被人手工删掉再跑一次也不会翻倍。
//
// 已知缺口：gift_quota 无法回填。历史赠额（注册奖励）只有 logs 表里的
// 文本日志、没有结构化金额，无法可靠还原。历史用户的 gift_quota 一律为 0，
// 上线后新产生的赠额全部准确。这个缺口是显式接受的，写在这里避免后人
// 误以为该字段自诞生起就数据完整。
func BackfillTopupQuota(db *gorm.DB) error {
	var opt Option
	err := db.Where("key = ?", migratedTopupQuotaOptionKey).First(&opt).Error
	if err == nil {
		return nil // 已迁移过
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	if config.QuotaPerUnit <= 0 {
		return errors.New("QuotaPerUnit must be positive to backfill topup_quota")
	}

	// 逐用户聚合而非一条带子查询的大 UPDATE：三种数据库对
	// UPDATE ... FROM (SELECT ...) 的语法差异很大，逐用户是唯一三库通吃的
	// 写法。这是一次性迁移，用户量级下性能不是瓶颈。
	var userIds []int
	if err := db.Model(&User{}).Pluck("id", &userIds).Error; err != nil {
		return err
	}

	// charge_orders 不在 AutoMigrate 清单里（main.go 只迁移了 Order），
	// 全新部署上该表不存在，此时跳过这部分聚合而不是报错
	hasChargeOrders := db.Migrator().HasTable(&ChargeOrder{})
	if !hasChargeOrders {
		logger.SysLog("backfill topup_quota: charge_orders table not found, skipping that source")
	}

	updated := 0
	for _, userId := range userIds {
		var topupSum float64
		if err := db.Model(&TopUp{}).
			Where("user_id = ? AND status = ?", userId, "success").
			Select("COALESCE(SUM(amount), 0)").Scan(&topupSum).Error; err != nil {
			return err
		}

		var chargeSum float64
		if hasChargeOrders {
			if err := db.Model(&ChargeOrder{}).
				Where("user_id = ? AND status = ?", userId, StatusMap["success"]).
				Select("COALESCE(SUM(amount), 0)").Scan(&chargeSum).Error; err != nil {
				return err
			}
		}

		total := AmountToQuota(topupSum + chargeSum)
		if total <= 0 {
			continue
		}
		if err := db.Model(&User{}).Where("id = ?", userId).
			Update("topup_quota", total).Error; err != nil {
			return err
		}
		updated++
	}

	if err := db.Create(&Option{Key: migratedTopupQuotaOptionKey, Value: "1"}).Error; err != nil {
		return err
	}

	logger.SysLog(fmt.Sprintf(
		"backfill topup_quota done: %d of %d users updated", updated, len(userIds)))
	return nil
}
```

- [ ] **Step 4: 在启动迁移中调用**

`model/main.go`，把 `InitGroupConfigs(db)` 那段改为：

```go
		if err := InitGroupConfigs(db); err != nil {
			logger.SysError("failed to init group configs: " + err.Error())
		}
		if err := BackfillTopupQuota(db); err != nil {
			logger.SysError("failed to backfill topup_quota: " + err.Error())
		}
```

用 `logger.SysError` 而非 `return nil, err`：回填失败不应阻止服务启动。若失败，等级判定会偏保守（只升不降保住现状），管理员可修好问题后删除 options 表里的 `MigratedTopupQuotaV1` 再重启。

- [ ] **Step 5: 运行确认通过**

Run: `go test ./model/ -run BackfillTopupQuota -v`

Expected: 3 个测试全 PASS，含 `charge_orders` 表缺失的容错用例

- [ ] **Step 6: 提交**

```bash
git add model/migration_topup_quota.go model/migration_topup_quota_test.go model/main.go
git commit -m "feat(invite): topup_quota 历史回填（自 P4 提前到 P3）

topup_quota 是 RecalcUserLevel 的唯一基准，不回填则所有历史用户的累计
充值为 0。虽然「只升不降」能保住现有 Group，但这层保护很脆弱。提前到
本期让 P3 自身安全可上线，不依赖部署顺序的人工纪律。

从 topups(status=success) 与 charge_orders(status=3) 聚合，退款与
pending 不计入。靠 options 表 MigratedTopupQuotaV1 标记位保证只跑一次，
且用 SET 而非 += 赋值，标记位被误删重跑也不会翻倍。

逐用户聚合而非一条带子查询的大 UPDATE：三种数据库对 UPDATE...FROM 的
语法差异很大，逐用户是唯一三库通吃的写法。

ChargeOrder 不在 AutoMigrate 清单里（main.go 只迁移了 Order），全新
部署上 charge_orders 表不存在，用 Migrator().HasTable 检测并跳过，
否则启动迁移会直接失败。

已知缺口：gift_quota 无法回填，历史赠额只有文本日志没有结构化金额。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 5: 接入 Stripe Checkout 链路

**Files:**
- Modify: `model/topup.go`

- [ ] **Step 1: 在事务内累加 `topup_quota` 并发放返现**

`model/topup.go` 的 `completeTopUpOrder`，把事务内更新用户额度那段（原第 146-149 行）：

```go
		if err := tx.Model(&User{}).Where("id = ?", topUp.UserId).
			Update("quota", gorm.Expr("quota + ?", quotaToAdd)).Error; err != nil {
			return err
		}

		userId = topUp.UserId
```

替换为：

```go
		// quota 是可用余额，topup_quota 是累计真实充值（等级判定的唯一依据）
		if err := tx.Model(&User{}).Where("id = ?", topUp.UserId).
			Updates(map[string]interface{}{
				"quota":       gorm.Expr("quota + ?", quotaToAdd),
				"topup_quota": gorm.Expr("topup_quota + ?", quotaToAdd),
			}).Error; err != nil {
			return err
		}

		// 邀请返现与充值入账同事务，靠 trade_no 唯一索引保证 webhook 重放幂等。
		// 除唯一键冲突外的错误会回滚整笔充值 —— Stripe 会重试，最终一致。
		//
		// 用 := 接局部变量再赋给外层：闭包签名是 func(tx *gorm.DB) error，
		// 内部没有名为 err 的变量可供 = 赋值。
		inviterId, cq, gErr := GrantCommission(
			tx, topUp.UserId, topUp.Money, quotaToAdd,
			SourceTypeStripeCheckout, topUp.TradeNo)
		if gErr != nil {
			return gErr
		}
		commissionInviterId = inviterId
		commissionQuota = cq

		userId = topUp.UserId
```

**注意返现基数用 `topUp.Money`（用户实付金额）而非 `topUp.Amount`。** `Amount` 是充值的额度单位数，`Money` 才是实付货币金额——设计文档 §5.1 明确返现按用户实付金额算。

在函数开头的变量声明处（原第 108-111 行）追加两个变量：

```go
	var userId int
	var quotaToAdd int64
	var money float64
	var currency string
	var commissionInviterId int
	var commissionQuota int64
```

- [ ] **Step 2: 事务提交后刷缓存、重算等级、记返现日志**

在 `completeTopUpOrder` 末尾的 `if userId > 0 && quotaToAdd > 0 { ... }` 块内，`RecordLog` 之后追加：

```go
		// 事务已提交，以下都在事务外做：
		// Redis 不参与事务回滚，放在事务内会在回滚时留下脏缓存
		CacheUpdateUserQuota2(userId)

		if commissionInviterId > 0 && commissionQuota > 0 {
			CacheUpdateUserQuota2(commissionInviterId)
			RecordLog(commissionInviterId, LogTypeAffCommission, fmt.Sprintf(
				"referral commission %s from invitee %d top-up",
				common.LogQuota(commissionQuota), userId))
		}

		// 等级重算放事务外：等级变化不影响资金正确性，失败可由下次充值自愈
		RecalcUserLevelAndRefreshCache(userId)
```

`CacheUpdateUserQuota2` 返回 error，需要忽略或记 log。若 `go vet` 报未使用的返回值，改为：

```go
		if err := CacheUpdateUserQuota2(userId); err != nil {
			logger.SysError("failed to refresh quota cache: " + err.Error())
		}
```

`model/topup.go` 需要新增 `common` 导入（`config` 在 P1 中已被移除）：

```go
import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/helper"
	"github.com/songquanpeng/one-api/common/logger"
	"gorm.io/gorm"
)
```

- [ ] **Step 3: 验证**

Run: `go build ./model/ && go vet ./model/ && go test ./model/`

Expected: 全部无输出 / `ok`

- [ ] **Step 4: 提交**

```bash
git add model/topup.go
git commit -m "feat(invite): Stripe Checkout 链路接入返现与 topup_quota

事务内：quota 与 topup_quota 同时累加，并调 GrantCommission 发放返现，
source_no 用 topUp.TradeNo。返现基数取 topUp.Money（用户实付金额）而非
topUp.Amount（额度单位数）。

事务外：刷新充值方与返现方的 quota 缓存、重算等级、记 LogTypeAffCommission
日志。Redis 不参与事务回滚，放在事务内会在回滚时留下脏缓存。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 6: 接入 Stripe 套餐链路与退款冲正

**Files:**
- Modify: `model/charge_order.go`

- [ ] **Step 1: 新增 tx 版的原子改单函数**

`UpdateChargeOrderStatusWithCondition` 走全局 `DB`，冲正需要与改单原子。在 `model/charge_order.go` 中该函数之后追加：

```go
// UpdateChargeOrderStatusWithConditionTx 是 UpdateChargeOrderStatusWithCondition
// 的事务版本。退款冲正需要与改单状态原子，否则改单成功而冲正失败会让
// 返现永久留在邀请人账上。
func UpdateChargeOrderStatusWithConditionTx(tx *gorm.DB, appOrderId, userId string,
	expectedStatus, newStatus int) bool {
	result := tx.Model(&ChargeOrder{}).
		Where("app_order_id = ? AND user_id = ? AND status = ?", appOrderId, userId, expectedStatus).
		Update("status", newStatus)
	return result.RowsAffected == 1
}
```

- [ ] **Step 2: 套餐链路事务内接入返现与 `topup_quota`**

`stripeChargeSuccess` 中，把原第 200 行那段（P1 已改为 `AmountToQuota`）：

```go
			//更新余额 待定手续费和用户组别的变更
			err := IncreaseUserQuota(chargeOrder.UserId, AmountToQuota(amount))
			if err != nil {
				return err
			}
```

替换为：

```go
			// quota 是可用余额，topup_quota 是累计真实充值（等级判定的唯一依据）。
			// 不用 IncreaseUserQuota：它走全局 DB 且受 BatchUpdateEnabled 影响，
			// 会脱离当前事务。
			quotaToAdd := AmountToQuota(amount)
			if err := tx.Model(&User{}).Where("id = ?", chargeOrder.UserId).
				Updates(map[string]interface{}{
					"quota":       gorm.Expr("quota + ?", quotaToAdd),
					"topup_quota": gorm.Expr("topup_quota + ?", quotaToAdd),
				}).Error; err != nil {
				return err
			}

			// 返现与入账同事务，source_no 用 app_order_id。
			// 用 := 接局部变量再赋给外层，理由同 Task 5。
			inviterId, cq, gErr := GrantCommission(
				tx, chargeOrder.UserId, amount, quotaToAdd,
				SourceTypeStripeCharge, chargeOrder.AppOrderId)
			if gErr != nil {
				return gErr
			}
			commissionInviterId = inviterId
			commissionQuota = cq
```

**注意这个事务闭包内原本用的是 `DB` 而不是 `tx`**（`model/charge_order.go:187-210` 里的 `UpdateChargeOrderStatusWithCondition`、`DB.Model(&chargeOrder)`、`DB.Model(&Bill{})` 全都绕过了事务）。本步只把新增的两处改用 `tx`；把既有的三处也改成 `tx` 属于独立的正确性修复，见 Task 6 Step 5。

在 `stripeChargeSuccess` 的 `if charge.Status == "succeeded" {` 之后、事务开始之前声明：

```go
		var commissionInviterId int
		var commissionQuota int64
```

- [ ] **Step 3: 事务提交后刷缓存与重算等级**

`stripeChargeSuccess` 末尾，`AfterChargeSuccess(...)` 调用之前追加：

```go
		if err := CacheUpdateUserQuota2(chargeOrder.UserId); err != nil {
			logger.SysError("failed to refresh quota cache: " + err.Error())
		}
		if commissionInviterId > 0 && commissionQuota > 0 {
			if err := CacheUpdateUserQuota2(commissionInviterId); err != nil {
				logger.SysError("failed to refresh inviter quota cache: " + err.Error())
			}
			RecordLog(commissionInviterId, LogTypeAffCommission, fmt.Sprintf(
				"referral commission %s from invitee %d top-up",
				common.LogQuota(commissionQuota), chargeOrder.UserId))
		}
		RecalcUserLevelAndRefreshCache(chargeOrder.UserId)
```

- [ ] **Step 4: 退款接入冲正**

把 `stripeChargeRefund`（`model/charge_order.go:157`）整个替换为：

```go
func stripeChargeRefund(charge *stripe.Charge) error {
	if charge.Status != "succeeded" {
		return nil
	}

	orderId := charge.Metadata["appOrderId"]
	userId := charge.Metadata["userId"]

	var reversedInviterId int
	var refundedUserId int

	err := DB.Transaction(func(tx *gorm.DB) error {
		// 原子改单：只有成功状态的订单才能退款
		if !UpdateChargeOrderStatusWithConditionTx(tx, orderId, userId,
			StatusMap["success"], StatusMap["refund"]) {
			// 订单已被处理或状态不符合预期
			return nil
		}

		var order ChargeOrder
		if err := tx.Where("app_order_id = ? AND user_id = ?", orderId, userId).
			First(&order).Error; err != nil {
			return err
		}
		refundedUserId = order.UserId

		// 扣回被邀请人自己的充值额度与累计充值。
		// 不扣的话退款用户的余额与 topup_quota 都虚高，等级也虚高。
		// 余额不足时扣到 0 为止，绝不产生负余额。
		quotaToRevoke := AmountToQuota(order.Amount)
		var u User
		if err := tx.Where("id = ?", order.UserId).First(&u).Error; err != nil {
			return err
		}
		actualQuota := quotaToRevoke
		if u.Quota < actualQuota {
			actualQuota = u.Quota
		}
		if actualQuota < 0 {
			actualQuota = 0
		}
		actualTopup := quotaToRevoke
		if u.TopupQuota < actualTopup {
			actualTopup = u.TopupQuota
		}
		if err := tx.Model(&User{}).Where("id = ?", order.UserId).
			Updates(map[string]interface{}{
				"quota":       gorm.Expr("quota - ?", actualQuota),
				"topup_quota": gorm.Expr("topup_quota - ?", actualTopup),
			}).Error; err != nil {
			return err
		}
		if actualQuota < quotaToRevoke {
			logger.SysError(fmt.Sprintf(
				"refund for order %s: revoked %d of %d quota from user %d (balance insufficient)",
				orderId, actualQuota, quotaToRevoke, order.UserId))
		}

		// 冲正这笔充值产生的邀请返现，防「充值→拿返现→退款」套利
		inviterId, reversed, err := ReverseCommission(tx, orderId)
		if err != nil {
			return err
		}
		if inviterId > 0 && reversed > 0 {
			reversedInviterId = inviterId
			RecordLog(inviterId, LogTypeAffCommission, fmt.Sprintf(
				"referral commission reversed %s due to order %s refund",
				common.LogQuota(reversed), orderId))
		}
		return nil
	})
	if err != nil {
		return err
	}

	// 事务外刷缓存
	if refundedUserId > 0 {
		if err := CacheUpdateUserQuota2(refundedUserId); err != nil {
			logger.SysError("failed to refresh quota cache after refund: " + err.Error())
		}
	}
	if reversedInviterId > 0 {
		if err := CacheUpdateUserQuota2(reversedInviterId); err != nil {
			logger.SysError("failed to refresh inviter quota cache after refund: " + err.Error())
		}
	}
	return nil
}
```

**这里刻意不调 `RecalcUserLevelAndRefreshCache`** —— 等级只升不降，退款后不降级是有意的产品决定（避免用户等级反复跳动引发客诉）。若将来要支持降级，需要显式实现而非依赖这里的副作用。

- [ ] **Step 5: 修 `stripeChargeSuccess` 事务闭包内误用全局 `DB` 的问题**

`stripeChargeSuccess` 的事务闭包内有三处用了 `DB` 而不是 `tx`：`UpdateChargeOrderStatusWithCondition`、`DB.Model(&chargeOrder).Updates(...)`、`DB.Model(&Bill{})` 相关。这意味着这些写入**不在事务里**——事务回滚时它们不会被撤销。

把这三处改用 `tx`：

```go
			// 原：success := UpdateChargeOrderStatusWithCondition(orderId, userId, ...)
			success := UpdateChargeOrderStatusWithConditionTx(tx, orderId, userId,
				StatusMap["create"], StatusMap["success"])
			if !success {
				return errors.New("order has already been processed or has an unexpected status")
			}
```

```go
			// 原：if err := DB.Model(&chargeOrder).Updates(...)
			if err := tx.Model(&chargeOrder).Updates(ChargeOrder{...}).Error; err != nil {
				return err
			}
```

```go
			// 原：DB.Model(&Bill{}).Where(...).First(&bill) 与 DB.Model(&bill).Updates(...)
			if err := tx.Model(&Bill{}).Where("source_id = ?", orderId).First(&bill).Error; err != nil {
				return err
			}
			if err := tx.Model(&bill).Updates(Bill{Status: StatusMap["success"]}).Error; err != nil {
				return err
			}
```

- [ ] **Step 6: 验证**

Run: `go build ./model/ && go vet ./model/ && go test ./model/`

Expected: 全部无输出 / `ok`

- [ ] **Step 7: 提交**

```bash
git add model/charge_order.go
git commit -m "feat(invite): Stripe 套餐链路接入返现，退款接入冲正

入账：quota 与 topup_quota 同时累加，并调 GrantCommission 发放返现，
source_no 用 app_order_id。改用 tx 而非 IncreaseUserQuota（后者走全局
DB 且受 BatchUpdateEnabled 影响，会脱离事务）。

退款：stripeChargeRefund 此前只改订单状态，既不扣回充值方的额度与
topup_quota（导致余额与等级虚高），也不冲正已发放的返现（导致
「充值→拿返现→退款」可以免费套利）。现在两者都在同一事务内处理，
余额不足时扣到 0 为止、绝不产生负余额，缺口记 log 告警。

顺带修 stripeChargeSuccess 事务闭包内三处误用全局 DB 而非 tx 的问题 ——
改单状态、订单详情、账单状态这三处写入原本不在事务里，事务回滚时不会
被撤销。新增 UpdateChargeOrderStatusWithConditionTx 支持事务。

退款刻意不触发等级重算：等级只升不降是有意的产品决定，避免用户等级
反复跳动引发客诉。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 7: 删除失效的 `UserLevelUpgrade`（收尾 Bug 3）

**Files:**
- Modify: `controller/stripeCharge.go`
- Modify: `controller/cryptoPay.go`

- [ ] **Step 1: 删除 `UserLevelUpgrade` 函数**

删除 `controller/stripeCharge.go:14-44` 整个 `UserLevelUpgrade` 函数。它已被 `model.RecalcUserLevel` 取代，且原实现有两处致命缺陷（条件写反导致跨级充值不升级；调用点从无登录态的 webhook context 取 userId 永远得 0）。

- [ ] **Step 2: 删除 `StripeCallback` 中的调用点**

`controller/stripeCharge.go` 的 `StripeCallback`，删除这三行：

```go
	userId := c.GetInt("id")
	err = UserLevelUpgrade(userId)
	if err != nil {
		return
	}
```

等级重算已经在 `model.stripeChargeSuccess` 事务提交后完成，这里不需要也不应该再做（webhook 没有登录态，`c.GetInt("id")` 永远是 0）。

注意删除后要确认 `err` 变量仍被使用，否则会报未使用变量。

- [ ] **Step 3: 删除 `cryptoPay.go` 中的调用点**

`controller/cryptoPay.go:62` 附近的 `UserLevelUpgrade` 调用一并删除。加密货币充值链路本期不接返现，等级重算留待后续（`source_type` 已预留扩展位）。

- [ ] **Step 4: 确认无残留引用**

Run: `grep -rn "UserLevelUpgrade" --include="*.go" .`

Expected: 除 `.claude/worktrees/` 下的旧检出外无任何命中

- [ ] **Step 5: 验证**

Run: `go build ./... && go vet ./...`

Expected: 均无输出

- [ ] **Step 6: 提交**

```bash
git add controller/stripeCharge.go controller/cryptoPay.go
git commit -m "fix(level): 删除从未生效过的 UserLevelUpgrade

该函数有两处致命缺陷：
1. 条件写成 totalQuota <= levelMap[nextLevel]，导致「超过下一级门槛
   反而不升级」，一次充 \$300 的用户永远卡在 Lv1
2. 调用点用 c.GetInt('id') 取 userId，而 Stripe webhook 请求没有登录态，
   该值永远是 0 —— 升级从来没有生效过

已由 model.RecalcUserLevel 取代：取「满足门槛的最高等级」，门槛来自
group_configs.upgrade_threshold，userId 从订单记录取而非 gin context。

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Task 8: 全量验证与变更记录

**Files:**
- Modify: `docs/CHANGELOG.md`

- [ ] **Step 1: 跑完整验证门**

```bash
go build ./... && go vet ./... && go test ./... -count=1
```

Expected: build 与 vet 无输出；所有测试包 `ok`，0 失败。`-count=1` 绕过测试缓存，避免缓存假绿。

- [ ] **Step 2: 确认本期新增测试都在跑**

Run: `go test ./model/ -v -count=1 2>&1 | grep -E "^(--- PASS|--- FAIL|ok|FAIL)"`

Expected: 至少包含 `TestRecalcUserLevel`、`TestRecalcUserLevelThresholdTie`、`TestRecalcUserLevelEdgeCases`、`TestGrantCommission`、`TestGrantCommissionIdempotent`、`TestGrantCommissionEarlyReturns`、`TestGrantCommissionRounding`、`TestReverseCommission`、`TestReverseCommissionInsufficientBalance`、`TestReverseCommissionIdempotentAndMissing`、`TestBackfillTopupQuota`、`TestBackfillTopupQuotaIdempotent`、`TestBackfillTopupQuotaMissingChargeOrders`，全部 PASS

- [ ] **Step 3: 插入 CHANGELOG 记录**

在 `docs/CHANGELOG.md` 的 `---` 分隔线之后、现有最新日期标题之前插入：

```markdown
## 2026-07-28

### feat(invite): 按等级返现的核心逻辑与充值链路接入
- **分支**: `main`
- **类型**: feat + fix
- **涉及文件**: `model/user_level.go`、`model/aff_commission.go`、`model/migration_topup_quota.go`、`model/topup.go`、`model/charge_order.go`、`model/main.go`、`controller/stripeCharge.go`、`controller/cryptoPay.go`
- **说明**: 实现 `RecalcUserLevel`（按 `topup_quota` 取满足门槛的最高等级，只升不降）、`GrantCommission`（在充值事务内按邀请人等级发放返现，`source_no` 唯一索引保证 Stripe webhook 重放幂等）、`ReverseCommission`（退款冲正，余额不足扣到 0 绝不产生负余额）。两条 Stripe 链路（Checkout 与套餐）接入返现与 `topup_quota` 累加，事务提交后刷新 Redis 余额/分组缓存。`topup_quota` 历史回填从 P4 提前到本期，使 P3 自身安全可上线。同时修三个 bug：① 删除从未生效过的 `UserLevelUpgrade`（条件写反导致跨级充值不升级，且调用点从无登录态的 webhook context 取 userId 永远得 0）；② `stripeChargeRefund` 原本只改订单状态，既不扣回充值方额度也不冲正返现，「充值→拿返现→退款」可免费套利；③ `stripeChargeSuccess` 事务闭包内三处误用全局 `DB` 而非 `tx`，导致改单状态、订单详情、账单状态这三处写入不在事务里、回滚时不会被撤销。
- **关联计划**: `docs/superpowers/plans/2026-07-27-invite-commission-p3-logic.md`
```

- [ ] **Step 4: 提交**

```bash
git add docs/CHANGELOG.md
git commit -m "docs(changelog): 记录 P3 返现核心逻辑与充值接入

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## 本期完成标准

- [ ] `RecalcUserLevel` 能让一次充 $300 的用户从 Lv1 直达 Lv5（Bug 3 回归）
- [ ] 门槛并列时取 `sort_order` 较小者，新增分组忘设门槛不会把用户拉高
- [ ] `GrantCommission` 按邀请人等级计算返现，同时累加邀请人的 `quota` 与 `gift_quota`
- [ ] 同一 `source_no` 重复调用不重复发放（webhook 重放幂等）
- [ ] 无邀请人 / 比例为 0 / 自邀请 / 分组配置缺失 / 返现额取整为 0 —— 五种情况都不产生记录
- [ ] `ReverseCommission` 冲正后余额与 `gift_quota` 同步递减，余额不足扣到 0 且不为负
- [ ] 退款同时扣回充值方的 `quota` 与 `topup_quota`
- [ ] 两条 Stripe 链路都累加 `topup_quota` 并发放返现
- [ ] 事务提交后刷新充值方与返现方的 quota 缓存、等级变化时失效 group 缓存
- [ ] `topup_quota` 回填在启动时执行一次，`charge_orders` 表不存在时不报错
- [ ] `UserLevelUpgrade` 已从全仓删除
- [ ] `go build ./... && go vet ./... && go test ./... -count=1` 全绿
- [ ] `docs/CHANGELOG.md` 已更新

## 本期不做

- 不做 4 个查询接口与用户名脱敏（P4）
- 不接加密货币充值返现（`source_type` 已预留扩展位）
- 不做兑换码 / 管理员补单的返现（设计文档明确排除：那是运营白送的额度）
- 不做等级降级（只升不降是有意的产品决定）
- 不修 `gift_quota` 的历史回填（设计文档 §7.2 已显式接受为 0）
- 不动前端（前端在 `~/code/ezlinkai-web` 独立仓库）
- 不收敛消费侧的 `500000` 硬编码（独立技术债）

## 上线注意

**本期上线后返现仍不会实际发生**，因为 P2 把 `group_configs.commission_rate` 全部设为 0。运营需要在后台逐级配置比例才会生效——这是有意的安全默认，可按等级灰度放量。

`RecalcUserLevel` 则是**立即生效**的：本期上线后第一笔充值就会按 `topup_quota` 重算等级。由于回填已包含在本期，历史用户的等级会被修正到「按累计充值本该有的等级」——**这可能让部分用户等级上升**（因为原升级逻辑从未生效，很多用户本该升级却没升）。建议上线前先在生产库上跑一次只读的 dry-run 统计受影响用户数与分布，交运营确认。
