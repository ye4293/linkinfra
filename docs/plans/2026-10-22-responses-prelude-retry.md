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

## 2026-10-23 生产问题续修方案
用户要求同时解决历史状态绑定后的 429 无重试，以及客户端取消被记为上游 502。
- 429 日志已经进入 action=retry，但 state_channel=673 与 excluded_channels=[673] 冲突。仅对未输出且状态绑定的 429，等待 Retry-After / retry-after-ms（无有效提示时 2/4/8/16 秒退避，单次上限 30 秒）后重新选择原渠道。超过等待上限明确停止，不提前重试。仍检查渠道可用性、原 Key 索引及指纹，不放宽 provider 或资源边界，不重放已输出请求。
- Responses 流请求在等待上游头时也传播客户端取消；取消造成的未完成流使用 client_cancelled / 499 记录，不写失败 SSE 到已断开的连接，不计作渠道故障。真实未完成流仍为 502，完成事件优先保留成功。
- 参考 new-api relay/helper/stream_scanner.go 的清理错误抑制和客户端断开分类；不修改 new-api。
- 扩展既有完整路由测试，覆盖同渠道/同 Key/原正文重试、次数耗尽、禁用渠道和取消等待、取消前后无额外输出；保留前置 429 跨渠道成功等回归。运行完整测试、相关 race、build、vet。
- 无数据库结构、计费算法或价格调整。当前用户已明确要求实施这两项修复。

### 续修验证结果
- 全量 `go test ./... -count=1 -timeout 5m` 通过，日志 `/tmp/linkinfra-state-retry-all.log`。
- `go test -race ./router ./relay/controller ./relay/helper ./controller -run 'TestResponses|TestRetryStays' -count=1 -timeout 5m` 通过，日志 `/tmp/linkinfra-state-retry-race.log`。relay/helper 无匹配测试，扫描器由完整路由回归实际调用。
- `go build ./...`、`go vet ./...`、`git diff --check` 通过。
- 回归覆盖 HTTP/SSE 429 原渠道与非零 Key 索引重试成功、未配置 provider 的加密历史、完整大正文及一次结算、重试次数耗尽、渠道禁用、Key 指纹变化、超长 Retry-After 停止、退避期间取消，以及响应头/前置/已输出三个阶段的取消和不误禁用渠道。
- 首轮新增测试重复使用成功 response id，触发状态缓存来源污染并使后台写日志撞上测试清理；改为按用例独立 id。等待响应头的模拟上游未消费请求体，无法感知连接关闭；修正测试服务端先读请求体。修正后完成上述全部验证。
- 官方协议参考：https://developers.openai.com/api/docs/guides/conversation-state 。文档说明服务端 response 引用与客户端完整重放历史的区别，没有提供任意 Azure 资源可共享历史的保证；本次不放宽资源边界。
- 生产根因来自用户提供的日志，修复验证使用本地模拟上游；尚未发布镜像或做生产验收。
