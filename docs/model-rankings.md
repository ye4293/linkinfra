# 每日模型排行榜

## 页面与接口

前端 `/rankings`，公开接口 `GET /api/rankings`。包括最近 30 个完整 UTC 日的 Top 10 + Others 趋势，以及近 1/7/30 天模型用量榜。数据来源为本站最终文本消费日志，不是能力评测。

默认每天 UTC 00:15（北京时间 08:15）后汇总前一天。Worker 每分钟检查一次进度；多实例由日志库中的租约选出单个执行者。首次启用从下一个完整 UTC 日开始统计，第一份榜单在该日结束后发布，不自动扫旧日志回填。

同模型不同 provider 合并排名；日汇总保留实际成功渠道的 provider。未知模型作者/部署名不公开，`ranking_jobs.excluded_requests` 记录对应请求数。作者根据公开模型名识别，与路由 provider 无关。

## 开销与一致性

- 正常每天只处理新增一天日志；拆成 24 个带时间范围的分组查询，每个查询超时 2 分钟、默认间隔 100ms。通过既有 `logs.created_at` 索引访问，不在排名页面请求里执行查询。
- 最新已发布日次日再复核一次，然后标记 finalized。超出复核窗口的迟到数据可定向重算；不宣称支持无限迟到且永不漏记。
- 一天的结果和任务状态同事务提交；重复运行完整替换该日，失败回滚。租约含 owner/generation 和心跳，旧 worker 不能覆盖新结果。
- 日汇总保留 90 天；环比只读最多 60 天汇总数据。日志清理受同一租约保护，最多清理连续已完成复核的日期。应用外自行清理日志不受此保护。
- 关闭消费日志会暂停公开榜单，并重置完整统计区间；恢复后从新的完整日开始积累，避免把未采集日期当零用量。
- 快照持久化到日志库，节点每分钟读取一条快照到内存。HTTP 返回预先序列化的 JSON，支持 ETag、60 秒浏览器缓存和 300 秒共享缓存。第一版不依赖 Redis，避免再维护一套缓存一致性机制。
- 冷启动没有快照时返回 preparing；后台故障继续保留旧快照，页面显示真实截止日期和延迟提示。发布失败通过持久化 dirty 标志重试。
- 现有 ModelMetrics 监控仍独立运行；其当前小时重扫开销不会因新增榜单自动消失。

## 配置

```dotenv
RANKINGS_ENABLED=true
RANKING_BATCH_PAUSE_MS=100
RANKING_MODEL_ALIASES={"azure-deployment-a":"gpt-4.1","arn:aws:bedrock:example":"claude-sonnet-4-20250514"}
```

所有后端节点使用相同别名配置，修改后重启。别名目标应与模型广场公开名称一致；模型版本后缀不自动合并。别名变化只影响新日志中的快照，不改写旧数据。

`RANKINGS_ENABLED=false` 停止榜单发布与接口展示，文本日志仍会保存排名字段，便于之后恢复。`ModelMetricsEnabled` 与排行榜相互独立。

## 迁移与发布

1. 新版本包含 GORM 迁移定义：logs 新增 `ranking_model_name`、`ranking_tokens`，以及 `ranking_daily`、`ranking_states`、`ranking_jobs`、`ranking_snapshots` 四张表。未启动新服务就不会运行迁移。
2. 需要提前审核 SQL 时见 `scripts/migrations/2026-09-21-rankings.pg.sql`。执行目标是 LOG_SQL_DSN 指向的日志库；未拆库则为业务库。脚本未在本次开发中对现有数据库执行。
3. 默认 NODE_TYPE=master 启动会沿用项目既有 AutoMigrate 流程；非 master 节点需在表结构准备后启动。滚动升级应在首个采集完整日开始前完成，避免混合新旧节点导致统计缺失。
4. 前端部署 `../linkinfra-web` 的排名页及公开代理，沿用 `NEXT_PUBLIC_API_BASE_URL`。
5. `/api/rankings` 初次返回 `{"success":false,"status":"preparing","data":null}` 是预期行为；首个完整日后返回榜单。

## 指定日期重算

Root 身份调用 `POST /api/rankings/rebuild`，JSON 为 `{"date":"2026-09-20"}`。只标记任务，不在 HTTP 请求中扫日志。后台恢复时执行；最近一天仍按次日复核规则处理。已清理源日志、早于完整覆盖起点或超出保留期的日期拒绝重算。

生产流量下仍需观察每小时分组查询的 EXPLAIN 和实际 IO/耗时。此次验证使用隔离内存数据库和模拟页面数据，未进行生产规模压测。
