# 邀请返现体系 —— 分期实施总览

> **设计文档**: `docs/superpowers/specs/2026-07-27-invite-commission-by-level-design.md`

## 为什么分期

设计文档覆盖 4 个 bug 修复 + 3 张表的结构变更 + 3 个核心函数 + 2 条充值链路接入 + 2 个回填迁移 + 4 个 API。一次性实施的问题：

1. 单个 commit 过大，出问题难以定位回滚点
2. 数据结构变更与业务逻辑混在一起，无法分别验证
3. `model/` 包目前**零测试**，测试基座必须先立起来，否则后续所有 TDD 步骤都无处落脚

## 分期与依赖

```
P1 测试基座 + 计费口径修复
  └─> P2 数据模型与后台配置
        └─> P3 返现核心逻辑与充值接入
              └─> P4 历史回填与查询接口
```

严格串行，每期的产出是下一期的前提。

| 期 | 计划文档 | 目标 | 独立可交付性 |
|---|---|---|---|
| P1 | `2026-07-27-invite-commission-p1-foundation.md` | 立起 `model/` 包测试基座；修 Bug 1（充值绕过 `QuotaPerUnit`）与 Bug 4（充值邮件金额恒为 $0） | 纯修复，不引入新功能。上线后两条 Stripe 链路的入账口径一致 |
| P2 | `2026-07-27-invite-commission-p2-schema.md` | `users` 加 `gift_quota`/`topup_quota`；`group_configs` 加 `commission_rate`/`upgrade_threshold`；新建 `aff_commission_records`；新增 `LogTypeAffCommission` 与 `InvalidateUserGroupCache`；修 Bug 2（GroupConfig 不持久化） | 只加结构不接业务，返现比例默认 0 即全局关闭。上线后管理员可在后台配置各等级比例与门槛 |
| P3 | `2026-07-27-invite-commission-p3-logic.md` | `GrantCommission`/`ReverseCommission`/`RecalcUserLevel`；接入 Checkout 与套餐两条 Stripe 链路；退款冲正；修 Bug 3（等级升级逻辑与调用点双重失效） | 返现功能真正生效。因 P2 的比例默认 0，可通过后台逐级灰度开启 |
| P4 | `2026-07-27-invite-commission-p4-api.md` | `topup_quota` 与 `group_configs` 新列的历史回填；4 个查询接口与用户名脱敏 | 历史数据补齐，前端（`~/code/ezlinkai-web`）可对接 |

## 每期完成后的强制检查

```bash
go build ./... && go vet ./... && go test ./model/... ./controller/...
```

并同步更新 `docs/CHANGELOG.md`（项目 CLAUDE.md 强制要求）。

## 上线顺序注意

P4 的 `topup_quota` 回填**必须在 P3 的 `RecalcUserLevel` 生效前跑完**，否则所有历史用户会因 `topup_quota = 0` 被判定为 Lv1。

两种安全的上线组合：

- **推荐**：P1 → P2 → P4 的回填部分 → P3 → P4 的接口部分
- 或：P1 → P2 → P3+P4 一起上线，但部署脚本保证回填先于第一笔充值回调

计划文档按 P1→P4 编号，但 P4 的回填任务标注了「可提前到 P3 之前执行」。
