# Responses 429 临时诊断日志

## 背景与目标
用户明确要求在 linkinfra 增加可后续移除的日志，查明同 provider 存在备用渠道时，429 为什么没有继续跨渠道重试。本次只增加诊断，不修改重试边界或流转发行为；是否需要暂存前置事件，待实际日志确认。

## 方案设计
- `controller/relay.go`、`controller/retry_policy.go`：记录请求、每次尝试、剩余预算、重试决策和最终停止原因。
- `relay/controller/opeai_response.go`、`relay/helper/common.go`：区分 HTTP 错误和 SSE 错误，记录首次向客户端写入的事件或 ping，以及失败前是否已有内容事件。
- `model/cache.go`：使用请求级诊断标记，记录现有候选查询在 provider/状态渠道过滤前后的数量与有界候选摘要，不增加数据库查询。
- 扩展现有 Responses 完整路由测试，校验日志、真实请求次数和原有重试边界。更新变更记录。

## 影响范围
INFO 日志统一标记 `responses_retry_diag`。不打印密钥、请求/响应正文、密文历史或上游错误消息，不改变数据库结构、查询或计费。用户已明确授权上述临时诊断范围。

## 验证方式
运行相关路由回归、`go build ./...` 和 `go vet ./...`。检查 HTTP 429 能重试、前置事件/ping 后的 429 停止原因明确、同 provider 候选被正确统计。

## 验证与发布记录
- `go test ./... -count=1`：通过；完整路由覆盖 created/in_progress、ping、HTTP/SSE 429、provider 过滤、禁用渠道和大正文重试。
- `go build ./...`、`go vet ./...`、`git diff --check`：通过。
- `git fetch origin --tags` 后远端最新版本为 v0.1.41，本次使用 v0.1.42。用户明确授权推送 tag 触发既有多架构镜像工作流；线上根因仍需部署后复现确认。
