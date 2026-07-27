# 更新记录 (CHANGELOG)

所有通过 Claude Code 辅助完成的代码变更必须记录在此文件中。

格式要求：每条记录包含日期、分支、变更类型、涉及文件和简要说明。

---

## 2026-07-27

### fix(topup): 统一充值金额换算口径并修复邮件金额 bug
- **分支**: `worktree-p1-foundation`
- **类型**: fix + 测试基础设施
- **涉及文件**: `model/testutil_test.go`、`model/quota_convert.go`、`model/quota_convert_test.go`、`model/charge_order.go`、`model/order.go`、`model/topup.go`
- **说明**: 为 model 包建立 in-memory sqlite 测试基座（此前该包零测试）。新增 `AmountToQuota` 作为金额→quota 的唯一换算入口，替换 `charge_order.go:200` 与 `order.go:82` 中硬编码的 `500000`——此前管理员修改后台 `QuotaPerUnit` 对这两条链路无效，与 `topup.go` 口径不一致。同时修复 `order.go:81` 用 `:=` 在事务闭包内遮蔽外层 `addAmount`，导致加密货币充值成功邮件金额恒为 $0 的问题。
- **关联计划**: `docs/superpowers/plans/2026-07-27-invite-commission-p1-foundation.md`

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
