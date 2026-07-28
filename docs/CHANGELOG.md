# 更新记录 (CHANGELOG)

所有通过 Claude Code 辅助完成的代码变更必须记录在此文件中。

格式要求：每条记录包含日期、分支、变更类型、涉及文件和简要说明。

---

## 2026-07-28

### fix(db): 行锁改用 clause.Locking，并修 Redis 辅助函数的空指针 panic
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `model/topup.go`、`model/topup_stripe.go`、`model/redemption.go`、`model/topup_complete_test.go`、`common/redis.go`
- **说明**: **① 行锁此前根本不存在**：三处充值/兑换用的 `Set("gorm:query_option", "FOR UPDATE")` 是 GORM v1 的 API，v2 里只往 Statement settings 存了个值、不生成任何 SQL（实测 `ToSQL` 输出裸 SELECT），并发全靠事务内的 status 判断兜底。改用 `clause.Locking{Strength: LockingStrengthUpdate}`，无需方言分支——sqlite 驱动的 `"FOR"` ClauseBuilder 注释 `SQLite3 does not support row-level locking` 后直接 return（已实测静默剥离），PG 无覆盖走默认渲染 `FOR UPDATE`，MySQL 的覆盖只改写 `SHARE`。**② Redis 辅助函数会 panic**：写回归测试时发现 `common.RedisEnabled` 默认值是 `true`，只有 `InitRedisClient()` 发现没配 `REDIS_CONN_STRING` 才置 false；而 `RedisSet`/`RedisGet`/`RedisDel`/`RedisDecrease` 四个裸函数直接 `RDB.Set(...)` 没有 nil 检查，任何在 `InitRedisClient()` 之前或没走完整启动流程的场景调用都会空指针 panic。同文件的 `RedisLockAcquire`/`RedisLockRelease`/`RedisIncrMod` 本就是 `!RedisEnabled || RDB == nil` 双条件守卫，只有这四个漏了，已补齐。**③ 补测试**：`completeTopUpOrder` 此前零测试覆盖，而 P3 往里加了 `topup_quota` 累加与返现发放——正是这个缺口让上述 panic 一直没被发现。新增两条测试覆盖 Stripe Checkout 完整入账链路与管理员补单不返现的分支。
- **关联计划**: 无（PG 兼容专项的后续）

### fix(pg): PostgreSQL 兼容性修复（15 处 / 9 个文件）
- **分支**: `main`
- **类型**: fix
- **涉及文件**: `model/channel.go`、`model/topup.go`、`model/user.go`、`model/ability.go`、`model/model_metrics.go`、`model/log.go`、`model/redemption.go`、`model/charge_config.go`、`model/order.go`
- **说明**: 本项目的 PG 分支此前从未被真正验证过——`AutoMigrate` 的第一张表（`Channel`，`type:mediumtext`）就会失败。修复三类问题：**① 三个启动阻断点**：`mediumtext`/`longtext` 在 PG 不存在（PG 只有 `text`，无长度分级）；`Quota`/`UsedQuota` 是 Go `int64` 却标 `type:int`，PG 的 4 字节 int 装不下 `main.go:39` 给 root 账号写的 `500000000000000`，root 用户建不出来（顺带修掉 MySQL 上的静默截断，并与 P2 的 `gift_quota`/`topup_quota` 对齐）。**② 反引号与保留字**：`ability.go:37/235` 硬编码反引号且没用同函数 `:24-29` 已备好的 `groupCol` 分支（选渠道是核心路径，PG 上等于服务不可用）；`charge_config.go:21` 的 `order`；`channel.go:331` 漏掉的 `keyCol` 分支。**③ 方言语义**：`Ability.Enabled`/`Log.IsStream` 是 Go `bool` → PG `boolean`，与 `0`/`1` 比较报 `operator does not exist`；PG 不支持 `UPDATE t JOIN` 且 `SET` 子句不允许表限定名，原代码把 MySQL 与 PG 归为一类，已拆成独立分支；`ifnull` → `COALESCE`；`HOUR(FROM_UNIXTIME())` 改为「减取模得桶起点」（没用 `(x % 86400) / 3600`，因为 MySQL 的 `/` 是浮点除法、三库结果不一致），小时换算与零填充移到 Go 侧，顺带修正了原实现用数据库服务器时区、而调用方用 UTC 的时区错配；用户输入的字符串比给 int 列改用 `helper.String2Int`。**⚠️ 本机无 docker/psql，未能在真实 PG 上端到端验证**，正确性依赖静态判断与 PG 语义的硬事实，上线前需在真实 PG 实例上按计划文档末尾的 6 项清单验证。
- **关联计划**: `docs/superpowers/plans/2026-07-28-postgres-compat.md`

### feat(invite): 邀请返现查询接口
- **分支**: `worktree-p4-api`
- **类型**: feat
- **涉及文件**: `model/aff_query.go`、`model/aff_query_test.go`、`controller/aff.go`、`controller/aff_test.go`、`router/api-router.go`
- **说明**: 新增 4 个只读查询接口供前端（`~/code/ezlinkai-web`，独立仓库）对接：`GET /api/user/aff/stats`（邀请汇总：邀请码、人数、已充值人数、累计返现、当前等级与返现比例）、`GET /api/user/aff/records`（返现明细分页，按时间倒序，已冲正记录也返回以便用户看到「这笔为什么被扣回」）、`GET /api/user/invitees`（被邀请人分页）、`GET /api/aff/report`（管理员全局报表：发放/冲正总额、因邀请人余额不足没扣回的差额、Top 推广人排行）。前三个接口的被邀请人用户名一律脱敏（保留首尾、中间打星，按 rune 处理避免中文乱码），返现明细投影成 DTO 而非直接返回 model 结构体，避免暴露 `source_no`、`inviter_username` 等内部字段。列表用 `[]T{}` 初始化而非 nil，防止前端拿到 `null`。端到端验证（临时 sqlite + 真实 HTTP 服务）确认：四个接口均返回 `success:true`、`zhangsan → z******n`、`王小明同学 → 王***学`、`ab → **`、已冲正记录出现在明细但不计入累计收益、分页 `total` 不受当前页影响、管理员报表不脱敏、gin 无路由冲突（仓库已有 `/api/affinity` 组）。本文件同时是 `controller` 包的第一个测试。
- **关联计划**: `docs/superpowers/plans/2026-07-27-invite-commission-p4-api.md`

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
