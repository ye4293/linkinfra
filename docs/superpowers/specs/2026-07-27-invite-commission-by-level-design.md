# 按等级返现的邀请体系 —— 设计文档

- **日期**: 2026-07-27
- **状态**: 已确认，待制定实施计划
- **取代**: `docs/superpowers/specs/2026-04-21-invite-tier-system-design.md`（该文档设计了 `AffQuota` + 提现划转，本次明确不做提现）

---

## 1. 背景与目标

### 1.1 现状

仓库当前只有上游 one-api 原生的最小邀请功能：

- `model/user.go:37-38` —— 只有 `AffCode` 与 `InviterId` 两个字段
- `model/user.go:255-283` `Insert(inviterId)` —— 注册瞬间一次性双向发放 `QuotaForInvitee` / `QuotaForInviter`
- `web/default/src/components/PersonalSetting.js:180` —— 前端只有一个「复制邀请链接」按钮

**没有任何充值返现 / 分佣实现。** 全仓搜索 `commission` / 返现 / 返佣零命中。`model/order.go:121` `AfterChargeSuccess` 是空 hook，只发一封邮件。

### 1.2 目标

建立**按邀请人等级计算比例的充值返现机制**，并把它建在正确的基础上——本文档同时修正 4 个既有 bug，因为不修则返现的计算基数与等级取值来源本身就是错的。

### 1.3 明确不做（YAGNI）

| 不做 | 原因 |
|---|---|
| 提现 / 划转（`AffQuota` → `Quota`） | 用户明确排除 |
| 提现到微信 / 支付宝 | 同上 |
| 多级分佣（二级、三级） | 单层 `InviterId` 足够；多级需要递归链路与防环检测，YAGNI |
| 赠金优先扣减的双余额 | 扣费入口分散在 ~25 处（`DecreaseUserQuota` / `CacheDecreaseUserQuota` / `CacheGetUserQuota`），改造风险远大于收益。本次只做记账区分，见 §6 前向兼容 |
| 赠金有效期 / 模型白名单 | 需要额外的过期任务与白名单，YAGNI |
| 兑换码、管理员手工补单计入返现 | 这两条是运营白送的额度，计入返现等于白送两次 |
| 加密货币支付计入返现 | 本期只覆盖 Stripe；加密货币链路留待后续（数据模型的 `source_type` 已预留扩展位） |
| OAuth 注册流程与界面优化 | 用户要求单独处理，不在本期范围 |

---

## 2. 已确认的产品规则

| 规则 | 决策 |
|---|---|
| 返现比例来源 | **邀请人自己的等级**（邀请人等级越高，返现比例越高） |
| 触发频次 | **被邀请人每笔充值都返**，无次数上限 |
| 计入渠道 | **仅 Stripe**（两条链路：Checkout 与套餐制） |
| 到账方式 | **立即到账、直接可用**（无待结算期、无审核） |
| 返现基准 | **用户实付金额 `amount`**，不扣手续费 |
| 赠金语义 | **仅记账区分**，`quota` 仍是唯一真实余额 |
| 赠金是否计入等级升级 | **不计入**。等级只看累计真实充值 |
| 退款处理 | **冲正**：标记记录 + 从邀请人余额扣回（不允许负余额） |

### 2.1 规则示例

邀请人小李当前 Lv4（`commission_rate = 0.08`），被邀请人小王：

| 小王行为 | 小李所得 |
|---|---|
| 首充 $20 | +$1.60 赠金（立即可用） |
| 再充 $50 | +$4.00 赠金 |
| 再充 $100 | +$8.00 赠金 |
| 退款那笔 $100 | −$8.00（冲正） |

---

## 3. 必须一并修正的既有 bug

这 4 个 bug 直接影响返现的正确性，不修则返现建在错误的基数之上。

### Bug 1 —— 充值入账绕过 `QuotaPerUnit`

`model/charge_order.go:200`：
```go
err := IncreaseUserQuota(chargeOrder.UserId, int64(amount*500000))
```
硬编码 `500000`，不跟随后台可配的 `config.QuotaPerUnit`。而另一条 Stripe 链路 `model/topup.go:124` 用的是 `float64(topUp.Amount) * config.QuotaPerUnit`。管理员改了 `QuotaPerUnit` 之后两条链路的入账口径不一致，返现基数也随之错乱。

**修**：改为 `config.QuotaPerUnit`。

### Bug 2 —— GroupConfig 变更不持久化到 options 表

`controller/group_config.go:67`、`:112`、`:151` 只同步内存 `common.GroupRatio`，从未调用 `model.UpdateOption("GroupRatio", ...)`。重启后 options 表的旧值覆盖内存，但 `group_configs` 表仍是新值，两处永久漂移。

**修**：三个 handler 在写 DB 成功后，把完整的 `GroupRatio` map 序列化并 `model.UpdateOption("GroupRatio", ...)` 持久化。

**注意**：新增的 `commission_rate` 与 `upgrade_threshold` **不进 options 表、不进内存 map**，而是每次使用时直接查 `group_configs` 表。返现与升级都发生在充值回调里（低频），无需缓存，从而根本上避免这一类漂移 bug 再次出现。

### Bug 3 —— 等级升级逻辑与调用点双重失效

`controller/stripeCharge.go:31-42`：
```go
if user.Group == currentLevel &&
   totalQuota > levelMap[currentLevel] &&
   totalQuota <= levelMap[nextLevel] {   // ← 超过下一级门槛反而不升级
```
一次充 $300 的用户 `totalQuota = 150000000`，超过 Lv5 门槛 `250*500000`，所有分支条件都不满足 → 停在 Lv1。

`controller/stripeCharge.go:105-106`：
```go
userId := c.GetInt("id")        // ← webhook 请求无登录态，永远是 0
err = UserLevelUpgrade(userId)
```
升级从来没有生效过。

**修**：见 §5.2，逻辑下移到 model 层重写。

### Bug 4 —— 充值成功邮件金额永远是 $0

`model/order.go:47`（声明）与 `:81`（内层同名新变量）：
```go
var addAmount float64          // 外层，始终为 0
...
addAmount := response.ValueForwardedCoin   // 内层 :=，遮蔽了外层
...
AfterChargeSuccess(response.UserId, addAmount)   // :93 用的是外层的 0
```

**修**：内层改为赋值而非声明。同时把 `AfterChargeSuccess` 的签名从 `(userId int, addAmount float64)` 改为接收结构体，为后续扩展留位。

---

## 4. 数据模型

### 4.1 `users` 表新增 2 列

```go
// model/user.go 的 User 结构体
GiftQuota  int64 `json:"gift_quota"  gorm:"type:bigint;default:0;column:gift_quota"`   // 累计获赠总额（只增）
TopupQuota int64 `json:"topup_quota" gorm:"type:bigint;default:0;column:topup_quota"`  // 累计真实充值总额（只增）
```

**两者都是只增不减的累计量，不是可用余额。** 这是刻意的设计，见 §6。

- `gift_quota` 累计：注册奖励（`QuotaForNewUser` / `QuotaForInvitee` / `QuotaForInviter`）+ 邀请返现
- `topup_quota` 累计：仅 Stripe 真实入账（后续可扩展加密货币）
- 退款冲正时 `gift_quota` 与 `topup_quota` **同步递减**——这是唯一的例外，因为那笔钱在事实上没有发生

### 4.2 `group_configs` 表新增 2 列

与现有 `discount` 列同构，复用已有的 `/api/group-config/` 管理 API：

```go
// model/group_config.go 的 GroupConfig 结构体
CommissionRate   float64 `json:"commission_rate"   gorm:"type:decimal(5,4);default:0"` // 该等级的返现比例，0~1
UpgradeThreshold int64   `json:"upgrade_threshold" gorm:"type:bigint;default:0"`        // 升到该等级所需累计充值 quota
```

回填后的目标状态（`upgrade_threshold` 沿用原硬编码值以保持行为不变，`commission_rate` 默认全 0，由运营在后台按需开启）：

| group_key | discount | commission_rate | upgrade_threshold |
|---|---|---|---|
| Lv1 | 1.00 | 0 | 0 |
| Lv2 | 现值 | 0 | 2500000（$5） |
| Lv3 | 现值 | 0 | 25000000（$50） |
| Lv4 | 现值 | 0 | 50000000（$100） |
| Lv5 | 现值 | 0 | 125000000（$250） |
| Lv6 | 现值 | 0 | 250000000（$500） |

`common/group-ratio.go:9` 的默认分组含 Lv1~Lv6，但原硬编码升级逻辑只覆盖到 Lv5，Lv6 没有升级路径。本次给 Lv6 补一个明确门槛 $500。

**这个值必须显式回填，不能留默认 0** —— 若 Lv6 的 `upgrade_threshold` 为 0，则任何新用户都同时满足 Lv1 与 Lv6 的门槛，`RecalcUserLevel` 会把所有人拉到 Lv6（最低折扣、最高返现比例）。这是回填步骤中最危险的一处，见 §5.2 的并列门槛处理规则。

**`commission_rate` 默认 0 意味着上线后返现处于关闭状态**，运营在后台逐级配置后才生效。这是有意的安全默认。

**校验**：`commission_rate` 必须在 `[0, 1]`，`upgrade_threshold` 必须 `>= 0`，在 `controller/group_config.go` 的 Create / Update handler 中与现有 `discount` 校验并列。

### 4.3 新表 `aff_commission_records`

```go
// model/aff_commission.go
type AffCommissionRecord struct {
    Id              int     `json:"id" gorm:"primaryKey;autoIncrement"`
    InviterId       int     `json:"inviter_id" gorm:"index;not null"`
    InviteeId       int     `json:"invitee_id" gorm:"index;not null"`
    InviterUsername string  `json:"inviter_username" gorm:"type:varchar(64)"`  // 快照，防改名断链
    InviteeUsername string  `json:"invitee_username" gorm:"type:varchar(64)"`  // 快照，防改名断链
    SourceType      string  `json:"source_type" gorm:"type:varchar(32);not null"` // stripe_checkout | stripe_charge
    SourceNo        string  `json:"source_no" gorm:"type:varchar(128);uniqueIndex;not null"`
    TopupAmount     float64 `json:"topup_amount" gorm:"type:decimal(20,6)"`    // 被邀请人实付金额（USD）
    TopupQuota      int64   `json:"topup_quota"`                                // 换算后的充值 quota
    Rate            float64 `json:"rate" gorm:"type:decimal(5,4)"`             // 比例快照
    InviterGroup    string  `json:"inviter_group" gorm:"type:varchar(32)"`     // 等级快照
    CommissionQuota int64   `json:"commission_quota"`                          // 实发返现 quota
    Status          int     `json:"status" gorm:"default:1;index"`             // 1=已发放 2=已冲正
    ReversedQuota   int64   `json:"reversed_quota" gorm:"default:0"`           // 实际扣回的额度（可能小于 CommissionQuota）
    CreatedAt       int64   `json:"created_at" gorm:"bigint;index"`
    ReversedAt      int64   `json:"reversed_at" gorm:"bigint;default:0"`
}
```

设计要点：

- **`SourceNo` 唯一索引是幂等的核心。** Stripe 会重放 webhook，唯一索引是唯一可靠的防重复发放手段，比事务本身更重要。
- **`Rate` 与 `InviterGroup` 是快照。** 后台改了比例不影响已发放记录的对账，历史记录永远可解释。
- **用户名是快照。** 用户改名后对账不断链（用户 id 也保留，两者互补）。
- **`ReversedQuota` 与 `CommissionQuota` 分开记。** 冲正时若邀请人余额不足，实际扣回额小于原发放额，这个差额是运营的真实损失，必须能查出来。
- 该表建在主库 `DB`，**不是** `LOG_DB`（`model/log.go` 的 logs 表可能在独立库）。

### 4.4 新增 log type

`model/log.go:52-59` 的常量组末尾追加：
```go
LogTypeAffCommission  // 邀请返现（发放与冲正）
```
追加在末尾以保持现有 iota 值不变，避免历史日志的 type 语义漂移。

### 4.5 迁移登记

`model/main.go` 的 AutoMigrate 序列中追加 `&AffCommissionRecord{}`。`User` 与 `GroupConfig` 已在序列中（`main.go:173`），新增列由 AutoMigrate 自动加上。

---

## 5. 核心逻辑

### 5.1 `GrantCommission` —— 返现发放

新建 `model/aff_commission.go`：

```go
// GrantCommission 在给定事务内为被邀请人的一笔充值发放邀请返现。
// 必须在充值入账的同一事务内调用，以保证「入账 + 返现 + 明细」原子。
// 返回 nil 表示无需返现或已成功发放（含重放场景）。
func GrantCommission(tx *gorm.DB, inviteeId int, topupAmount float64,
                     topupQuota int64, sourceType, sourceNo string) error
```

执行顺序：

1. 查被邀请人的 `inviter_id`；`== 0` → 返回 `nil`（早退，不产生记录）
2. `inviter_id == inviteeId` → 返回 `nil`（自邀请。理论上不可能，但 DB 被手工改过或未来新增绑定入口时会出现，必须挡住）
3. 查邀请人的 `group`，再查 `group_configs` 得到 `commission_rate`
   - 分组配置不存在 → 记 log 并返回 `nil`（视为无返现，不阻塞充值）
4. `rate <= 0` → 返回 `nil`
5. `commissionQuota = int64(topupAmount * rate * config.QuotaPerUnit)`；`<= 0` → 返回 `nil`
6. `INSERT aff_commission_records`
   - **唯一键冲突 → 视为 webhook 重放，返回 `nil`**（幂等的关键路径）
   - 其他错误 → 返回 `err`
7. 邀请人 `quota += commissionQuota`、`gift_quota += commissionQuota`

**错误处理决策（重要）**：除唯一键冲突外的 DB 错误一律**返回 err、回滚整笔充值**。

这看似激进，但是唯一正确的选择：Stripe 会重试 webhook，重试时靠 `source_no` 唯一索引保证不重复入账，最终一致。反之若采用「返现失败只记 log 不阻塞充值」，返现就会静默丢钱，后期只能靠人工对账捞回——对一个长期项目而言这是不可接受的技术债。

**返现基准取用户实付 `amount`，不取扣手续费后的 `realAmount`**。理由是可解释性：对用户承诺「充值额的 5%」必须字面成立，否则客服无法解释。毛利通过调低 `commission_rate` 控制，而不是通过隐藏基数。

### 5.2 `RecalcUserLevel` —— 等级重算

新建 `model/user_level.go`，替换 `controller/stripeCharge.go:14-44` 的 `UserLevelUpgrade`：

```go
// RecalcUserLevel 依据累计真实充值（users.topup_quota）重算用户等级，
// 取「满足门槛的最高等级」而非逐级 +1。只升不降。
// 门槛来自 group_configs.upgrade_threshold，实时查询、不缓存。
func RecalcUserLevel(userId int) error
```

逻辑：

1. 读 `user.TopupQuota`（**不再是** `Quota + UsedQuota`，赠金不再抬高等级）
2. 读全部 `group_configs`，按 `upgrade_threshold` 降序；**门槛相同时按 `sort_order` 升序**
3. 取第一个 `topup_quota >= upgrade_threshold` 的分组
4. 若目标分组的 `upgrade_threshold` 大于当前分组的 → 更新 `user.Group`；否则不动（只升不降）
5. 等级变化时 `RecordLog(userId, LogTypeSystem, ...)`

**门槛并列时取 `sort_order` 较小者（即较低等级）**，这样运营新增分组时若忘记设置门槛（默认 0），用户会落到 Lv1 而不是被意外拉到新分组。安全方向的默认。

**「只升不降」的判定依据是 `upgrade_threshold` 的大小关系，而非 group_key 的字典序**，这样运营新增 / 重命名等级时不会破坏语义。当前分组在 `group_configs` 中不存在时（如手工改过 DB），视其门槛为 0，即允许升级。

**调用时机**：在充值事务**提交后**调用，`userId` 从订单记录取（不从 gin context 取——webhook 没有登录态，这正是 Bug 3 的第二半）。放在事务外是因为等级变化不影响资金正确性，失败可由下一次充值自愈。

`controller/stripeCharge.go` 中的 `UserLevelUpgrade` 与调用点 `:105-106` 一并删除。

### 5.3 `ReverseCommission` —— 退款冲正

`model/charge_order.go:157` 的 `stripeChargeRefund` 目前只改订单状态，**不扣回已发放的额度**——这本身已是漏洞。加上立即到账的返现后，「充值 → 拿返现 → 退款」将成为免费套利路径。

```go
// ReverseCommission 冲正一笔已发放的返现。
// 余额不足时扣到 0 为止并记 log 告警，绝不产生负余额。
func ReverseCommission(tx *gorm.DB, sourceNo string) error
```

逻辑：

1. 按 `source_no` 查记录；不存在 → 返回 `nil`（那笔充值本来没有返现）
2. `status != 1` → 返回 `nil`（已冲正，幂等）
3. 读邀请人当前 `quota`，`actualReverse = min(quota, commission_quota)`
4. 邀请人 `quota -= actualReverse`、`gift_quota -= actualReverse`
5. 更新记录 `status = 2`、`reversed_quota = actualReverse`、`reversed_at = now`
6. `actualReverse < commission_quota` → `logger.SysError` 告警（差额是运营的真实损失）
7. `RecordLog(inviterId, LogTypeAffCommission, ...)`

同时在 `stripeChargeRefund` 中扣回**被邀请人自己**的充值额度与 `topup_quota`（同样不允许负余额），否则退款用户的余额与 `topup_quota` 都虚高，等级也虚高。

### 5.4 Redis 缓存失效

`model/cache.go` 对用户数据有两层 Redis 缓存，TTL 均为 `config.SyncFrequency`（`cache.go:22-28`）：

| 缓存 key | 读取函数 | 影响 |
|---|---|---|
| `user_quota:%d` | `CacheGetUserQuota`（`cache.go:186`） | 不失效则邀请人在 TTL 内看不到、也用不了刚到账的返现 |
| `user_group:%d` | `CacheGetUserGroup`（`cache.go:60`） | 不失效则等级升级后仍按旧分组折扣计费 |

**必须在事务提交后失效**（不能在事务内——事务可能回滚，而 Redis 操作不参与回滚，会造成缓存与 DB 不一致）：

- `GrantCommission` 成功 → `CacheUpdateUserQuota2(inviterId)`（`cache.go:217`，已存在，直接从 DB 重读并回写）
- `ReverseCommission` 成功 → 同上
- 充值入账 → `CacheUpdateUserQuota2(inviteeId)`
- `RecalcUserLevel` 改变了等级 → 失效 group 缓存

`user_group` 目前**没有**失效函数，需要在 `model/cache.go` 新增（与已有的 `InvalidateUserChannelRatiosCache`（`cache.go:124`）同构）：

```go
// InvalidateUserGroupCache 清除指定用户的分组缓存。
func InvalidateUserGroupCache(id int) {
    if id <= 0 || !common.RedisEnabled {
        return
    }
    if err := common.RedisDel(fmt.Sprintf("user_group:%d", id)); err != nil {
        logger.SysError("Redis del user group error: " + err.Error())
    }
}
```

缓存操作失败一律只记 log、不返回错误——资金已经落库，缓存最多陈旧一个 TTL，不能因为 Redis 抖动就让充值回调返回失败触发 Stripe 重试。

**已知的既有缺陷（本次不扩大范围）**：`model/topup.go:146` 与 `model/charge_order.go:200` 现有的充值入账本来就没有失效 quota 缓存。本次在这两处补上，但其他历史路径（如兑换码 `model/redemption.go`）不在本次范围内。

### 5.5 完整数据流

```
Stripe webhook 到达
  ↓
[事务开始]
  ├─ 原子改单状态（复用现有幂等锁）
  │    topup.go:120  status != "pending" 早退
  │    charge_order.go:187  UpdateChargeOrderStatusWithCondition
  ├─ 被邀请人 quota       += topupQuota
  ├─ 被邀请人 topup_quota += topupQuota            ← 新增
  └─ GrantCommission(tx, inviteeId, amount, topupQuota, sourceType, sourceNo)
       ├─ inviter_id == 0 → nil
       ├─ rate <= 0 → nil
       ├─ INSERT 明细（source_no 唯一索引；冲突 → nil）
       └─ 邀请人 quota += c, gift_quota += c
[事务提交]
  ↓
CacheUpdateUserQuota2(inviteeId)                    ← 事务外，刷新充值方余额缓存
CacheUpdateUserQuota2(inviterId)                    ← 事务外，刷新返现方余额缓存
RecalcUserLevel(inviteeId)                          ← 事务外，等级变化时失效 group 缓存
RecordLog(inviterId, LogTypeAffCommission, ...)     ← 事务外
AfterChargeSuccess(...)                             ← 事务外，现有邮件通知
```

两条 Stripe 链路的接入点：

| 链路 | 入账位置 | `source_no` 取值 |
|---|---|---|
| Checkout | `model/topup.go:146-149` 事务内 | `topUp.TradeNo` |
| 套餐制 | `model/charge_order.go:200` 事务内 | `chargeOrder.AppOrderId` |

---

## 6. 前向兼容（长期基础）

`gift_quota` 与 `topup_quota` 被刻意设计为**累计量**（只增，退款冲正是唯一例外，见 §4.1），而不是可用余额。

未来若要升级为真正的双余额（赠金优先扣减），可以新增 `gift_balance` 字段，从 `gift_quota` 与消费明细推导初值，**无需回溯改写历史数据**。

反之，如果现在就把 `gift_quota` 当可用余额来加减，未来想拆分「累计获赠」与「赠金余额」这两个语义时，会发现数据已经混死——两个语义共用一个字段，任何一笔历史扣减都无法区分是消费还是冲正。

同理，`aff_commission_records.source_type` 用字符串而非枚举整数，为加密货币等新渠道预留扩展位而不需要迁移。

---

## 7. 历史数据回填

### 7.1 `topup_quota`（必须回填）

`topup_quota` 是等级判定的新基准。若不回填，**所有历史用户会掉回 Lv1**。

从两张订单表聚合：

```sql
-- 伪代码；实际以 Go 迁移函数实现，兼容 SQLite / MySQL / PostgreSQL
UPDATE users SET topup_quota =
    COALESCE((SELECT SUM(amount) FROM topups
              WHERE user_id = users.id AND status = 'success'), 0) * <QuotaPerUnit>
  + COALESCE((SELECT SUM(amount) FROM charge_orders
              WHERE user_id = users.id AND status = 3), 0) * <QuotaPerUnit>;
```

- `charge_orders.status = 3` 即 `StatusMap["success"]`（`model/charge_order.go:43`）
- 退款订单（`status = 5`）不计入
- 实现为**一次性迁移函数**，用 options 表的一个标记位（如 `MigratedTopupQuotaV1`）保证只执行一次
- 迁移在 `InitGroupConfigs` 之后执行（`model/main.go:182` 附近）

### 7.2 `gift_quota`（已知缺口，显式接受为 0）

历史赠额（注册奖励）没有独立记录，`logs` 表的注册奖励日志只有文本内容、没有结构化金额，无法可靠回填。

**决策：历史用户的 `gift_quota` 一律为 0。** 这个缺口写进本文档是为了避免后人误以为该字段自诞生起就数据完整。上线后新产生的赠额全部准确。

### 7.3 `group_configs` 新列

`upgrade_threshold` 从原硬编码值回填（见 §4.2 表格），保持升级行为不变。`commission_rate` 保持默认 0，返现处于关闭状态，由运营在后台开启。

同为一次性迁移函数，同样用标记位保证幂等。

---

## 8. 接口契约

前端在 `~/code/ezlinkai-web` 单独实现，本文档只定后端契约。

| 接口 | 权限 | 返回 |
|---|---|---|
| `GET /api/user/aff/stats` | 登录（`selfRoute`） | 邀请码、邀请人数、已充值人数、累计返现 quota、当前等级、当前返现比例、下一级门槛与差额 |
| `GET /api/user/aff/records` | 登录 | 分页返现明细：时间、被邀请人用户名（脱敏）、充值额、比例、返现额、状态（已发放 / 已冲正） |
| `GET /api/user/invitees` | 登录 | 分页被邀请人列表：用户名（脱敏）、注册时间、是否已充值 |
| `GET /api/aff/report` | `AdminAuth` | 全局返现总额、Top 推广人排行、异常账号（邀请数高但充值率为 0）、冲正总额与损失差额 |

新建 `controller/aff.go` 承载全部 4 个 handler。路由注册在 `router/api-router.go`：前 3 个挂在现有 `selfRoute` 组（`api-router.go:54` 附近），第 4 个挂在 AdminAuth 组。

**用户名脱敏规则**：保留首尾字符，中间以 `*` 替代（如 `zhangsan` → `z******n`；长度 ≤ 2 时全部替代）。邀请人不应看到被邀请人的完整账号信息。

---

## 9. 影响范围

### 9.1 需要数据迁移

是。新增 2 张表列组 + 1 张新表 + 2 个一次性回填函数。全部通过 GORM AutoMigrate 与幂等迁移函数完成，**不涉及任何 DROP / TRUNCATE / 无条件 DELETE**。

### 9.2 对现有功能的影响

| 模块 | 影响 |
|---|---|
| 计费 / 扣费链路 | **零影响**。`DecreaseUserQuota` 等 ~25 处调用点全部不动 |
| Stripe Checkout 充值 | 事务内多两条 UPDATE 与一次 GrantCommission |
| Stripe 套餐充值 | 同上；另修正 `QuotaPerUnit` 口径（Bug 1） |
| 等级升级 | 行为变化：从「实际失效」变为「真正生效」。门槛值保持不变，但历史用户会在回填后被重算到应有等级 |
| 加密货币充值 | 仅修 Bug 4（邮件金额）；不接返现 |
| 兑换码 / 管理员补单 | 不动 |
| 分组配置管理 API | 新增 2 个字段的读写与校验；修 Bug 2 的持久化 |
| 前端 `web/default/` | 不动（实际前端在独立仓库） |

### 9.3 风险点

1. **等级重算会让部分历史用户等级上升**（因为原升级逻辑失效，很多用户本该升级却没升）。这会让他们的 `discount` 变低、返现比例变高。上线前需要先跑一次 dry-run 统计受影响用户数与分布，交运营确认。
2. **`commission_rate` 默认 0** 是安全默认，但意味着上线后需要运营手动开启，否则返现功能"看起来没生效"。需要在交付说明中写明。
3. **返现失败会回滚充值**（§5.1 的有意决策）。若 `group_configs` 表出现异常导致查询持续失败，会阻塞充值入账。缓解：分组配置不存在时降级为「无返现」而非报错（§5.1 第 2 步）。

---

## 10. 验证方式

### 10.1 单元测试

- `GrantCommission`：inviter 为 0 / 自邀请（inviter == invitee）/ rate 为 0 / rate 正常 / 重复 sourceNo（幂等）/ 分组配置缺失（降级）
- `RecalcUserLevel`：跨级跳跃（一次充 $300 从 Lv1 直达 Lv5，即 Bug 3 的回归测试）/ 只升不降 / 门槛并列时取 sort_order 较小者 / 新增分组门槛为 0 时不把用户拉高 / 分组表为空 / 当前分组不在表中
- `ReverseCommission`：正常冲正 / 余额不足扣到 0 / 重复冲正（幂等）/ 记录不存在
- 用户名脱敏：长度 1 / 2 / 3 / 正常

### 10.2 集成验证

1. 构造 Lv4 邀请人 + 被邀请人，走 Stripe test mode 完成一笔 $100 Checkout 充值，断言：
   - 被邀请人 `quota` 与 `topup_quota` 各 +50000000
   - 邀请人 `quota` 与 `gift_quota` 各 +4000000（8%）
   - `aff_commission_records` 一条记录，`rate = 0.08`、`inviter_group = "Lv4"`
2. **重放同一 webhook payload**，断言余额与记录数完全不变（幂等）
3. 触发 Stripe test mode 退款，断言邀请人被扣回 4000000、记录 `status = 2`
4. 邀请人余额人为改为 1000000 后再退款，断言扣到 0、`reversed_quota = 1000000`、日志有告警
5. 4 个接口各调一次，核对字段完整性与脱敏生效

### 10.3 强制检查

```bash
go build ./... && go vet ./...
```

---

## 11. 关键文件清单

### 新建

| 文件 | 内容 |
|---|---|
| `model/aff_commission.go` | `AffCommissionRecord` 模型、`GrantCommission`、`ReverseCommission`、查询函数 |
| `model/user_level.go` | `RecalcUserLevel` |
| `model/migration_topup_quota.go` | 两个一次性回填函数（`topup_quota`、`group_configs` 新列） |
| `controller/aff.go` | 4 个接口 handler + 用户名脱敏工具 |

### 修改

| 文件 | 改动 |
|---|---|
| `model/user.go` | `User` 新增 `GiftQuota` / `TopupQuota`；`Insert` 中注册奖励同步累加 `gift_quota` |
| `model/group_config.go` | `GroupConfig` 新增 `CommissionRate` / `UpgradeThreshold`；新增按门槛降序查询函数 |
| `model/topup.go` | `completeTopUpOrder` 事务内累加 `topup_quota` + 调 `GrantCommission`；提交后 `RecalcUserLevel` |
| `model/charge_order.go` | Bug 1（`QuotaPerUnit`）；累加 `topup_quota`；调 `GrantCommission`；`stripeChargeRefund` 接冲正 |
| `model/order.go` | Bug 4（`addAmount` 遮蔽）；`AfterChargeSuccess` 改结构体签名 |
| `model/log.go` | 新增 `LogTypeAffCommission` |
| `model/cache.go` | 新增 `InvalidateUserGroupCache` |
| `model/main.go` | AutoMigrate 追加 `AffCommissionRecord`；调用两个回填函数 |
| `controller/group_config.go` | Bug 2（持久化 `GroupRatio`）；新增 2 字段校验 |
| `controller/stripeCharge.go` | 删除 `UserLevelUpgrade` 与调用点 `:105-106` |
| `controller/cryptoPay.go` | 删除 `UserLevelUpgrade` 调用（`:62`） |
| `router/api-router.go` | 注册 4 条新路由 |

---

## 12. 后续独立任务（不在本期）

1. **OAuth 注册流程与界面优化** —— 用户明确要求单独处理。同时修复 GitHub / Google / 微信注册硬编码 `Insert(0)` 导致的邀请关系丢失（`controller/github.go:76,241`、`controller/google.go:59,191`、`controller/wechat.go:90`）
2. **`AffCode` 碰撞重试** —— `model/user.go:265` 与 `controller/user.go:378` 只生成 4 位随机码，而 `aff_code` 列有 `uniqueIndex`，用户量上升后注册会因唯一键冲突失败。可在本期或 OAuth 任务中顺带修复
3. **加密货币充值接入返现** —— `source_type` 已预留
4. **前端邀请页** —— 在 `~/code/ezlinkai-web` 实现
5. **`gift_quota` 升级为可扣减的赠金余额** —— 见 §6 前向兼容
