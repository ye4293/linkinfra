# 更新记录 (CHANGELOG)

所有通过 Claude Code 辅助完成的代码变更必须记录在此文件中。

格式要求：每条记录包含日期、分支、变更类型、涉及文件和简要说明。

---

## 2026-07-28

### fix(runway): 修 Runway 图像扣费日志成本显示为实际 1/10
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `relay/controller/directvideo.go`
- **说明**: `directvideo.go:746` 的除数误写为 `5000000`（多一个零），而 `$1 = 500000 quota`，导致 Runway 图像生成的扣费日志把成本显示成实际的十分之一。同文件的 Runway 视频段（`:777`）一直是正确的 `500000`，两处相差一个零、肉眼极难发现。实际扣除的 quota 正确，仅日志字符串错——属展示与对账问题而非计费问题，但会让用户看到错误成本、财务对账偏差 10 倍。该 bug 曾记录于 `docs/superpowers/plans/2026-04-25-runway-rewrite.md:450` 但未修复。
- **关联计划**: 无

### fix(config): QuotaPerUnit 拒绝非法值，避免充值静默入账 0 quota
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `controller/option.go`、`model/option.go`、`model/option_quota_test.go`
- **说明**: `model/option.go` 中 `config.QuotaPerUnit, _ = strconv.ParseFloat(value, 64)` 丢弃了错误。管理员在后台输入框里填入非数字时 `QuotaPerUnit` 会静默变成 0，之后所有充值入账 0 quota（被 `AmountToQuota` 的守卫拦下），而消费侧硬编码的 `500000` 仍照常扣费——用户付了钱拿不到额度还继续被扣。该风险是 P1 放大的：P1 之前 `charge_order.go` 硬编码 `500000`，改该配置对那条链路无效；P1 之后两条充值链路都真正跟随它。加两道防线：`validateOptionUpdate` 挡住 API 入口（非数字或 ≤ 0 直接拒绝），`updateOptionMap` 从 DB 加载时保留旧值并告警（DB 可能被手工改过）。测试覆盖 5 种非法输入与 3 种合法输入。
- **关联计划**: 无

### feat(level): 新增 `--preview-levels` 只读预览等级重算影响面
- **分支**: `main`
- **类型**: feat
- **涉及文件**: `model/user_level_preview.go`、`model/user_level_preview_test.go`、`common/init.go`、`main.go`
- **说明**: P3 上线后 `RecalcUserLevel` 会在每笔充值后自动跑，而历史上的 `UserLevelUpgrade` 因两处 bug 从未生效，意味着会有一批用户被补到本该早就到达的等级、折扣随之变低，此前无法在改动发生前评估影响面。新增只读的 `PreviewLevelRecalc`，按与 `RecalcUserLevel` 完全一致的判定逻辑输出变更总数、`from -> to` 分布与最多 20 条样本；严格不写用户表、不写日志、不动缓存（测试中有专门断言守住）。CLI 开关 `--preview-levels` 在 DB 初始化之后、Redis/缓存预热/定时任务/HTTP 服务启动之前打印报告并退出。端到端验证（临时 sqlite、6 个用户）正确识别 3 个应升级用户且运行后库中分组全部未变；该验证同时实证了 P3 的两处设计生效——`charge_orders` 表不存在时回填跳过而非崩溃、第二次运行不再重跑回填。
- **关联计划**: 无（P3 的运维配套）

### feat(invite): 按等级返现的核心逻辑与充值链路接入
- **分支**: `worktree-p3-logic`
- **类型**: feat + fix
- **涉及文件**: `model/user_level.go`、`model/aff_commission.go`、`model/migration_topup_quota.go`、`model/topup.go`、`model/charge_order.go`、`model/order.go`、`model/main.go`、`controller/stripeCharge.go`、`controller/cryptoPay.go`
- **说明**: 实现 `RecalcUserLevel`（按 `topup_quota` 取满足门槛的最高等级，只升不降，门槛并列时取 `sort_order` 较小者）、`GrantCommission`（在充值事务内按邀请人等级发放返现，`source_no` 唯一索引保证 Stripe webhook 重放幂等；除唯一键冲突外的错误回滚整笔充值以保证最终一致，仅分组配置缺失时降级为不返现）、`ReverseCommission`（退款冲正，余额不足扣到 0 绝不产生负余额，差额记入 `reversed_quota` 并告警）。两条 Stripe 链路接入返现与 `topup_quota` 累加，事务提交后刷新 Redis 余额缓存、等级变化时失效分组缓存。`topup_quota` 历史回填从 P4 提前到本期，使 P3 自身安全可上线而不依赖部署顺序。同时修四个 bug：① 删除从未生效过的 `UserLevelUpgrade`（条件写成 `totalQuota <= levelMap[nextLevel]` 导致跨级充值不升级，且 `StripeCallback` 调用点从无登录态的 webhook context 取 userId 恒为 0）；② `stripeChargeRefund` 原本只改订单状态，既不扣回充值方的 `quota`/`topup_quota`（余额与等级虚高）也不冲正返现（「充值→拿返现→退款」可免费套利）；③ `stripeChargeSuccess` 事务闭包内三处误用全局 `DB` 而非 `tx`，改单状态、订单详情、账单状态的写入不在事务里、回滚时不会被撤销；④ 管理员手工补单原会与 Stripe 走同一入账路径，现用 `manualOtherJSON == ""` 闸门排除其计入 `topup_quota` 与返现（运营白送的额度不应白送两次）。删除 `UserLevelUpgrade` 时发现加密货币路径的 userId 来自 `response.UserId`（真实值）、升级确实在跑，为避免功能回退，在加密货币入账中补上 `topup_quota` 累加与等级重算（该渠道仍不接返现，`source_type` 已预留扩展位）。`model` 包测试 22 个全部通过。
- **关联计划**: `docs/superpowers/plans/2026-07-27-invite-commission-p3-logic.md`

### fix(test): 修复 3 个既有失败测试，`go test ./...` 恢复全绿
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `common/image/image_test.go`、`relay/channel/anthropic/beta_test.go`、`relay/channel/kling/util_test.go`
- **说明**: 三个失败均为测试自身写错、被测代码正确，且在基线 commit `e84f162`（本轮改动介入之前）实测确认为既有失败，与邀请返现体系无关。① kling `TestGetModelNameFromRequest` 传 `model` 键，但 Kling API 用 `model_name`（`adaptor.go:73-82` 注入 `model_name` 后 `delete(model)`），修正键名并补「只有 model 字段必须取不到」的契约用例与类型不匹配用例；② anthropic `TestFilterBetaFlags` 仍断言 Vertex 允许 `files-api`，而该 flag 已于 2026-06-11 从白名单移除，属改代码未改测试，改为断言 reject 并补 `fast-mode` 用例以保住两个白名单的差异覆盖；③ `common/image` 四个测试依赖 5 个 Wikimedia URL，其中 `2560px-Gfp-wisconsin...jpg` 因站点限制缩略图尺寸返回 HTTP 400、原图路径亦 404，而测试既不检查 `StatusCode` 又用 `assert` 而非 `require`，导致 HTML 错误页被喂给 `image.Decode` 后对 nil 结果调 `Bounds()` 触发 panic，炸掉整个测试二进制并吞掉同包其它测试结果。改为完全离线：jpeg/png/gif 用标准库现场编码（尺寸各异且非正方形，宽高颠倒必被断言抓到），webp 内嵌 34 字节 fixture（`x/image/webp` 仅有解码器，代价是尺寸仅 1×1），URL 分支改用 `httptest` 本地 server 以保住 `IsImageUrl` 内容嗅探与 `GetImageSizeFromUrl` 的覆盖，断言全部换为 `require`。全仓已无测试发起真实外网请求；耗时 1.3s → 0.13s。
- **关联计划**: 无

## 2026-07-27

### fix(topup): 统一充值金额换算口径并修复邮件金额 bug
- **分支**: `worktree-p1-foundation`
- **类型**: fix + 测试基础设施
- **涉及文件**: `model/testutil_test.go`、`model/quota_convert.go`、`model/quota_convert_test.go`、`model/charge_order.go`、`model/order.go`、`model/topup.go`
- **说明**: 为 model 包建立 in-memory sqlite 测试基座（此前该包零测试）。新增 `AmountToQuota` 作为金额→quota 的唯一换算入口，替换 `charge_order.go:200` 与 `order.go:82` 中硬编码的 `500000`——此前管理员修改后台 `QuotaPerUnit` 对这两条链路无效，与 `topup.go` 口径不一致。同时修复 `order.go:81` 用 `:=` 在事务闭包内遮蔽外层 `addAmount`，导致加密货币充值成功邮件金额恒为 $0 的问题。
- **关联计划**: `docs/superpowers/plans/2026-07-27-invite-commission-p1-foundation.md`

### feat(invite): 邀请返现体系数据模型与后台配置
- **分支**: `worktree-p1-foundation`
- **类型**: feat + fix
- **涉及文件**: `model/user.go`、`model/group_config.go`、`model/aff_commission.go`、`model/log.go`、`model/cache.go`、`model/main.go`、`controller/group_config.go`
- **说明**: 为按等级返现的邀请体系铺设数据结构。`users` 新增 `gift_quota`/`topup_quota` 两个只增的累计字段（用 bigint 避免现有 `Quota` 字段 `type:int` 在 MySQL 上 32 位溢出的问题，且不改动任何扣费链路）；`group_configs` 新增 `commission_rate`（默认 0，即返现全局关闭）与 `upgrade_threshold`（沿用原硬编码 levelMap 并补 Lv6）；新建 `aff_commission_records` 明细表，`source_no` 唯一索引保证 Stripe webhook 重放幂等，`rate`/`inviter_group`/用户名均为快照以保证历史记录可对账。同时修两个 bug：`controller/group_config.go` 三个 handler 只改内存不持久化 `GroupRatio` 导致重启配置漂移；`InitGroupConfigs` 用 `for range map` 分配 `sort_order` 导致每次全新部署等级排序随机（后续等级判定要用 `sort_order` 做门槛并列时的取值依据）。
- **关联计划**: `docs/superpowers/plans/2026-07-27-invite-commission-p2-schema.md`

## 2026-06-11

### fix(anthropic): 更新 Vertex AI beta flags 白名单
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `relay/channel/anthropic/beta.go`
- **说明**: 移除 Vertex 白名单中 5 个对应功能在 Vertex 上不支持的 flag（`mcp-client` x2、`files-api`、`code-execution`、`skills`），新增 3 个已验证支持的 flag（`compaction`、`context-editing`、`fallback-credit`）。经官方文档交叉验证。
- **关联计划**: 无

## 2026-06-09

### fix(streaming): SSE ping 格式改为 Claude 官方格式
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `relay/channel/common.go`, `relay/helper/common.go`, `relay/helper/stream_scanner.go`
- **说明**: 将 ping 心跳从 SSE 注释格式 (`: PING`) 改为 Claude 官方格式 (`event: ping\ndata: {"type": "ping"}`)，与上游 Claude API 透传的 ping 保持一致。同时将 stream_scanner 中部分 println 调试日志改为 logger 正式日志。
- **关联计划**: 无

### feat(streaming): 等待上游响应期间发送 SSE ping 保活
- **分支**: `stream-ping`
- **类型**: 新功能
- **涉及文件**: `relay/channel/common.go`
- **说明**: 借鉴 new-api 实现，在 `DoRequest` 中增加 pre-request ping 机制。当流式请求等待上游（如 Claude thinking）响应时，定期发送 SSE 注释 (`: PING`) 防止中间代理层（ALB/nginx）误判连接空闲并断开。stop 函数同步等待 goroutine 退出，避免与后续 StreamScannerHandler 产生并发写入竞态。
- **关联计划**: 无（小功能，直接实现）

### refactor(logging): 改用原始 JSON 记录 message_delta 事件
- **分支**: `main`
- **类型**: 重构
- **涉及文件**: `relay/controller/claude.go`
- **说明**: 将 Claude 流式响应中 `message_delta` 的日志从仅记录 `stop_reason` 改为打印完整原始 JSON，便于排查 usage、output_tokens_details 等信息。

### feat(logging): Claude 流式响应增加 stop_reason 日志
- **分支**: `main`
- **类型**: 新功能
- **涉及文件**: `relay/controller/claude.go`
- **说明**: 在流式处理中记录 Claude 响应的 stop_reason 和 OutputTokens，用于排查客户反馈的 output_token 异常问题。
