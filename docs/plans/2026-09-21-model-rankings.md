# 模型用量趋势与排行榜适配方案

日期：2026-09-21。状态：用户已确认每日方案，已实现，未部署或对现有数据库执行迁移。实际配置与发布步骤见 `../model-rankings.md`。

## 背景与目标

参考 OpenRouter Rankings 的 Top Models 趋势图、模型用量排行榜。第一版只实现这两项，优先控制数据库开销。

采用每天更新，展示最近完整 UTC 自然日的数据；页面明确显示数据截止日期。

实施细化：第一版节点每分钟直接加载一条持久快照到内存，不依赖 Redis；明细按小时执行带超时的 SQL 分组，避免拉取全部明细到应用内存，暂不增加 keyset 分页索引。未知模型的归一化 Token 保留但模型名为空，用于任务排除计数。租约集中保存在 ranking_states，日任务保存在 ranking_jobs，失败发布有持久化 dirty 标记。

### 已核实的代码现状

- `model/channel_provider.go`、`docs/channel-provider.md`：渠道 config.provider 是路由/状态兼容组，可细分到 Azure 资源，不能直接当模型作者。
- `model/log.go`：Log.Provider 已有字段，但消费日志入口没有赋值。ModelName 会还原为用户请求名；模型重定向信息在 Other 中。
- `middleware/distributor.go` 的 SetupContextForSelectedChannel、`relay/util/relay_meta.go`：选渠时已有实际渠道和配置，可以取得 provider 快照，无需额外查询渠道表。
- `model/model_metrics_aggregator.go`：默认每 300 秒重新统计当前小时；启动执行回填。日常调度只处理当前小时，跨小时之后不会主动补齐上个小时的尾部。
- `model/model_metrics.go`：现有聚合同时存 channel 明细和 channel_id=0 汇总。混合求和会重复计算。
- `main.go`：模型监控 worker 在每个启用该功能的实例启动，没有在此处选举唯一聚合实例。
- `common/config/config.go`：现有 ModelMetrics 默认只保留 30 天，不足以支持近 30 天与前 30 天比较。
- `relay/controller/claude.go`：原生 Claude 的 promptTokens 取 InputTokens，缓存读写另存 usageDetails；直接加日志 prompt/completion 会少计缓存。
- `relay/controller/gemini.go`：completionTokens 已包括 CandidatesTokenCount 和 ThoughtsTokenCount，不能再次追加 reasoning。
- `relay/controller/image.go`：部分图片消费日志的 completionTokens 保存图片张数，因此不能以 type=consume 作为唯一纳入条件。
- 实际前端在 `../linkinfra-web`，现有 `sections/model-plaza/components/metrics-chart.tsx` 使用 Recharts，可复用样式与 tooltip；面积图堆叠需补 stackId。

## 第一版产品范围

1. 趋势图：最近 30 个完整日，每天一个点；按整个时间窗总量选固定 Top 10，其余合并为 Others。补齐无调用日期，整个窗口保持模型颜色、序列身份一致。
2. 排行榜：近 1 日、近 7 日、近 30 日；默认周榜。显示排名、模型、作者、Token 总量、Token 份额、相对前一个等长窗口的变化率。
3. 同模型跨渠道/provider 合并为一条模型排名。底层保留 provider 维度，以便以后扩展来源筛选；第一版不额外开发 provider 独立榜。
4. 增长榜可后续增加，现有日数据足够计算，不需要另采明细。第一版无需任务分类、成本分析、性能评测或应用榜。

上一窗口为零时 change_percent=null，不计算无穷大；显示“上期无用量”，不能据此认定新模型。历史覆盖不足时隐藏涨跌幅，显示实际统计起点，不把未采集当作零。

## Provider 与模型身份

| 字段 | 含义 | 用途 |
| --- | --- | --- |
| routing_provider | 实际成功渠道当时的 config.provider | 路由隔离、内部用量分组；可沿用 logs.provider 存储 |
| ranking_model_name | 平台公开的标准模型标识 | 主榜分组、模型详情链接 |
| model_author | 模型作者，如 OpenAI、Anthropic | 展示标签，从模型目录/明确映射获取 |

同一个模型经 openai、azure-resource-a、azure-resource-b 服务时，公共主榜合并用量；provider 明细保留三条。

- 选渠/每次换渠时取得实际 provider，成功结算时持久化快照；不能拿首次重试边界值代替实际来源，首次 provider 为空时仍可能跨 provider 重试。
- provider 做 trim + lowercase，未配置写空值并在内部展示 unknown。后续修改或删除渠道不改写历史归属。
- 模型作者不能使用渠道协议类型推断：OpenAI 兼容渠道也能提供 Claude；现有 GetModelProvider 对此不够准确。
- 排名按实际服务的模型映射回公开标准名称，显式别名映射优先。不把 Azure 部署名、AWS ARN 或内部自定义别名直接发布到主榜，也不对日期/版本后缀随意截断。
- 第一版只纳入已确认的公开文本模型。无法识别的用量记入内部排除计数；公开总量与份额只基于纳入集合，避免分母不一致。

## Token 统计口径

只统计最终结算的文本请求。失败重试、预扣额度、退款、充值、图片张数和视频时长不进入榜单。最终有可记录用量的流式请求按结算用量计一次。

新增排名专用的归一化用量，不修改现有计费公式：

- OpenAI Chat/Responses：输入包含其内含的缓存 Token，输出包含其内含的 reasoning Token，明细项不重复追加。
- Claude 原生：输入总量 = 普通输入 + 缓存读取 + 缓存创建；缓存创建总字段与 5m/1h 拆分取一种可靠来源，不能双加。
- Gemini：输入 + 候选输出 + thinking，遵循现有适配器已经合并的字段，避免重复计算。
- 缺 usage 的请求沿用平台现有估算行为并标明榜单基于记录用量，不能宣称全部是上游实测。

建议在 logs 增加 nullable `ranking_model_name`、`ranking_tokens` 字段，并填充已有 provider。ranking_tokens 为 NULL 表示旧数据/不纳入，0 表示已归一化但无 Token，用来区分数据缺失与真实零值。仅最终文本结算入口填充这些字段；不可在通用日志入口无条件以 prompt+completion 自动填充。

## 推荐方案：按完整日增量汇总，发布静态快照

```text
实际成功渠道与归一化文本用量
             ↓ 随现有消费日志一起保存
日志库 logs
             ↓ 单 worker，每天只处理尚未完成的日期
ranking_daily（日 × 标准模型 × provider）
             ↓ 从最多 60 天日汇总计算
榜单快照（持久化 + Redis/各节点内存）
             ↓
趋势图 / 日周月排行榜
```

这是“按日期增量”，仍会在后台读取新增日志，不是零数据库工作。正常情况下一个日期只汇总一次，失败或数据修复时允许重算该日期；访问量不会增加明细扫描次数。

### 表与任务

- `ranking_daily`：day_start、model_name、provider、tokens(int64)、requests(int64)、updated_at。唯一键 `(day_start, model_name, provider)`；只存这一层，不混入渠道明细或全模型总计行。
- `ranking_jobs`：day_start、state、generation、lease_owner、lease_until、completed_at、error。唯一日期任务记录，支持空日也标记完成、失败重试和多实例租约。
- `ranking_snapshots`：一个固定榜单键，保存完整 JSON、version、data_through、coverage_start、generated_at。Redis 是可重建缓存，SQL 快照支持重启恢复。
- 日汇总至少保留 90 天；月榜环比需要完整 60 天。日志可以按原策略清理，但不得清理尚未汇总的日期，需与 DeleteOldLog 接口联动。

### 每天执行过程

1. 默认 UTC 00:15 开始处理前一天，预留日志提交缓冲。运行期间用数据库租约/条件更新保证只有一个有效 worker；提交检查 owner/generation，防止旧 worker 租约过期后覆盖新结果。
2. 只查目标日期 `[day_start, day_end)` 的合格日志，利用已有 created_at 索引。不扫描以前已完成日期，不每次回查最近 30 天。
3. 流量大时拆成 24 个小时区间，逐个限速读取；每段按 `(created_at, id)` keyset 分页、只取统计列，避免 OFFSET、一次加载整天日志、解析大段 Other。额外复合索引是否必要以 EXPLAIN 决定。
4. 先将该日期的分组结果累计到有界集合/任务 staging 中，完成后在事务中发布该日结果和完成状态。同一日期重跑使用完整替换或带 generation 的版本发布，不能重复 `tokens += 日总量`，也不能仅 upsert 而遗留已被修正消失的分组。
5. 从日汇总读取最多 60 天，在后台一次生成趋势图和三个榜单，一并原子发布，保证图表、列表、截止日期一致。
6. 保存 SQL 快照后更新 Redis，再由各实例定时加载至内存。Redis 失败仍可服务旧快照，显示实际截止时间；不在 HTTP 请求中回退扫描 logs。
7. worker 停机后按未完成日期逐日恢复，设置每轮预算，避免重启时突发全量回填。无数据也记录完成，不能用 MAX(日汇总日期) 判断是否漏日。

00:15 缓冲不能保证覆盖无限延迟的日志事务。实现必须检测已完成日期的迟到写入并将该日期标记待修复（由日志成功写入后的日期失效通知处理，需有持久化重试），或在上线运行约束下安排有界的日对账；超出范围支持指定日期重算。没有可靠失效通知时，不能宣称严格只处理一次且绝不漏记。首次实现可采用每天额外复核前一天已经发布的日期一次，明确超过该缓冲窗口的日志需定向修复。

### API 与缓存

建议单个公开接口 `GET /api/rankings` 返回固定快照，包含：

- `version`、`generated_at`、`data_through`、`coverage_start`、`timezone=UTC`；
- `leaderboards.day/week/month`：各模型 tokens、share、change_percent、comparison_complete；
- `trend`：日期数组、固定模型系列、Others。

第一版数据量小，三个榜单一起返回可减少切换请求。默认 Top 20/50，在已缓存数组上展开或分页；不开放任意时间 SQL 查询。支持 ETag 和短 HTTP/CDN 缓存。多节点缓存刷新只读 Redis/SQL 快照，聚合不由页面请求触发。冷启动无可用快照时返回“数据准备中”，不伪造零榜单。

### 成本估算（非实测）

假设每天 100 万条请求、200 个活跃模型/provider 组合：

- 每日只聚合该日约 100 万条明细；选用迟到复核则增加固定一天的读取，不随用户访问增加。
- 日汇总约 200 行；60 天约 12000 行，足够计算三个榜单与环比。
- 每个页面请求只序列化/返回已有快照；通常无 SQL 查询。
- 若日批任务影响业务数据库，进一步限速、迁移到日志只读副本（等待复制水位）或改用下述持久化事件方案，不靠缩短 HTTP 缓存 TTL 解决。

## 若需要每 5～10 分钟更新

保留同样的日汇总/快照/API，采集改为持久化增量事件：消费日志与排名事件在同一个 LOG_DB 事务写入；worker 按批领取未处理事件，在同一事务内累加日汇总并标记事件完成，失败整体回滚。事件具有唯一 ID，多实例不会重复累计，已处理事件按条件保留/清理。

不要只用内存计数后定时清空，也不要把 Redis 自增作为唯一账本；不要简单 `id > last_id` 后推进到 MAX(id)，并发事务的 ID 分配顺序不等于提交顺序，可能永久漏掉后提交的小 ID。

这个方案避免周期性扫明细，但每请求增加一条持久事件写入及后续清理；需要事件积压监控、幂等和事务测试。若只需要 OpenRouter 风格每日榜，第一版不承担这套额外机制。

## 影响范围与实施文件

- `middleware/distributor.go`、`relay/util/relay_meta.go`：实际 provider/模型快照传递，重试时更新，异步日志复制值而非读取可变 Gin context。
- `relay/controller/helper.go`、`claude.go`、`gemini.go`、`opeai_response.go` 等文本结算路径：生成统一排名用量。
- `model/log.go`：日志快照字段与落库；日志清理不得越过未完成统计日期。依赖 LogConsumeEnabled，关闭日志时榜单必须显式显示暂停/缺口，不能继续标记日期完整。
- 新增 `model/ranking.go`、`model/ranking_aggregator.go`、`model/ranking_cache.go`：日汇总、任务、快照；不依赖 ModelMetricsEnabled 开关。
- `model/main.go`、`main.go`、`common/config/config.go`：新增表/字段迁移定义、任务与配置。现有 ModelMetrics 保持监控用途，排名不上接当前小时重扫链路；其已有开销不会因新增榜单自动消失。
- 新增 `controller/ranking.go`，在 `router/api-router.go` 注册接口。
- `../linkinfra-web`：新增公开排名页、导航入口与 API 类型，复用图表组件并补堆叠支持。

需要新增日志字段和排名表，生产迁移单独提供可审核 SQL；本设计阶段不执行。排名字段缺失的历史日志不能按当前渠道配置伪造旧 provider。默认从上线后统计，首次展示覆盖范围；如要历史回填，单独限速解析旧日志，未知 provider 保留 unknown，并记录归一化版本与可用性。

## 验证方式

- Token 口径：OpenAI 缓存/reasoning 不双计；Claude 缓存完整；Gemini thinking 不双计；图片/视频/失败重试不进入文本榜。
- 归属：跨 provider 同模型合并；provider 改名和渠道删除不改变历史；模型别名、真实版本有明确测试。
- 聚合：日期边界、空日期、停机补跑、事务回滚、重复运行、租约过期、多实例竞争、迟到日期修复，不重复、不漏已持久化且符合口径的记录。
- 窗口：1/7/30 天与前期比较；历史不足不显示误导增长；Top10 固定，Others 与总量守恒。
- 缓存：Redis 故障、SQL 快照恢复、并发冷启动；榜单请求路径不得查询 logs。
- 性能：用代表性日志量测日批耗时、EXPLAIN、CPU/IO/连接占用；多次并发打开榜单不能增加日志查询数。
- 实现 Go 代码后按仓库要求运行 go build、go vet 与相关测试；前端运行类型/构建检查及桌面、移动端页面验证。
