# Responses 前置事件导致重试失效

## 背景与目标
用户提供 v0.1.42 日志并要求检查、修复：Azure 返回 HTTP 200，先发 response.created 再发流内 429，网关因提前提交下游响应而停止重试。OpenAI 与 Azure 共用此处理路径，属于原修复遗漏。

## 方案设计
- 在原生 Responses 流处理器内缓冲 response.created、response.in_progress、response.queued，最多 32 个事件、1 MiB 原始数据。超限返回 502，保留现有重试边界，不提前提交响应。
- 仅在首个非前置事件到达后记录待发送状态并按原顺序发送原始 JSON。前置阶段遇到错误或 EOF 丢弃缓冲，交由现有重试循环处理。
- Responses 的 ping 等待下游输出开始后再发送，其他协议不变。
- 客户端取消时明确停止 Responses 重试；输出开始、状态缓存失败、指定渠道与 provider/会话边界保持有效。
- 扩展现有路由及流错误测试，更新开始输出的旧测试夹具与真实联调预期。

## 影响范围
仅 Responses 流提交时机及重试取消保护，不修改数据库结构、价格、额度算法或 provider 选择策略。保留临时诊断日志。

## 验证方式
覆盖 OpenAI/Azure 前置事件后 429 换渠成功、完整请求重放、失败事件与状态不泄漏、原始 JSON 顺序、前置缓冲上限、EOF、已输出/取消不重试、ping 延迟与状态缓存失败。运行相关测试、全量 go test、go build、go vet 及 diff 检查。

## 验证结果
- `go test ./... -count=1`：通过，日志 `/tmp/linkinfra-prelude-tests.log`。
- `go test -race ./router ./relay/controller ./relay/helper ./controller -run 'TestResponses|TestRetryStays' -count=1`：通过，日志 `/tmp/linkinfra-prelude-race.log`。
- `go build ./...`、`go vet ./...`、`git diff --check`：通过。
- 初次 Azure 测试缺少显式 api_version，补全夹具配置后通过；生产版本/URL 构造逻辑未改。
- 未调用真实上游，未执行需显式环境变量的 Codex 联调用例，未发布镜像。

## 发布
用户后续明确授权“打tag 提交 触发镜像打包”。已 fetch 远端确认最新版本为 v0.1.42；更新 VERSION 至 v0.1.43，提交发布记录后原子推送 main 和注解 tag。沿用现有镜像工作流，检查 GitHub Actions 结果和镜像标签。此操作不直接重启生产服务。
